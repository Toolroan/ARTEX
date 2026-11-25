package server

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func steeringEngine(t *testing.T) *Engine {
	t.Helper()
	e := NewEngine(nil)
	_, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	e.registerWork(42, cancel)
	return e
}

func TestWorkerSteeringBlocksAllStaleToolsUntilModelRequest(t *testing.T) {
	e := steeringEngine(t)
	if err := e.SteerWork(42, "Use the corrected target"); err != nil {
		t.Fatal(err)
	}
	h := steerHooks{drain: func() (string, bool) { return e.pendingSteer(42) }}
	// Every sibling in a parallel response must be blocked; checking a tool
	// must not consume the correction for the other siblings.
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if blocked, text, _ := h.PreToolUse(context.Background(), "Bash", nil); !blocked || !strings.Contains(text, "corrected target") {
				t.Errorf("stale tool escaped steering: blocked=%v, text=%q", blocked, text)
			}
		})
	}
	wg.Wait()
	original := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("original")}}
	request := e.workerSteeringRequest(42, original)
	if len(original.Messages) != 1 || len(request.Messages) != 2 || !strings.Contains(request.Messages[1].Text(), "corrected target") {
		t.Fatal("correction was not isolated and included in model request")
	}
	if blocked, _, _ := h.PreToolUse(context.Background(), "Bash", nil); blocked {
		t.Fatal("new plan remains blocked")
	}
	if err := e.SteerWork(42, "Second correction"); err != nil {
		t.Fatal(err)
	}
	if blocked, text, _ := h.PreToolUse(context.Background(), "Bash", nil); !blocked || !strings.Contains(text, "Second correction") {
		t.Fatal("new correction did not block stale response")
	}
	request = e.workerSteeringRequest(42, original)
	if text := request.Messages[1].Text(); !strings.Contains(text, "corrected target") || !strings.Contains(text, "Second correction") {
		t.Fatal("standing corrections lost across requests")
	}
}

func TestWorkerSteeringFinalTextAndGuards(t *testing.T) {
	e := steeringEngine(t)
	if err := e.SteerWork(42, "Check the correction before finishing"); err != nil {
		t.Fatal(err)
	}
	h := steerHooks{drain: func() (string, bool) { return e.pendingSteer(42) }}
	final := []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}}
	if prevent, blocking, _ := h.Stop(context.Background(), final); prevent || len(blocking) != 1 {
		t.Fatal("text-only completion discarded correction")
	}
	if _, blocking, _ := h.Stop(runCtxAfter(t, "refusal"), final); len(blocking) != 0 {
		t.Fatal("correction retried a provider refusal")
	}
	h.inner = fakeHooks{prevent: true, msg: "scope guard"}
	if prevent, blocking, message := h.Stop(context.Background(), final); !prevent || len(blocking) != 0 || message != "scope guard" {
		t.Fatal("correction overrode hard stop")
	}
	h.inner = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, blocking, _ := h.Stop(ctx, final); len(blocking) != 0 {
		t.Fatal("cancelled worker requested continuation")
	}
}

func TestWorkerSteeringCompletionAdmissionIsAtomic(t *testing.T) {
	for range 100 {
		e := steeringEngine(t)
		start := make(chan struct{})
		result := make(chan error, 1)
		go func() { <-start; result <- e.SteerWork(42, "late correction") }()
		close(start)
		pending, again := e.finishWorkerSteering(42, true)
		err := <-result
		if err == nil && (!again || pending != "late correction") {
			t.Fatal("accepted correction disappeared across completion")
		}
		if err != nil && (again || pending != "") {
			t.Fatal("rejected correction scheduled continuation")
		}
	}
}

func TestWorkerSteeringBoundedStopRetainsUnapplied(t *testing.T) {
	e := steeringEngine(t)
	if err := e.SteerWork(42, "late correction"); err != nil {
		t.Fatal(err)
	}
	if pending, again := e.finishWorkerSteering(42, false); again || pending != "late correction" {
		t.Fatal("bounded stop lost pending correction or continued")
	}
	if err := e.SteerWork(42, "too late"); err == nil {
		t.Fatal("finishing worker accepted a new correction")
	}
	if pending, _ := e.finishWorkerSteering(42, false); pending != "late correction" {
		t.Fatal("failure report cannot recover unapplied correction")
	}
}

type approvalWaitHooks struct {
	fakeHooks
	entered chan struct{}
	release chan struct{}
	blocked bool
}

func (h approvalWaitHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	close(h.entered)
	<-h.release
	return h.blocked, "guard decision", []byte(`{"approved":true}`)
}

func TestWorkerSteeringRechecksAfterApprovalWait(t *testing.T) {
	for _, tc := range []struct {
		name    string
		correct bool
		blocked bool
	}{
		{"correction during approval", true, false},
		{"preserve approved rewrite", false, false},
		{"preserve guard block", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := steeringEngine(t)
			inner := approvalWaitHooks{entered: make(chan struct{}), release: make(chan struct{}), blocked: tc.blocked}
			hooks := steerHooks{inner: inner, drain: func() (string, bool) { return e.pendingSteer(42) }}
			type outcome struct {
				blocked bool
				message string
				updated []byte
			}
			result := make(chan outcome, 1)
			go func() {
				blocked, message, updated := hooks.PreToolUse(context.Background(), "Bash", []byte(`{}`))
				result <- outcome{blocked, message, updated}
			}()
			<-inner.entered
			if tc.correct {
				if err := e.SteerWork(42, "correction during approval"); err != nil {
					t.Fatal(err)
				}
			}
			close(inner.release)
			got := <-result
			if tc.correct && !tc.blocked {
				if !got.blocked || !strings.Contains(got.message, "correction during approval") {
					t.Fatal("stale tool escaped after approval wait")
				}
			} else if got.blocked != tc.blocked || got.message != "guard decision" || string(got.updated) != `{"approved":true}` {
				t.Fatalf("guard decision or rewritten input changed: %+v", got)
			}
		})
	}
}

func TestUnappliedSteeringIsDurableAndUrgent(t *testing.T) {
	skipWithoutDSN(t)
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	created, err := m.CreateTaskWithOptions("steering test", "synthetic goal", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(created.ID, DeleteTaskOptions{}) })
	task, _ := m.Task(created.ID)
	e := NewEngine(m)
	intentID, err := task.Store.AddIntent(map[string]any{"summary": "synthetic work"}, 0, nil, "seed")
	if err != nil {
		t.Fatal(err)
	}
	e.reportUnappliedSteering(task, intentID, "synthetic correction")
	hints, err := task.Store.ListByKind(db.KindHint, 50)
	if err != nil || len(hints) != 1 {
		t.Fatalf("durable hints: %v, %v", hints, err)
	}
	if hints[0].Origin == "human" || !strings.Contains(string(hints[0].Payload), "synthetic correction") {
		t.Fatal("failure lost or mislabeled human")
	}
	batch, skipped := takePlannerTriggers(task, true)
	if skipped || len(batch) != 1 || batch[0].Kind != "steering_unapplied" {
		t.Fatalf("failure was quiet-suppressed: %#v", batch)
	}
}
