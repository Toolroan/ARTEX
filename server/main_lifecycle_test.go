package server

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func mainLifecycleFixtureMessage(t *testing.T, task *Task, key string, tools []string) *db.MainMessage {
	t.Helper()
	m, _, err := task.Store.CreateMainMessage(t.Context(), db.MainMessageInput{RequestKey: key, Mode: "auto", InputSource: "main", RawText: "Continue the synthetic task", OperatorText: "Continue the synthetic task"})
	if err != nil {
		t.Fatal(err)
	}
	m, err = task.Store.ClaimMainRouting(t.Context(), m.ID)
	if err != nil || m == nil {
		t.Fatalf("claim: %v", err)
	}
	d, _ := json.Marshal(agent.MainMessageRoute{Kind: "control", ControlTools: tools})
	m, err = task.Store.FinishMainRouting(t.Context(), m.ID, m.RoutingToken, db.MainRouteDecision{Route: "control", Owner: "main", DecisionData: d})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMainLifecycleControlUsesDurableOwnershipOnce(t *testing.T) {
	for _, tool := range []string{"wake_planner", "continue_task"} {
		t.Run(tool, func(t *testing.T) {
			s, task := plannerBridgeFixture(t)
			status, action := "running", "wake"
			if tool == "continue_task" {
				status, action = "timeout", "restart"
			}
			if err := s.m.SetTaskStatus(task.ID, status); err != nil {
				t.Fatal(err)
			}
			m := mainLifecycleFixtureMessage(t, task, "owned-control", []string{tool})
			m, err := task.Store.ClaimMainDelivery(t.Context(), m.ID)
			if err != nil || m == nil {
				t.Fatalf("claim: %v", err)
			}
			policy := s.mainMutationPolicy(task, m)
			calls := 0
			run := func() (actool.Result, error) {
				calls++
				result, err := s.applyTaskLifecycleControlLocked(task, action)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				data, _ := json.Marshal(result)
				return actool.Text(string(data)), nil
			}
			result, err := policy.Execute(t.Context(), tool, json.RawMessage(`{}`), run)
			if err != nil || result.IsError || calls != 1 {
				t.Fatalf("control failed: %v %+v calls=%d", err, result, calls)
			}
			if again, _ := policy.Execute(t.Context(), tool, json.RawMessage(`{}`), run); !again.IsError || calls != 1 {
				t.Fatal("one message executed lifecycle twice")
			}
			if err := task.Store.FinishMainDelivery(t.Context(), m.ID, m.DeliveryToken, true); err != nil {
				t.Fatal(err)
			}
			if stale, _ := s.mainMutationPolicy(task, m).Execute(t.Context(), tool, json.RawMessage(`{}`), run); !stale.IsError || calls != 1 {
				t.Fatal("stale delivery executed lifecycle")
			}
		})
	}
}

func TestMainLifecycleGateDoesNotReleaseOrdinaryMutations(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	if err := s.m.SetTaskStatus(task.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	s.engine.settling.Store(task.ID, true)
	for i, tools := range [][]string{{"continue_task"}, {"set_goals"}, {"continue_task", "set_goals"}} {
		m := mainLifecycleFixtureMessage(t, task, string(rune('a'+i)), tools)
		waiting := s.mainMessageWaitingReason(task, m)
		if (waiting == "") != (i == 0) {
			t.Fatalf("wrong lifecycle gate tools=%v reason=%q", tools, waiting)
		}
	}
}

func TestMainContinueToolActuallyRestartsAndPreservesPlannerInbox(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	if err := s.m.SetTaskStatus(task.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	s.engine.settling.Store(task.ID, true)
	pending := statusFixtureMessage(t, task, "existing-instruction", "action", "planner", 0)
	s.taskAgents = map[string]*taskAgentBundle{}
	s.provByProfile = map[int64]*provEntry{}
	s.llmOn = true
	s.llmCfg = agent.Config{Format: llm.FormatOpenAI, Model: "fixture", Stream: false}
	var mu sync.Mutex
	mainCalls := 0
	s.llmProv = retestProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(req.Tools) == 0 {
			route := `{"kind":"control","summary":"Continue the existing synthetic task","context_ids":[],"proposal_id":0,"requires_context":false,"clarification":"","control_tools":["continue_task"],"one_off":false}`
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(route)}}, "end_turn", llm.Usage{}, nil
		}
		mainCalls++
		found := false
		for _, tool := range req.Tools {
			if tool.Name == "continue_task" {
				found = true
			}
			if tool.Name == "add_intent" || tool.Name == "set_goals" || tool.Name == "wake_planner" {
				t.Errorf("unassigned tool exposed: %s", tool.Name)
			}
		}
		if !found {
			t.Error("explicit continuation was not available to Main")
		}
		if mainCalls == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "continue", Name: "continue_task", Input: json.RawMessage(`{}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("The synthetic task was continued; worker application remains unverified.")}}, "end_turn", llm.Usage{}, nil
	}}
	w := plannerBridgeRequest(s, task, false, "Continue this ended synthetic task")
	if w.Code != 202 {
		t.Fatalf("request=%d %s", w.Code, w.Body)
	}
	waitMainFixture(t, s, task)
	if state := task.lifecycleSnapshot(); state.Status != "running" || state.Paused {
		t.Fatalf("not continued: %+v", state)
	}
	if s.engine.isSettling(task.ID) {
		t.Fatal("old timeout gate survived continuation")
	}
	stored, err := task.Store.GetMainMessage(t.Context(), pending.ID)
	if err != nil || stored.DeliveryStatus != "queued" {
		t.Fatalf("existing inbox changed: %+v %v", stored, err)
	}
	var count int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM main_message_routes WHERE exploration_id=$1 AND delivery_owner='planner'`, task.ExpID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate Planner request: %d %v", count, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if mainCalls != 2 {
		t.Fatalf("Main calls=%d", mainCalls)
	}
}

func TestTerminalPlannerReceiptsRequireExplicitContinuation(t *testing.T) {
	for _, status := range []string{"timeout", "failed", "done"} {
		t.Run(status, func(t *testing.T) {
			s, task := plannerBridgeFixture(t)
			if _, err := s.m.pg.Exec(`UPDATE exploration_nodes SET state='met' WHERE exploration_id=$1 AND kind='goal'`, task.ExpID); err != nil {
				t.Fatal(err)
			}
			if err := s.m.SetTaskStatus(task.ID, status); err != nil {
				t.Fatal(err)
			}
			m := statusFixtureMessage(t, task, "terminal-status", "action", "planner", 0)
			r := s.messageReceipt(task, m)
			if r.Delivery != "blocked" || r.TaskStatus != status || r.RequiredAction != "restart" || strings.Contains(r.Reason, "waiting for an eligible") {
				t.Fatalf("misleading receipt: %+v", r)
			}
			page, err := s.instructionStatusReader(task, 0, m.ID)(t.Context(), nil)
			if err != nil || len(page.Items) != 1 || page.Items[0].Delivery != "blocked" || page.Items[0].RequiredAction != "restart" {
				t.Fatalf("bad live status: %+v %v", page, err)
			}
			m.DeliveryStatus = "presented"
			if got := s.messageReceipt(task, m); got.Delivery != "presented" || got.RequiredAction != "" {
				t.Fatalf("reviewed instruction was requeued: %+v", got)
			}
			m.DeliveryStatus = "inflight"
			expired := time.Now().Add(-time.Second)
			m.DeliveryUntil = &expired
			if got := s.messageReceipt(task, m); got.Delivery != "needs_review" {
				t.Fatalf("uncertain delivery became replayable: %+v", got)
			}
		})
	}
}

func TestPlannerQueuedAdmissionIsNotUserPause(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	m := statusFixtureMessage(t, task, "queued-admission", "action", "planner", 0)
	task.updateLifecycle(func(state *taskLifecycleState) { state.Queued = true })
	s.engine.Pause(task.ID, agent.AbortPausedByUser) // admission uses the same execution barrier
	r := s.messageReceipt(task, m)
	if r.Delivery != "waiting" || r.RequiredAction != "" || !strings.Contains(r.Reason, "queued") {
		t.Fatalf("admission queue mislabeled as user pause: %+v", r)
	}
	page, err := s.instructionStatusReader(task, 0, m.ID)(t.Context(), nil)
	if err != nil || page.TaskPaused || !page.TaskQueued || page.Items[0].RequiredAction != "" || !strings.Contains(page.Items[0].Reason, "queued") {
		t.Fatalf("queued status requested user resume: %+v %v", page, err)
	}
}
