package server

import (
	"context"
	"encoding/json"
	"errors"
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

type instructionTestStore struct {
	err     error
	written bool
	text    string
}

func (s *instructionTestStore) AddNode(kind string, payload map[string]any, priority int, state, origin string, anchors []int64) (int64, error) {
	if kind != db.KindHint || priority != 0 || state != "active" || origin != "human" || len(anchors) != 0 {
		return 0, errors.New("unexpected instruction metadata")
	}
	if s.err != nil {
		return 0, s.err
	}
	s.written, s.text = true, payload["text"].(string)
	return 42, nil
}

func TestPlannerInstructionDurableBeforeNotification(t *testing.T) {
	for _, fail := range []bool{false, true} {
		store := &instructionTestStore{}
		if fail {
			store.err = errors.New("fixture persistence failure")
		}
		notified := false
		id, err := persistPlannerInstruction(store, "What's the status?", func(text string) {
			if !store.written || text != "What's the status?" {
				t.Fatal("notification preceded durable user-text save")
			}
			notified = true
		})
		if fail && (err == nil || notified || id != 0) {
			t.Fatalf("failed save accepted: id=%d notified=%v err=%v", id, notified, err)
		}
		if !fail && (err != nil || !notified || id != 42) {
			t.Fatalf("save not delivered: id=%d notified=%v err=%v", id, notified, err)
		}
	}
	for _, invalid := range []string{"", " \n ", strings.Repeat("界", maxPlannerInstructionChars+1)} {
		store := &instructionTestStore{}
		if _, err := persistPlannerInstruction(store, invalid, nil); err == nil || store.written {
			t.Fatal("invalid instruction reached persistence")
		}
	}
	if _, err := validatePlannerInstruction(strings.Repeat("界", maxPlannerInstructionChars)); err != nil {
		t.Fatal("Unicode character limit counted bytes instead of characters")
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

func TestPlannerInstructionAPIAndEveryMainMessage(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	for i, item := range []struct {
		direct bool
		text   string
	}{
		{true, "Correct the current worker's approach"},
		{false, "What's the status?"},
		{false, "hint Preserve the previous correction"},
	} {
		w := plannerBridgeRequest(s, task, item.direct, item.text)
		wantStatus := http.StatusOK
		if item.direct {
			wantStatus = http.StatusCreated
		}
		if w.Code != wantStatus {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body)
		}
		var receipt plannerInstructionDelivery
		if item.direct {
			_ = json.Unmarshal(w.Body.Bytes(), &receipt)
		} else {
			var response struct {
				PlannerDelivery plannerInstructionDelivery `json:"planner_delivery"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &response)
			receipt = response.PlannerDelivery
		}
		if receipt.HintID == 0 || receipt.Delivery != "notified" {
			t.Fatalf("receipt=%+v", receipt)
		}
		node, err := task.Store.GetNode(receipt.HintID)
		if err != nil || node == nil || node.Kind != db.KindHint || node.Origin != "human" {
			t.Fatalf("saved node=%+v err=%v", node, err)
		}
		var payload struct{ Text string }
		_ = json.Unmarshal(node.Payload, &payload)
		if payload.Text != item.text {
			t.Fatalf("stored operator text=%q, want %q", payload.Text, item.text)
		}
		triggers := task.drainTriggers()
		if len(triggers) != 1 || triggers[0].Kind != "hint" || triggers[0].HintOrigin != "human" || len(triggers[0].Hints) != 1 || triggers[0].Hints[0] != item.text {
			t.Fatalf("triggers=%+v", triggers)
		}
		nodes, err := task.Store.ListByKind(db.KindHint, 100)
		if err != nil || len(nodes) != i+1 {
			t.Fatalf("duplicate or lost instruction: count=%d err=%v", len(nodes), err)
		}
	}
	activities, _, err := task.Store.ActivityList(nil, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	operatorEchoes := 0
	for _, row := range activities {
		if row.Worker == "planner" && row.Kind == "user" {
			operatorEchoes++
		}
	}
	if operatorEchoes != 3 {
		t.Fatalf("Planner user echoes=%d, want exactly the three accepted user messages", operatorEchoes)
	}
}

func TestPlannerInstructionPreservesLifecycleAndRejectsUnacceptedMessages(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	for _, status := range []string{"paused", "done", "queued"} {
		task.updateLifecycle(func(state *taskLifecycleState) {
			state.Status, state.Paused, state.Queued = status, status == "paused", status == "queued"
		})
		before := task.lifecycleSnapshot()
		w := plannerBridgeRequest(s, task, true, "Preserve lifecycle "+status)
		var receipt plannerInstructionDelivery
		_ = json.Unmarshal(w.Body.Bytes(), &receipt)
		after := task.lifecycleSnapshot()
		if w.Code != http.StatusCreated || receipt.Delivery != "saved" || receipt.Reason == "" || after.Status != before.Status || after.Paused != before.Paused || after.Queued != before.Queued {
			t.Fatalf("state=%s response=%d %s before=%+v after=%+v", status, w.Code, w.Body, before, after)
		}
	}
	task.updateLifecycle(func(state *taskLifecycleState) { state.Status, state.Paused, state.Queued = "running", false, false })
	if _, err := s.m.pg.Exec(`UPDATE exploration_nodes SET state='met' WHERE exploration_id=$1 AND kind='goal'`, task.ExpID); err != nil {
		t.Fatal(err)
	}
	w := plannerBridgeRequest(s, task, true, "No new goal implied")
	var receipt plannerInstructionDelivery
	_ = json.Unmarshal(w.Body.Bytes(), &receipt)
	if w.Code != http.StatusCreated || receipt.Delivery != "saved" || !strings.Contains(receipt.Reason, "no open goals") {
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
