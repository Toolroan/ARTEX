package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestPlannerInstructionRouteRequiresAuthentication(t *testing.T) {
	s, _ := newReadyServer(t, func(context.Context) error { return nil })
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/tasks/1/planner-instructions", strings.NewReader(`{"text":"fixture"}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated instruction status=%d", w.Code)
	}
}

func TestPlannerInstructionTextLimit(t *testing.T) {
	for _, invalid := range []string{"", " \n ", strings.Repeat("界", maxPlannerInstructionChars+1)} {
		if _, err := validatePlannerInstruction(invalid); err == nil {
			t.Fatal("invalid instruction accepted")
		}
	}
	if _, err := validatePlannerInstruction(strings.Repeat("界", maxPlannerInstructionChars)); err != nil {
		t.Fatal(err)
	}
}

func TestPlannerInstructionAttachmentReferencesConfined(t *testing.T) {
	mainDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(mainDir, "uploads"), 0700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(mainDir, "uploads", "fixture.txt")
	if err := os.WriteFile(inside, []byte("FILE CONTENT MUST NOT BECOME AN OPERATOR INSTRUCTION"), 0600); err != nil {
		t.Fatal(err)
	}
	text, err := mainPlannerInstruction("Review the attached fixture", []chatAttachment{{Path: "uploads/fixture.txt", Abs: "/untrusted/client/value"}}, mainDir)
	if err != nil || !strings.Contains(text, inside) || strings.Contains(text, "FILE CONTENT") || strings.Contains(text, "/untrusted/client/value") {
		t.Fatalf("attachment reference=%q err=%v", text, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(mainDir, "uploads", "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainDir, "not-uploaded.txt"), []byte("main working file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../not-uploaded.txt", filepath.Join(mainDir, "uploads", "not-uploaded.txt")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, "../outside.txt", "uploads/../../outside.txt", "uploads/escape.txt", "uploads/not-uploaded.txt", "uploads/missing.txt", "uploads"} {
		if _, err := mainPlannerInstruction("fixture", []chatAttachment{{Path: path}}, mainDir); err == nil {
			t.Errorf("accepted invalid attachment %q", path)
		}
	}
}

func plannerBridgeFixture(t *testing.T) (*Server, *Task) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		db.SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { _ = m.Close() })
	task, err := m.CreateTask("Planner bridge fixture", "Observe local fixture only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	if _, err := task.Store.AddGoal(map[string]any{"text": "Observe local fixture only"}, "human"); err != nil {
		t.Fatal(err)
	}
	task.updateLifecycle(func(state *taskLifecycleState) { state.Status = "running" })
	s := newAdmissionTestServer(m, nil)
	s.ctx = t.Context()
	s.engine.started.Store(task.ID, true) // advertise availability, but start no background engine or LLM
	return s, task
}

func plannerBridgeRequest(s *Server, task *Task, direct bool, text string) *httptest.ResponseRecorder {
	field := "message"
	if direct {
		field = "text"
	}
	body, _ := json.Marshal(map[string]string{field: text})
	r := httptest.NewRequest(http.MethodPost, "/api/chat?task="+task.ID, strings.NewReader(string(body)))
	r.SetPathValue("id", task.ID)
	w := httptest.NewRecorder()
	if direct {
		s.plannerInstructions(w, r)
	} else {
		s.chat(w, r)
	}
	return w
}

func TestPlannerInstructionAPIUsesTypedQueueAndIdempotency(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	for i := 0; i < 2; i++ {
		body := `{"text":"Correct the current worker approach","request_id":"same-message","seg":0}`
		r := httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID+"/planner-instructions", strings.NewReader(body))
		r.SetPathValue("id", task.ID)
		w := httptest.NewRecorder()
		s.plannerInstructions(w, r)
		if w.Code != 201 {
			t.Fatalf("status %d %s", w.Code, w.Body)
		}
		var receipt mainMessageReceipt
		_ = json.Unmarshal(w.Body.Bytes(), &receipt)
		if receipt.MessageID == 0 || receipt.Delivery != "queued" {
			t.Fatalf("receipt %+v", receipt)
		}
	}
	rows, err := task.Store.ListMainMessages(t.Context(), 0, 0, 100)
	if err != nil || len(rows) != 1 || rows[0].DeliveryStatus != "queued" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	hints, err := task.Store.ListByKind(db.KindHint, 100)
	if err != nil || len(hints) != 0 {
		t.Fatal("conversation entered standing hints")
	}
	batch, err := task.Store.ClaimPlannerMessages(t.Context(), 8)
	if err != nil || batch == nil || len(batch.Messages) != 1 {
		t.Fatalf("batch=%+v err=%v", batch, err)
	}
}

func TestPlannerInstructionPreservesLifecycleAndRejectsUnacceptedMessages(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	for _, status := range []string{"paused", "done", "failed", "timeout", "queued"} {
		task.updateLifecycle(func(state *taskLifecycleState) {
			state.Status, state.Paused, state.Queued = status, status == "paused", status == "queued"
		})
		before := task.lifecycleSnapshot()
		w := plannerBridgeRequest(s, task, true, "Preserve lifecycle "+status)
		var receipt mainMessageReceipt
		_ = json.Unmarshal(w.Body.Bytes(), &receipt)
		after := task.lifecycleSnapshot()
		wantDelivery := "waiting"
		if isTerminalStatus(status) {
			wantDelivery = "blocked"
			if receipt.TaskStatus != status || receipt.RequiredAction != "restart" {
				t.Fatalf("terminal continuation metadata missing: %+v", receipt)
			}
		}
		if w.Code != http.StatusCreated || receipt.Delivery != wantDelivery || receipt.Reason == "" || after.Status != before.Status || after.Paused != before.Paused || after.Queued != before.Queued {
			t.Fatalf("state=%s response=%d %s before=%+v after=%+v", status, w.Code, w.Body, before, after)
		}
	}
	task.updateLifecycle(func(state *taskLifecycleState) { state.Status, state.Paused, state.Queued = "running", false, false })
	if _, err := s.m.pg.Exec(`UPDATE exploration_nodes SET state='met' WHERE exploration_id=$1 AND kind='goal'`, task.ExpID); err != nil {
		t.Fatal(err)
	}
	w := plannerBridgeRequest(s, task, true, "No new goal implied")
	var receipt mainMessageReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &receipt)
	if w.Code != http.StatusCreated || receipt.Delivery != "queued" {
		t.Fatalf("goalless response=%d %s", w.Code, w.Body)
	}
	before, _ := task.Store.ListByKind(db.KindHint, 100)
	s.chatBusy[task.ID] = true
	if w := plannerBridgeRequest(s, task, false, "busy request must not be forwarded"); w.Code != http.StatusConflict {
		t.Fatalf("busy response=%d %s", w.Code, w.Body)
	}
	s.chatBusy[task.ID] = false
	if w := plannerBridgeRequest(s, task, false, " \n "); w.Code != http.StatusBadRequest {
		t.Fatalf("blank response=%d %s", w.Code, w.Body)
	}
	if w := plannerBridgeRequest(s, task, true, strings.Repeat("x", maxPlannerInstructionChars+1)); w.Code != http.StatusBadRequest {
		t.Fatalf("oversized response=%d %s", w.Code, w.Body)
	}
	s.engine.runtimeMu.Lock()
	s.engine.draining = true
	s.engine.runtimeMu.Unlock()
	if w := plannerBridgeRequest(s, task, true, "draining request must not be forwarded"); w.Code != http.StatusConflict {
		t.Fatalf("draining response=%d %s", w.Code, w.Body)
	}
	s.engine.runtimeMu.Lock()
	s.engine.draining = false
	s.engine.runtimeMu.Unlock()
	after, _ := task.Store.ListByKind(db.KindHint, 100)
	if len(after) != len(before) {
		t.Fatal("rejected request created an instruction")
	}
}

func TestPlannerInstructionSaveFailureStopsMainAndReleasesAdmission(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	actualStore := task.Store
	task.Store = s.m.pg.Exploration(-1) // intentional FK failure, isolated fixture only
	t.Cleanup(func() { task.Store = actualStore })
	w := plannerBridgeRequest(s, task, false, "Do not start Main unless this saves")
	if w.Code != http.StatusInternalServerError || s.chatBusy[task.ID] || s.chatCancel[task.ID] != nil || s.engine.inflightCount(task.ID) != 0 {
		t.Fatalf("failed save leaked acceptance/busy state: %d %s", w.Code, w.Body)
	}
	if len(task.drainTriggers()) != 0 {
		t.Fatal("failed save notified Planner")
	}
	if _, exists := s.chatCancel[task.ID]; exists {
		t.Fatal("failed save retained a Main cancellation handle")
	}
}
