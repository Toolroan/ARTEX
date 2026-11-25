package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func workMessage(t *testing.T, s *ExplorationStore, key, owner string) *MainMessage {
	t.Helper()
	m := createMainMessage(t, s, mainMessageInput(key))
	return finishMainRoute(t, s, m.ID, MainRouteDecision{Route: "action", Owner: owner, DecisionData: json.RawMessage(`{}`)})
}
func claimWork(t *testing.T, s *ExplorationStore, m *MainMessage) WorkRequestGrant {
	t.Helper()
	var token string
	if m.DeliveryOwner == "main" {
		claimed, err := s.ClaimMainDelivery(t.Context(), m.ID)
		if err != nil || claimed == nil {
			t.Fatalf("claim Main: %v %v", claimed, err)
		}
		token = claimed.DeliveryToken
	} else {
		batch, err := s.ClaimPlannerMessages(t.Context(), 8)
		if err != nil || batch == nil {
			t.Fatalf("claim Planner: %v %v", batch, err)
		}
		token = batch.Token
	}
	w, err := s.GetWorkRequest(t.Context(), m.ID)
	if err != nil || w == nil {
		t.Fatalf("read request: %v %v", w, err)
	}
	return WorkRequestGrant{RequestID: m.ID, Revision: w.Revision, DeliveryToken: token, Owner: m.DeliveryOwner}
}
func createWorkNode(t *testing.T, s *ExplorationStore, g WorkRequestGrant, kind, summary string) int64 {
	t.Helper()
	item, err := s.AllocateWorkRequestItem(t.Context(), g, kind, summary)
	if err != nil {
		t.Fatal(err)
	}
	field := "summary"
	if kind == "goal" {
		field = "text"
	}
	id, created, err := s.CreateWorkRequestNode(t.Context(), g, WorkNodeSpec{ItemID: item.ID, Payload: map[string]any{field: summary}, Priority: 1})
	if err != nil || !created || id == 0 {
		t.Fatalf("create %s: id=%d created=%t err=%v", kind, id, created, err)
	}
	return id
}

func TestWorkRequestEnrollmentTracksOnlyNewActionableRouting(t *testing.T) {
	_, s := mainMessageFixture(t)
	for i, decision := range []MainRouteDecision{
		{Route: "context"}, {Route: "clarify"}, {Route: "action", Owner: "main"}, {Route: "correction", Owner: "planner"},
		{Route: "control", Owner: "main", DecisionData: json.RawMessage(`{"control_tools":["set_goals"]}`)},
		{Route: "control", Owner: "main", DecisionData: json.RawMessage(`{"control_tools":["continue_task"]}`)},
	} {
		m := createMainMessage(t, s, mainMessageInput(fmt.Sprint("enroll-", i)))
		m = finishMainRoute(t, s, m.ID, decision)
		w, err := s.GetWorkRequest(t.Context(), m.ID)
		want := i >= 2 && i <= 4
		if err != nil || (w != nil) != want {
			t.Fatalf("route %+v request=%+v err=%v", decision, w, err)
		}
		if w != nil && (w.ID != m.ID || w.SourceMessageID != m.ID || w.Revision != 1 || w.State != "pending" || w.Acceptance != m.OperatorText) {
			t.Fatalf("request lost authority: %+v", w)
		}
	}
}

func TestWorkRequestItemAllocationCreationAndRevisionAreFenced(t *testing.T) {
	d, s := mainMessageFixture(t)
	origin, err := s.OriginFactID()
	if err != nil {
		t.Fatal(err)
	}
	m := workMessage(t, s, "one-outcome", "main")
	g := claimWork(t, s, m)
	var wg sync.WaitGroup
	ids := make(chan int64, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			item, err := s.AllocateWorkRequestItem(t.Context(), g, "goal", "A verified fixture outcome")
			if err != nil {
				errs <- err
			} else {
				ids <- item.ID
			}
		})
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var itemID int64
	for id := range ids {
		if itemID != 0 && id != itemID {
			t.Fatal("allocation retry duplicated work item")
		}
		itemID = id
	}
	spec := WorkNodeSpec{ItemID: itemID, Payload: map[string]any{"text": "A verified fixture outcome"}}
	id, created, err := s.CreateWorkRequestNode(t.Context(), g, spec)
	if err != nil || !created {
		t.Fatalf("create: %d %t %v", id, created, err)
	}
	again, created, err := s.CreateWorkRequestNode(t.Context(), g, spec)
	if err != nil || created || again != id {
		t.Fatalf("retry: %d %t %v", again, created, err)
	}
	var linked bool
	if err = d.QueryRow(`SELECT EXISTS(SELECT 1 FROM exploration_edges WHERE src_id=$1 AND dst_id=$2 AND rel='spawns')`, origin, id).Scan(&linked); err != nil || !linked {
		t.Fatal("tracked goal lost origin lineage")
	}
	spec.Priority = 9
	if _, _, err = s.CreateWorkRequestNode(t.Context(), g, spec); err == nil {
		t.Fatal("mutated retry was accepted")
	}
	wrong := g
	wrong.DeliveryToken = "wrong"
	if _, err = s.AllocateWorkRequestItem(t.Context(), wrong, "intent", "must reject"); !errors.Is(err, ErrWorkRequestConflict) {
		t.Fatalf("wrong lease accepted: %v", err)
	}
	w, err := s.ReviseWorkRequest(t.Context(), g, "A corrected verified outcome")
	if err != nil || w.Revision != 2 {
		t.Fatalf("revise: %+v %v", w, err)
	}
	if _, err = s.AllocateWorkRequestItem(t.Context(), g, "intent", "stale revision"); !errors.Is(err, ErrWorkRequestConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
}

func TestWorkRequestMainHandoffBlocksPoolUntilPlannerReview(t *testing.T) {
	_, s := mainMessageFixture(t)
	m := workMessage(t, s, "main-intake", "main")
	g := claimWork(t, s, m)
	iid := createWorkNode(t, s, g, "intent", "Inspect the saved fixture")
	if token, err := s.ClaimIntentTokenAfterPlannerReview(t.Context(), iid, "pool"); err != nil || token != "" {
		t.Fatalf("pool raced Main intake: %q %v", token, err)
	}
	if err := s.HandoffMainRequest(t.Context(), m.ID, g.DeliveryToken); err != nil {
		t.Fatal(err)
	}
	m, _ = s.GetMainMessage(t.Context(), m.ID)
	if m.DeliveryOwner != "planner" || m.DeliveryStatus != "queued" {
		t.Fatalf("handoff replaced source identity: %+v", m)
	}
	g = claimWork(t, s, m)
	if token, err := s.ClaimIntentTokenAfterPlannerReview(t.Context(), iid, "pool"); err != nil || token != "" {
		t.Fatalf("pool raced Planner intake: %q %v", token, err)
	}
	if err := s.FinishPlannerMessages(t.Context(), g.DeliveryToken, true); err != nil {
		t.Fatal(err)
	}
	if token, err := s.ClaimIntentTokenAfterPlannerReview(t.Context(), iid, "pool"); err != nil || token == "" {
		t.Fatalf("review did not release pool: %q %v", token, err)
	}
	var count int
	_ = s.db.QueryRow(`SELECT count(*) FROM main_message_routes WHERE exploration_id=$1`, s.ID()).Scan(&count)
	if count != 1 {
		t.Fatal("handoff duplicated source message")
	}
}

func TestWorkRequestWorkerFinishRequiresOutcomeReview(t *testing.T) {
	d, s := mainMessageFixture(t)
	m := workMessage(t, s, "bounded-result", "planner")
	g := claimWork(t, s, m)
	iid := createWorkNode(t, s, g, "intent", "Compute the saved fixture total")
	if err := s.FinishPlannerMessages(t.Context(), g.DeliveryToken, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIntentState(iid, "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFinishedWorkRequestsForReview(t.Context()); err != nil {
		t.Fatal(err)
	}
	m, _ = s.GetMainMessage(t.Context(), m.ID)
	if m.DeliveryStatus != "queued" {
		t.Fatal("missed worker notification was not recovered")
	}
	w, _ := s.GetWorkRequest(t.Context(), m.ID)
	if w.State != "review" {
		t.Fatalf("worker return claimed fulfillment: %+v", w)
	}
	var taskID int64
	_ = d.QueryRow(`SELECT id FROM tasks WHERE exploration_id=$1`, s.ID()).Scan(&taskID)
	if won, err := d.TryCompleteTask(t.Context(), taskID); err != nil || won {
		t.Fatalf("unreviewed outcome completed task: %t %v", won, err)
	}
	g = claimWork(t, s, m)
	if err := s.ResolveWorkRequest(t.Context(), g, "completed", "worker returned normally", nil); err == nil {
		t.Fatal("worker termination used as fulfillment proof")
	}
	evidence, err := s.AddNode(KindFact, map[string]any{"summary": "The fixture total is 6", "confidence": "observed", "source": "tool_output"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveWorkRequest(t.Context(), g, "completed", "Observed fixture total satisfies the requested calculation", []int64{evidence}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishPlannerMessages(t.Context(), g.DeliveryToken, true); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkIntentRequestsForReview(t.Context(), iid); err != nil {
		t.Fatal(err)
	}
	if won, err := d.TryCompleteTask(t.Context(), taskID); err != nil || !won {
		t.Fatalf("verified outcome did not complete: %t %v", won, err)
	}
}

func TestWorkRequestDirtyReviewPreservesLeaseAndDoesNotSpin(t *testing.T) {
	_, s := mainMessageFixture(t)
	m := workMessage(t, s, "dirty-review", "planner")
	g := claimWork(t, s, m)
	iid := createWorkNode(t, s, g, "intent", "Review the synthetic result")
	if err := s.SetIntentState(iid, "blocked"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkIntentRequestsForReview(t.Context(), iid); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckMainDelivery(t.Context(), g.DeliveryToken); err != nil {
		t.Fatalf("worker completion revoked active Planner lease: %v", err)
	}
	if err := s.FinishPlannerMessages(t.Context(), g.DeliveryToken, true); err != nil {
		t.Fatal(err)
	}
	m, _ = s.GetMainMessage(t.Context(), m.ID)
	if m.DeliveryStatus != "queued" {
		t.Fatal("dirty round did not schedule exactly one follow-up")
	}
	g = claimWork(t, s, m)
	if err := s.ResolveWorkRequest(t.Context(), g, "blocked", "Fixture requires an operator decision", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishPlannerMessages(t.Context(), g.DeliveryToken, true); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := s.MarkIntentRequestsForReview(t.Context(), iid); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkFinishedWorkRequestsForReview(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	m, _ = s.GetMainMessage(t.Context(), m.ID)
	w, _ := s.GetWorkRequest(t.Context(), m.ID)
	if m.DeliveryStatus != "presented" || w.State != "blocked" {
		t.Fatalf("blocked request spun automatic reviews: %+v %+v", m, w)
	}
	if n, err := s.QueueWorkRequestReviews(t.Context()); err != nil || n != 1 {
		t.Fatalf("explicit wake did not reopen one review: %d %v", n, err)
	}
	if n, err := s.QueueWorkRequestReviews(t.Context()); err != nil || n != 0 {
		t.Fatalf("repeated wake duplicated review: %d %v", n, err)
	}
}

func TestWorkRequestGoalsAndUncertainDeliveryCannotPretendComplete(t *testing.T) {
	_, s := mainMessageFixture(t)
	m := workMessage(t, s, "persistent-outcome", "planner")
	g := claimWork(t, s, m)
	goal := createWorkNode(t, s, g, "goal", "Produce the verified fixture deliverable")
	if err := s.ResolveWorkRequest(t.Context(), g, "completed", "not yet proven", nil); err == nil {
		t.Fatal("open linked goal was ignored")
	}
	if err := s.SetNodeState(goal, "met"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveWorkRequest(t.Context(), g, "cancelled", "model decided to stop", nil); err == nil {
		t.Fatal("model cancelled operator request")
	}
	if err := s.FinishPlannerMessages(t.Context(), g.DeliveryToken, false); err != nil {
		t.Fatal(err)
	}
	if n, err := s.QueueWorkRequestReviews(t.Context()); err != nil || n != 0 {
		t.Fatalf("uncertain effect was automatically replayed: %d %v", n, err)
	}
	w, _ := s.GetWorkRequest(t.Context(), m.ID)
	if w.State != "blocked" || !strings.Contains(w.Resolution, "interrupted") {
		t.Fatalf("interruption lost uncertainty: %+v", w)
	}
}

func TestTaskCompletionUsesSameParentLockAsIntake(t *testing.T) {
	d, s := mainMessageFixture(t)
	var taskID int64
	if err := d.QueryRow(`SELECT id FROM tasks WHERE exploration_id=$1`, s.ID()).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	tx, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, taskID); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	result := make(chan bool, 1)
	errs := make(chan error, 1)
	go func() { won, err := d.TryCompleteTask(ctx, taskID); result <- won; errs <- err }()
	select {
	case <-result:
		t.Fatal("completion ignored intake parent lock")
	case <-time.After(30 * time.Millisecond):
	}
	if _, err = tx.ExecContext(t.Context(), `INSERT INTO main_message_routes(exploration_id,main_seg,request_key,mode,input_source,raw_text,operator_text) VALUES($1,0,'racing-intake','auto','main','fixture','fixture')`, s.ID()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if won := <-result; won {
		t.Fatal("completion ignored newly accepted pending request")
	}
	if err = <-errs; err != nil {
		t.Fatal(err)
	}
}

func TestWorkRequestActiveLeaseCanBeRevisitedButEmptyPlanBlocks(t *testing.T) {
	_, s := mainMessageFixture(t)
	m := workMessage(t, s, "active-plan", "planner")
	g := claimWork(t, s, m)
	createWorkNode(t, s, g, "intent", "First bounded fixture step")
	if err := s.FinishPlannerMessages(t.Context(), g.DeliveryToken, true); err != nil {
		t.Fatal(err)
	}
	batch, err := s.ClaimPlannerMessages(t.Context(), 8)
	if err != nil || batch == nil || batch.Messages[0].ID != m.ID {
		t.Fatalf("goalful follow-on round lost request authority: %+v %v", batch, err)
	}
	if err := s.FinishPlannerMessages(t.Context(), batch.Token, true); err != nil {
		t.Fatal(err)
	}
	empty := workMessage(t, s, "empty-plan", "planner")
	batch, err = s.ClaimPlannerMessages(t.Context(), 1)
	if err != nil || batch == nil || batch.Messages[0].ID != empty.ID {
		t.Fatalf("active historic plan starved new instruction: %+v %v", batch, err)
	}
	if err := s.FinishPlannerMessages(t.Context(), batch.Token, true); err != nil {
		t.Fatal(err)
	}
	w, _ := s.GetWorkRequest(t.Context(), empty.ID)
	if w.State != "blocked" {
		t.Fatalf("review-only reply silently fulfilled request: %+v", w)
	}
}

func TestWorkRequestExplicitReviewPreservesEffectsAndRejectsLiveOwner(t *testing.T) {
	_, s := mainMessageFixture(t)
	m := workMessage(t, s, "operator-review", "planner")
	g := claimWork(t, s, m)
	iid := createWorkNode(t, s, g, "intent", "Preserve already-created fixture work")
	if err := s.ReviewWorkRequest(t.Context(), m.ID); err == nil {
		t.Fatal("explicit review revoked live owner")
	}
	if err := s.FinishPlannerMessages(t.Context(), g.DeliveryToken, false); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetWorkRequest(t.Context(), m.ID)
	if err := s.ReviewWorkRequest(t.Context(), m.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReviewWorkRequest(t.Context(), m.ID); err != nil {
		t.Fatalf("review retry not idempotent: %v", err)
	}
	after, _ := s.GetWorkRequest(t.Context(), m.ID)
	source, _ := s.GetMainMessage(t.Context(), m.ID)
	if after.ID != before.ID || after.Revision != before.Revision || after.State != "review" || len(after.Nodes) != 1 || after.Nodes[0].NodeID != iid || source.DeliveryStatus != "queued" || source.DeliveryToken != "" {
		t.Fatalf("explicit review lost effects/identity: before=%+v after=%+v source=%+v", before, after, source)
	}
	if err := s.CheckMainDelivery(t.Context(), g.DeliveryToken); err == nil {
		t.Fatal("old token retained effect authority")
	}
}

func TestWorkRequestBlockedIdleDoesNotSuppressUnrelatedWork(t *testing.T) {
	_, s := mainMessageFixture(t)
	m := workMessage(t, s, "blocked-goal", "planner")
	g := claimWork(t, s, m)
	createWorkNode(t, s, g, "goal", "Outcome awaiting operator input")
	if err := s.ResolveWorkRequest(t.Context(), g, "blocked", "Need the fixture input", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishPlannerMessages(t.Context(), g.DeliveryToken, true); err != nil {
		t.Fatal(err)
	}
	if waiting, err := s.WorkRequestsAwaitingInput(t.Context()); err != nil || !waiting {
		t.Fatalf("blocked request would spin: %t %v", waiting, err)
	}
	unrelated, err := s.AddGoal(map[string]any{"text": "Unrelated legitimate work"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if waiting, err := s.WorkRequestsAwaitingInput(t.Context()); err != nil || waiting {
		t.Fatalf("blocked request suppressed unrelated goal: %t %v", waiting, err)
	}
	if err := s.SetNodeState(unrelated, "met"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueWorkRequestReviews(t.Context()); err != nil {
		t.Fatal(err)
	}
	if waiting, err := s.WorkRequestsAwaitingInput(t.Context()); err != nil || waiting {
		t.Fatalf("explicit review stayed idle: %t %v", waiting, err)
	}
}

func TestReopenedObligationSharesCompletionParentLock(t *testing.T) {
	d, s := mainMessageFixture(t)
	iid := addClaimIntent(t, s)
	if err := s.SetIntentState(iid, "blocked"); err != nil {
		t.Fatal(err)
	}
	tx, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), `SELECT id FROM tasks WHERE exploration_id=$1 FOR UPDATE`, s.ID()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.ReopenIntent(iid); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("reopen bypassed completion parent lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	goal, err := s.AddGoal(map[string]any{"text": "Old verified outcome"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetNodeState(goal, "met"); err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateGoalPayload(goal, "Changed operator outcome", ""); err != nil {
		t.Fatal(err)
	}
	node, _ := s.GetNode(goal)
	if node.State != "open" {
		t.Fatal("edited goal retained proof of its old definition")
	}
}

func TestWorkRequestArchiveRestorePreservesIdentityAndUncertainty(t *testing.T) {
	d, s := mainMessageFixture(t)
	m := workMessage(t, s, "archived-review", "planner")
	g := claimWork(t, s, m)
	iid := createWorkNode(t, s, g, "intent", "Archived fixture step")
	allocated, err := s.AllocateWorkRequestItem(t.Context(), g, "goal", "Allocated unfinished fixture goal")
	if err != nil {
		t.Fatal(err)
	}
	queued := workMessage(t, s, "archived-queued", "planner")
	var taskID int64
	if err = d.QueryRow(`SELECT id FROM tasks WHERE exploration_id=$1`, s.ID()).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if err = d.SetPaused(taskID, true); err != nil {
		t.Fatal(err)
	}
	job, err := d.QueueTaskArchive(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.SnapshotTaskArchive(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.FormatVersion != 4 || snapshot.DataCounts["work_requests"] != 2 || snapshot.DataCounts["work_request_nodes"] != 2 {
		t.Fatalf("archive omitted obligations: %+v", snapshot.DataCounts)
	}
	if err = d.CompleteTaskArchive(job.ID, snapshot, "/tmp/work-request-fixture.tar.zst", "fixture", 1, 1); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = d.QueryRow(`SELECT count(*) FROM work_requests WHERE exploration_id=$1`, s.ID()).Scan(&count); err != nil || count != 0 {
		t.Fatalf("archive hot cleanup left requests: %d %v", count, err)
	}
	if _, err = d.QueueTaskArchiveRestore(job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = d.RestoreTaskArchive(job.ID, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	w, err := s.GetWorkRequest(t.Context(), m.ID)
	if err != nil || w.State != "blocked" || w.Revision != 1 || len(w.Nodes) != 2 || w.Nodes[0].NodeID != iid || w.Nodes[1].ID != allocated.ID || w.Nodes[1].NodeID != 0 {
		t.Fatalf("restored request identity changed: %+v %v", w, err)
	}
	source, _ := s.GetMainMessage(t.Context(), m.ID)
	if source.DeliveryStatus != "needs_review" || source.DeliveryToken != "" {
		t.Fatalf("restored effects replayable: %+v", source)
	}
	q, _ := s.GetWorkRequest(t.Context(), queued.ID)
	qm, _ := s.GetMainMessage(t.Context(), queued.ID)
	if q.State != "pending" || qm.DeliveryStatus != "queued" {
		t.Fatal("archive changed never-attempted request")
	}
	if _, err = d.DeleteTaskCascade(taskID, false, true); err != nil {
		t.Fatal(err)
	}
	if err = d.QueryRow(`SELECT count(*) FROM work_request_nodes WHERE request_id=$1`, m.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("delete leaked request links: %d %v", count, err)
	}
}

func TestTaskCompletionDoesNotAdoptHistoricalUncertainMessages(t *testing.T) {
	d, s := mainMessageFixture(t)
	m := workMessage(t, s, "legacy-uncertain", "planner")
	g := claimWork(t, s, m)
	if err := s.FinishPlannerMessages(t.Context(), g.DeliveryToken, false); err != nil {
		t.Fatal(err)
	}
	var taskID int64
	if err := d.QueryRow(`SELECT id FROM tasks WHERE exploration_id=$1`, s.ID()).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if won, err := d.TryCompleteTask(t.Context(), taskID); err != nil || won {
		t.Fatalf("tracked uncertain request did not block success: %t %v", won, err)
	}
	// Simulate a pre-migration uncertain receipt: the migration deliberately
	// creates no durable obligation for historical messages.
	if _, err := d.Exec(`DELETE FROM work_requests WHERE id=$1`, m.ID); err != nil {
		t.Fatal(err)
	}
	if won, err := d.TryCompleteTask(t.Context(), taskID); err != nil || !won {
		t.Fatalf("historical uncertainty was silently adopted: %t %v", won, err)
	}
	stored, err := s.GetMainMessage(t.Context(), m.ID)
	if err != nil || stored.DeliveryStatus != "needs_review" {
		t.Fatalf("legacy receipt was changed/replayed: %+v %v", stored, err)
	}
}
