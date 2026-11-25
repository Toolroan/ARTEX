package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestMainActionOwnsDurableWorkWithExistingGoals(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	m, _, err := task.Store.CreateMainMessage(t.Context(), db.MainMessageInput{RequestKey: "ordinary-work", Mode: "instruction", InputSource: "main", RawText: "Analyze the synthetic fixture", OperatorText: "Analyze the synthetic fixture"})
	if err != nil {
		t.Fatal(err)
	}
	m, err = s.routeMainMessage(t.Context(), task, m.ID)
	if err != nil || m == nil || m.DeliveryOwner != "main" {
		t.Fatalf("Main lost ordinary work ownership: message=%+v err=%v", m, err)
	}
	w, err := task.Store.GetWorkRequest(t.Context(), m.ID)
	if err != nil || w == nil || w.State != "pending" {
		t.Fatalf("action was not durably enrolled: request=%+v err=%v", w, err)
	}
	m, err = task.Store.ClaimMainDelivery(t.Context(), m.ID)
	if err != nil || m == nil {
		t.Fatal(err)
	}
	if policy := s.mainMutationPolicy(task, m); policy.Mode != "work_request" {
		t.Fatalf("Main with existing goals got mode %q", policy.Mode)
	}
}

func TestWorkerEndQueuesOutcomeReviewBeforeTaskSuccess(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	if _, err := s.m.pg.Exec(`UPDATE exploration_nodes SET state='met' WHERE exploration_id=$1 AND kind='goal'`, task.ExpID); err != nil {
		t.Fatal(err)
	}
	m := makeMainOwnedFixture(t, task)
	m, err := task.Store.ClaimMainDelivery(t.Context(), m.ID)
	if err != nil || m == nil {
		t.Fatal(err)
	}
	grant := db.WorkRequestGrant{RequestID: m.ID, Revision: 1, DeliveryToken: m.DeliveryToken, Owner: "main"}
	item, err := task.Store.AllocateWorkRequestItem(t.Context(), grant, "intent", "Count synthetic rows")
	if err != nil {
		t.Fatal(err)
	}
	intentID, _, err := task.Store.CreateWorkRequestNode(t.Context(), grant, db.WorkNodeSpec{ItemID: item.ID, Payload: map[string]any{"summary": "Count synthetic rows"}, Origin: "mainagent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Store.HandoffMainRequest(t.Context(), m.ID, m.DeliveryToken); err != nil {
		t.Fatal(err)
	}
	batch, err := task.Store.ClaimPlannerMessages(t.Context(), 8)
	if err != nil || batch == nil {
		t.Fatalf("handoff not claimed: %v", err)
	}
	if err := task.Store.FinishPlannerMessages(t.Context(), batch.Token, true); err != nil {
		t.Fatal(err)
	}
	if err := task.Store.SetIntentState(intentID, "done"); err != nil {
		t.Fatal(err)
	}
	task.NotifyDone(intentID)
	request, err := task.Store.GetWorkRequest(t.Context(), m.ID)
	if err != nil || request.State != "review" {
		t.Fatalf("worker end did not request review: %+v %v", request, err)
	}
	if won, err := s.m.SetTaskStatusGuarded(task.ID, "done", ""); err != nil || won {
		t.Fatalf("worker end prematurely completed task: won=%v err=%v", won, err)
	}
	batch, err = task.Store.ClaimPlannerMessages(t.Context(), 8)
	if err != nil || batch == nil || len(batch.Messages) != 1 || batch.Messages[0].ID != m.ID {
		t.Fatalf("review did not reuse the source request: %+v %v", batch, err)
	}
	factID, err := task.Store.AddNode(db.KindFact, map[string]any{"summary": "Synthetic row count was verified", "confidence": "observed", "source": "tool_output"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	grant.Owner, grant.DeliveryToken = "planner", batch.Token
	if err := task.Store.ResolveWorkRequest(t.Context(), grant, "completed", "Count matches the fixture", []int64{factID}); err != nil {
		t.Fatal(err)
	}
	if err := task.Store.FinishPlannerMessages(t.Context(), batch.Token, true); err != nil {
		t.Fatal(err)
	}
	if won, err := s.m.SetTaskStatusGuarded(task.ID, "done", ""); err != nil || !won {
		t.Fatalf("verified request did not permit completion: won=%v err=%v", won, err)
	}
	if task.lifecycleSnapshot().Status != "done" {
		t.Fatal("durable completion was not reflected on live task")
	}
	r := httptest.NewRequest(http.MethodGet, "/api/tasks/"+task.ID+"/work-requests", nil)
	r.SetPathValue("id", task.ID)
	response := httptest.NewRecorder()
	s.taskWorkRequests(response, r)
	var body struct {
		Requests []db.WorkRequest `json:"requests"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body.Requests) != 1 || body.Requests[0].State != "completed" || body.Requests[0].OperatorText != "" {
		t.Fatalf("work status projection: %d %s", response.Code, response.Body)
	}
}

func TestUnresolvedRequestBlocksSuccessButNotResourceTermination(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	makeMainOwnedFixture(t, task)
	if err := s.m.SetTaskStatus(task.ID, "done"); err == nil {
		t.Fatal("unguarded success setter bypassed durable work")
	}
	if won, err := s.m.SetTaskStatusGuarded(task.ID, "timeout", db.EndReasonBudget); err != nil || !won {
		t.Fatalf("resource limit could not end run: won=%v err=%v", won, err)
	}
	requests, err := task.Store.ListWorkRequests(t.Context(), 10)
	if err != nil || len(requests) != 1 || requests[0].State != "pending" {
		t.Fatalf("run limit lost the unresolved request: %+v %v", requests, err)
	}
}

func TestOperatorReviewRecoversInterruptedRequestWithoutResumingTask(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	m := makeMainOwnedFixture(t, task)
	m, err := task.Store.ClaimMainDelivery(t.Context(), m.ID)
	if err != nil || m == nil {
		t.Fatal(err)
	}
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/review", nil)
		r.SetPathValue("id", task.ID)
		r.SetPathValue("request", strconvMessageID(m.ID))
		w := httptest.NewRecorder()
		s.reviewWorkRequest(w, r)
		return w
	}
	if w := call(); w.Code != http.StatusConflict {
		t.Fatalf("review stole live ownership: %d %s", w.Code, w.Body)
	}
	if err := task.Store.FinishMainDelivery(t.Context(), m.ID, m.DeliveryToken, false); err != nil {
		t.Fatal(err)
	}
	if err := s.m.SetTaskStatus(task.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	w := call()
	var receipt struct {
		Queued        bool   `json:"queued"`
		WaitingReason string `json:"waiting_reason"`
	}
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || !receipt.Queued || receipt.WaitingReason == "" {
		t.Fatalf("review receipt: %d %s", w.Code, w.Body)
	}
	if task.lifecycleSnapshot().Status != "timeout" {
		t.Fatal("review bypassed task continuation")
	}
	requests, err := task.Store.ListWorkRequests(t.Context(), 10)
	if err != nil || len(requests) != 1 || requests[0].ID != m.ID || requests[0].Revision != 1 || requests[0].State != "review" {
		t.Fatalf("review replaced or duplicated request: %+v %v", requests, err)
	}
}
