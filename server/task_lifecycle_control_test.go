package server

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

type lifecycleReviewProvider struct {
	calls       atomic.Int64
	instruction atomic.Bool
	entered     chan struct{}
	release     chan struct{}
}

func (p *lifecycleReviewProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls.Add(1)
	p.instruction.Store(strings.Contains(strings.Join(req.System, "\n"), "instruction-only coordination round"))
	select {
	case p.entered <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return llm.Message{}, "", llm.Usage{}, ctx.Err()
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("Reviewed the explicit fixture request. No worker action was requested.")}}, "end_turn", llm.Usage{}, nil
}

func (p *lifecycleReviewProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		msg, stop, _, err := p.Complete(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		for _, event := range []llm.StreamEvent{
			{Type: llm.SEMessageStart},
			{Type: llm.SETextDelta, Text: msg.Text()},
			{Type: llm.SEMessageDelta, StopReason: stop},
			{Type: llm.SEMessageStop},
		} {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func lifecycleControlFixture(t *testing.T, timeout int) (*Server, *Task) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		db.SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { _ = m.Close() })
	t.Cleanup(restoreConcurrencySetting(t, m))
	if err := m.SetConcurrency(false, defaultConcurrencyLimit); err != nil {
		t.Fatal(err)
	}
	task, err := m.CreateTask("Lifecycle fixture", "Review existing synthetic data only", nil, timeout, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	return newAdmissionTestServer(m, nil), task
}

func TestExplicitRestartReviewsGoallessPendingInboxWithoutImplicitSuccess(t *testing.T) {
	s, task := lifecycleControlFixture(t, 120)
	if _, err := s.m.StampTaskFirstRun(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.m.SetTaskStatus(task.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	msg := statusFixtureMessage(t, task, "restart-fixture", "correction", "planner", 0)
	p := &lifecycleReviewProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	planner := agent.NewPlanner(p, "fixture", t.TempDir(), nil, 0, 2)
	s.engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) {
		return planner, new(agent.Worker)
	})
	s.engine.debounce = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	s.ctx = ctx
	s.engine.started.Store(task.ID, true)
	s.engine.settling.Store(task.ID, true)
	s.engine.deadline.Store(task.ID, task.lifecycleSnapshot().DeadlineAt)
	s.engine.stamped.Store(task.ID, true)
	s.engine.coordStarted.Store(task.ID, true)
	loopDone := make(chan struct{})
	go func() { defer close(loopDone); s.engine.plannerLoop(ctx, task) }()
	t.Cleanup(func() {
		cancel()
		<-loopDone
		wait, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if err := s.engine.Wait(wait); err != nil {
			t.Errorf("engine did not stop: %v", err)
		}
	})
	// A saved instruction and an ordinary wake must preserve the terminal gate.
	task.Notify()
	time.Sleep(30 * time.Millisecond)
	if p.calls.Load() != 0 || task.lifecycleSnapshot().Status != "timeout" {
		t.Fatal("ordinary notification resumed a timed-out task")
	}
	if out, err := s.applyTaskControl(task, "wake"); err == nil || out.WakeSent {
		t.Fatalf("wake revived terminal task: %+v %v", out, err)
	}
	out, err := s.applyTaskControl(task, "restart")
	if err != nil || out.Status != "running" || out.Queued {
		t.Fatalf("restart: %+v %v", out, err)
	}
	select {
	case <-p.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("explicit restart did not review pending goalless inbox")
	}
	clock := task.lifecycleSnapshot()
	if clock.FirstRunAt == 0 || clock.DeadlineAt <= time.Now().Unix() || s.engine.isSettling(task.ID) {
		t.Fatalf("timeout clock/settling not renewed: %+v", clock)
	}
	for range 3 {
		out, err := s.applyTaskControl(task, "restart")
		if err != nil || !out.AlreadyActive || out.WakeSent || task.lifecycleSnapshot().DeadlineAt != clock.DeadlineAt {
			t.Fatalf("repeated restart changed active run: %+v %v", out, err)
		}
	}
	close(p.release)
	deadline := time.Now().Add(3 * time.Second)
	var stored *db.MainMessage
	var request *db.WorkRequest
	for time.Now().Before(deadline) {
		stored, err = task.Store.GetMainMessage(t.Context(), msg.ID)
		request, _ = task.Store.GetWorkRequest(t.Context(), msg.ID)
		if err == nil && stored.DeliveryStatus == "presented" && request != nil && request.State == "blocked" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || stored.DeliveryStatus != "presented" || request == nil || request.State != "blocked" || task.lifecycleSnapshot().Status != "running" || p.calls.Load() != 1 || !p.instruction.Load() {
		t.Fatalf("delivery must not imply outcome completion: delivery=%+v request=%+v state=%+v calls=%d instruction=%t err=%v", stored, request, task.lifecycleSnapshot(), p.calls.Load(), p.instruction.Load(), err)
	}
	if intents, err := task.Store.ListByKind(db.KindIntent, 10); err != nil || len(intents) != 0 {
		t.Fatalf("restart created work without a request: %v %v", intents, err)
	}
	// An ordinary notification does not repeat a blocked request indefinitely.
	// The explicit Review request / wake controls can ask for another review.
	task.Notify()
	time.Sleep(50 * time.Millisecond)
	if p.calls.Load() != 1 || task.lifecycleSnapshot().Status != "running" {
		t.Fatal("ordinary notification replayed a blocked request or completed it")
	}
}

func TestRestartHonorsFIFOAndDoesNotResetPriorIntents(t *testing.T) {
	s, task := lifecycleControlFixture(t, 120)
	if err := s.m.SetConcurrency(true, 1); err != nil {
		t.Fatal(err)
	}
	holder, err := s.m.CreateTask("Slot holder", "Synthetic fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.DeleteTask(holder.ID, DeleteTaskOptions{}) })
	iid, err := task.Store.AddIntent(map[string]any{"summary": "Old synthetic work"}, 1, nil, "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := task.Store.CompareAndSetIntentState(iid, "open", "stopped"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.StampTaskFirstRun(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.m.SetTaskStatus(task.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	out, err := s.applyTaskControl(task, "restart")
	state := task.lifecycleSnapshot()
	if err != nil || !out.Queued || out.Status != "queued" || !s.engine.IsPaused(task.ID) || state.FirstRunAt != 0 || state.DeadlineAt != 0 {
		t.Fatalf("restart bypassed queue/reset: %+v state=%+v err=%v", out, state, err)
	}
	for range 3 {
		repeat, err := s.applyTaskControl(task, "restart")
		if err != nil || !repeat.AlreadyActive || !repeat.Queued || task.lifecycleSnapshot().QueuedAt != state.QueuedAt {
			t.Fatalf("retry changed queue position: %+v %v", repeat, err)
		}
	}
	if out, err := s.applyTaskControl(task, "wake"); err == nil || out.WakeSent {
		t.Fatalf("wake bypassed queue: %+v %v", out, err)
	}
	node, err := task.Store.GetNode(iid)
	if err != nil || node.State != "stopped" {
		t.Fatalf("restart reset old intent: %+v %v", node, err)
	}
}

func TestLifecycleControlPreservesPauseDeleteSettlementAndProviderGates(t *testing.T) {
	for _, gate := range []string{"paused", "deleting", "settling", "provider", "not_started"} {
		t.Run(gate, func(t *testing.T) {
			s, task := lifecycleControlFixture(t, 0)
			s.engine.started.Store(task.ID, true)
			switch gate {
			case "paused":
				if err := s.m.ApplyTaskPause(task.ID); err != nil {
					t.Fatal(err)
				}
				s.engine.Pause(task.ID, agent.AbortPausedByUser)
			case "deleting":
				s.engine.BeginDelete(task.ID)
				t.Cleanup(func() { s.engine.AbortDelete(task.ID, false) })
			case "settling":
				s.engine.settling.Store(task.ID, true)
			case "provider":
				s.engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return nil, nil })
			case "not_started":
				s.engine.started.Delete(task.ID)
			}
			before := task.lifecycleSnapshot()
			out, err := s.applyTaskControl(task, "wake")
			if err == nil || out.BlockedReason == "" || out.WakeSent {
				t.Fatalf("wake bypassed %s: %+v %v", gate, out, err)
			}
			out, err = s.applyTaskControl(task, "restart")
			if gate == "provider" || gate == "not_started" {
				if err != nil || !out.AlreadyActive || out.BlockedReason == "" {
					t.Fatalf("active blocked receipt: %+v %v", out, err)
				}
			} else if err == nil {
				t.Fatalf("restart bypassed %s: %+v", gate, out)
			}
			if after := task.lifecycleSnapshot(); !reflect.DeepEqual(after, before) {
				t.Fatalf("blocked control changed lifecycle: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestRestartUnavailableProviderReportsPending(t *testing.T) {
	for _, limited := range []bool{false, true} {
		s, task := lifecycleControlFixture(t, 0)
		if err := s.m.SetConcurrency(limited, 1); err != nil {
			t.Fatal(err)
		}
		s.engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return nil, nil })
		if err := s.m.SetTaskStatus(task.ID, "done"); err != nil {
			t.Fatal(err)
		}
		out, err := s.applyTaskControl(task, "restart")
		if err != nil || out.Queued != limited || out.BlockedReason == "" || out.WakeSent {
			t.Fatalf("missing provider misreported: %+v %v", out, err)
		}
	}
}

func TestExplicitRestartClearsPriorPauseOnlyAfterTaskEnded(t *testing.T) {
	for _, status := range []string{"done", "failed", "timeout"} {
		t.Run(status, func(t *testing.T) {
			s, task := lifecycleControlFixture(t, 120)
			if err := s.m.ApplyTaskPause(task.ID); err != nil {
				t.Fatal(err)
			}
			s.engine.Pause(task.ID, agent.AbortPausedByUser)
			// Model a pre-existing terminal+paused record. New success transitions
			// cannot create this combination because the completion guard rejects pause.
			if err := s.m.pg.SetStatus(mustTaskID(t, task.ID), status); err != nil {
				t.Fatal(err)
			}
			task.updateLifecycle(func(state *taskLifecycleState) { state.Status = status })
			before := task.lifecycleSnapshot()
			if !before.Paused || before.Status != status {
				t.Fatalf("fixture lacks terminal/pause combination: %+v", before)
			}
			if out, err := s.applyTaskControl(task, "wake"); err == nil || out.WakeSent || out.Status != status {
				t.Fatalf("ordinary wake continued ended/paused task: %+v %v", out, err)
			}
			if !reflect.DeepEqual(task.lifecycleSnapshot(), before) {
				t.Fatal("wake changed ended/paused lifecycle")
			}
			out, err := s.applyTaskControl(task, "restart")
			state := task.lifecycleSnapshot()
			if err != nil || out.Paused || out.Queued || out.Status != "running" || state.Paused || state.Status != "running" || s.engine.IsPaused(task.ID) {
				t.Fatalf("explicit continuation retained terminal pause: %+v state=%+v err=%v", out, state, err)
			}
			stored, err := s.m.pg.GetTask(mustTaskID(t, task.ID))
			if err != nil || stored.Paused || stored.Status != "running" {
				t.Fatalf("continuation did not atomically clear persisted pause: %+v %v", stored, err)
			}
		})
	}
}

func TestRestartPreservesRecordedBudgetAndReportsBlock(t *testing.T) {
	for _, key := range []string{"ARTEX_TASK_BUDGET_TURNS", "ARTEX_INTENT_BUDGET_TOKENS", "ARTEX_INTENT_BUDGET_TURNS"} {
		t.Setenv(key, "")
	}
	t.Setenv("ARTEX_TASK_BUDGET_TOKENS", "100000")
	s, task := lifecycleControlFixture(t, 0)
	if err := s.m.pg.InsertLLMUsage(&db.LLMUsage{TaskID: task.ID, Worker: "planner", InputTokens: 80000, Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := s.m.SetTaskStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	out, err := s.applyTaskControl(task, "restart")
	if err != nil || !strings.Contains(out.BlockedReason, "budget") {
		t.Fatalf("exhausted budget not reported: %+v %v", out, err)
	}
	if budget := s.engine.budgetFor(task.ID).State(); budget.Spent.Tokens != 80000 || budget.Dispatch {
		t.Fatalf("restart reset recorded task spend: %+v", budget)
	}
	if out, err := s.applyTaskControl(task, "wake"); err == nil || out.WakeSent || !strings.Contains(out.BlockedReason, "budget") {
		t.Fatalf("wake bypassed budget: %+v %v", out, err)
	}
}

func TestRestartPoolClaimsWaitForSavedPlannerReview(t *testing.T) {
	s, task := lifecycleControlFixture(t, 120)
	iid, err := task.Store.AddIntent(map[string]any{"summary": "Old open synthetic work"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.m.SetTaskStatus(task.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	statusFixtureMessage(t, task, "pending-before-restart", "correction", "planner", 0)
	if _, err := s.applyTaskControl(task, "restart"); err != nil {
		t.Fatal(err)
	}
	if node, _ := s.engine.claimNext(task, "pool"); node != nil {
		t.Fatal("continued worker pool claimed old work before queued correction review")
	}
	batch, err := task.Store.ClaimPlannerMessages(t.Context(), 8)
	if err != nil || batch == nil {
		t.Fatalf("claim inbox: %v %v", batch, err)
	}
	if node, _ := s.engine.claimNext(task, "pool"); node != nil {
		t.Fatal("continued worker pool claimed old work during correction review")
	}
	if err := task.Store.FinishPlannerMessages(t.Context(), batch.Token, true); err != nil {
		t.Fatal(err)
	}
	node, token := s.engine.claimNext(task, "pool")
	if node == nil || node.ID != iid || token == "" {
		t.Fatalf("reviewed inbox did not release the preserved frontier: %+v %q", node, token)
	}
}

func TestLifecycleControlHTTPAuthenticationAndBlockedReceipt(t *testing.T) {
	s, _ := newReadyServer(t, func(context.Context) error { return nil })
	for _, action := range []string{"wake", "restart"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/tasks/1/control", strings.NewReader(`{"action":"`+action+`"}`)))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s=%d", action, w.Code)
		}
	}
	s, task := lifecycleControlFixture(t, 0)
	if err := s.m.SetTaskStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID+"/control", strings.NewReader(`{"action":"wake"}`))
	r.SetPathValue("id", task.ID)
	w := httptest.NewRecorder()
	s.control(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"blocked_reason"`) || !strings.Contains(w.Body.String(), `"status":"done"`) {
		t.Fatalf("blocked HTTP receipt=%d %s", w.Code, w.Body)
	}
}
