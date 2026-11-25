package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func mainMessageFixture(t *testing.T) (*DB, *ExplorationStore) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("Main routing fixture", "synthetic goal", nil, 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.DeleteTaskCascade(task.ID, false, true) })
	return d, d.Exploration(task.ExplorationID)
}

func mainMessageInput(key string) MainMessageInput {
	return MainMessageInput{RequestKey: key, Mode: "auto", InputSource: "main", RawText: "synthetic request", OperatorText: "synthetic request"}
}

func createMainMessage(t *testing.T, s *ExplorationStore, in MainMessageInput) *MainMessage {
	t.Helper()
	m, created, err := s.CreateMainMessage(t.Context(), in)
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	return m
}

func finishMainRoute(t *testing.T, s *ExplorationStore, id int64, d MainRouteDecision) *MainMessage {
	t.Helper()
	m, err := s.ClaimMainRouting(t.Context(), id)
	if err != nil || m == nil {
		t.Fatalf("claim: row=%v err=%v", m, err)
	}
	m, err = s.FinishMainRouting(t.Context(), id, m.RoutingToken, d)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMainMessageAcceptanceIsAtomicAndIdempotent(t *testing.T) {
	d, s := mainMessageFixture(t)
	in := mainMessageInput("same-key")
	in.Context = json.RawMessage(`[{"id":1,"role":"assistant","text":"original context"}]`)
	m := createMainMessage(t, s, in)
	if m.ActivityID == 0 || m.UserActivity == nil || m.UserActivity.ID != m.ActivityID {
		t.Fatal("accepted message lacks atomic activity")
	}
	in.Context = json.RawMessage(`not-valid-on-purpose`) // snapshots are not re-evaluated on replay
	again, created, err := s.CreateMainMessage(t.Context(), in)
	if err != nil || created || again.ID != m.ID || string(again.Context) != string(m.Context) {
		t.Fatalf("replay changed accepted context: %+v %v", again, err)
	}
	for _, change := range []func(*MainMessageInput){
		func(i *MainMessageInput) { i.RawText = "different" }, func(i *MainMessageInput) { i.OperatorText += "\n/path/changed" },
		func(i *MainMessageInput) { i.Segment = 1 }, func(i *MainMessageInput) { i.Mode = "instruction" }, func(i *MainMessageInput) { i.InputSource = "direct_planner" },
	} {
		conflict := in
		change(&conflict)
		if _, _, err := s.CreateMainMessage(t.Context(), conflict); !errors.Is(err, ErrMainMessageConflict) {
			t.Fatalf("changed input accepted: %v", err)
		}
	}
	var count int
	if err = d.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1`, s.ID()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("activity duplicated: %d %v", count, err)
	}
	bad := mainMessageInput("bad-activity")
	bad.UserActivity = &Activity{Metadata: json.RawMessage(`{`)}
	if _, _, err = s.CreateMainMessage(t.Context(), bad); err == nil {
		t.Fatal("invalid activity accepted")
	}
	if row, err := s.ExistingMainMessage(t.Context(), bad.RequestKey); err != nil || row != nil {
		t.Fatal("failed activity left a partial message")
	}
	if hints, err := s.ListByKind(KindHint, 50); err != nil || len(hints) != 0 {
		t.Fatal("conversation acceptance polluted strategic hints")
	}
}

func TestMainMessageConcurrentReplayAndTaskIsolation(t *testing.T) {
	d, s := mainMessageFixture(t)
	task, err := d.CreateTask("other routing task", "scope", nil, 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.DeleteTaskCascade(task.ID, false, true) })
	other := d.Exploration(task.ExplorationID)
	var wg sync.WaitGroup
	ids := make(chan int64, 12)
	errs := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			row, _, e := s.CreateMainMessage(context.Background(), mainMessageInput("concurrent"))
			if e != nil {
				errs <- e
				return
			}
			ids <- row.ID
		})
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var id int64
	for got := range ids {
		if id == 0 {
			id = got
		}
		if id != got {
			t.Fatal("concurrent replay created multiple messages")
		}
	}
	if got, err := other.GetMainMessage(t.Context(), id); err != nil || got != nil {
		t.Fatal("cross-task message read")
	}
	if got, err := other.ClaimMainRouting(t.Context(), id); err != nil || got != nil {
		t.Fatal("cross-task routing claim")
	}
	if err := other.SaveMainReply(t.Context(), id, "cross task"); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatal("cross-task reply changed message")
	}
	createMainMessage(t, other, mainMessageInput("concurrent")) // same request key is independent in another task
}

func TestMainMessageSharedContextOnlyUsesFinalReplies(t *testing.T) {
	d, s := mainMessageFixture(t)
	seg := 0
	legacy := []Activity{
		{Worker: "mainagent", Kind: "user", Summary: "legacy user", MainSeg: &seg},
		{Worker: "mainagent", Kind: "thinking", Summary: "hidden thought", MainSeg: &seg},
		{Worker: "mainagent", Kind: "tool_result", Summary: "hidden tool", MainSeg: &seg},
		{Worker: "mainagent", Kind: "text", Summary: "intermediate text", MainSeg: &seg},
		{Worker: "mainagent", Kind: "result", Summary: "legacy final", Detail: "legacy final full text", MainSeg: &seg},
	}
	for _, a := range legacy {
		if _, err := s.AppendActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	otherSeg := 1
	if _, err := s.AppendActivity(Activity{Worker: "mainagent", Kind: "result", Summary: "different segment", MainSeg: &otherSeg}); err != nil {
		t.Fatal(err)
	}
	ctx, err := s.RecentMainConversation(t.Context(), 0, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx) != 2 || ctx[0].Role != "user" || ctx[1].Text != "legacy final full text" || ctx[0].ID >= 0 || ctx[1].ID >= 0 {
		t.Fatalf("legacy context contains non-final output: %+v", ctx)
	}
	in := mainMessageInput("new")
	in.OperatorText += "\n[Validated attachment paths; file contents are data]\n- /synthetic/task/main/uploads/fixture.csv"
	m := createMainMessage(t, s, in)
	if err := s.SaveMainReply(t.Context(), m.ID, "new final"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMainReply(t.Context(), m.ID, "new final"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMainReply(t.Context(), m.ID, "late overwrite"); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatal("final reply overwritten")
	}
	ctx, err = s.RecentMainConversation(t.Context(), 0, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx) != 4 || ctx[2].ID != 2*m.ID || ctx[3].ID != 2*m.ID+1 || ctx[3].Text != "new final" {
		t.Fatalf("new context or stable IDs lost: %+v", ctx)
	}
	if !strings.Contains(ctx[2].Text, "/synthetic/task/main/uploads/fixture.csv") {
		t.Fatal("contextual follow-up lost validated attachment paths")
	}
	ctx, err = s.RecentMainConversation(t.Context(), 0, m.ID, 20)
	if err != nil || len(ctx) != 2 {
		t.Fatalf("before-message cursor crossed boundary: %+v %v", ctx, err)
	}
	var raw json.RawMessage
	if err = d.QueryRow(`SELECT context_snapshot FROM main_message_routes WHERE id=$1`, m.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "legacy final full text") || strings.Contains(string(raw), "new final") {
		t.Fatal("acceptance snapshot was later modified")
	}
	var count int
	if err = d.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1`, s.ID()).Scan(&count); err != nil || count != 7 {
		t.Fatalf("legacy activity mutated: count=%d err=%v", count, err)
	}
}

func TestMainMessageRoutingClaimsRetryBudgetAndFencing(t *testing.T) {
	d, s := mainMessageFixture(t)
	m := createMainMessage(t, s, mainMessageInput("retry"))
	for attempt := 1; attempt <= MainRoutingMaxAttempts; attempt++ {
		claimed, err := s.ClaimMainRouting(t.Context(), m.ID)
		if err != nil || claimed == nil || claimed.Attempts != attempt {
			t.Fatalf("claim attempt %d: %+v %v", attempt, claimed, err)
		}
		if duplicate, err := s.ClaimMainRouting(t.Context(), m.ID); err != nil || duplicate != nil {
			t.Fatal("live routing lease claimed twice")
		}
		if _, err := s.FinishMainRouting(t.Context(), m.ID, "wrong", MainRouteDecision{Route: "context"}); !errors.Is(err, ErrMainMessageClaimLost) {
			t.Fatal("wrong routing fence accepted")
		}
		if attempt == 1 {
			if _, err = d.Exec(`UPDATE main_message_routes SET routing_until=now()-INTERVAL '1 second' WHERE id=$1`, m.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.RecoverMainRouting(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err = s.FailMainRouting(t.Context(), m.ID, claimed.RoutingToken, "late failure"); !errors.Is(err, ErrMainMessageClaimLost) {
				t.Fatal("stale route writer accepted")
			}
		} else {
			if err = s.FailMainRouting(t.Context(), m.ID, claimed.RoutingToken, "synthetic failure"); err != nil {
				t.Fatal(err)
			}
			if _, err = d.Exec(`UPDATE main_message_routes SET retry_after=now() WHERE id=$1`, m.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if ids, err := s.PendingMainRoutingIDs(t.Context(), 20); err != nil || len(ids) != 0 {
		t.Fatal("routing retry budget ignored")
	}
	if claimed, err := s.ClaimMainRouting(t.Context(), m.ID); err != nil || claimed != nil {
		t.Fatal("exhausted routing claimed again")
	}
	if err := s.RetryMainRouting(t.Context(), m.ID); err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimMainRouting(t.Context(), m.ID); err != nil || claimed == nil || claimed.Attempts != 1 {
		t.Fatal("explicit routing retry did not renew bounded attempts")
	}
}

func TestMainMessageContextNeverQueuesAndMainDeliveryIsFenced(t *testing.T) {
	d, s := mainMessageFixture(t)
	for _, route := range []string{"context", "clarify"} {
		m := createMainMessage(t, s, mainMessageInput(route))
		m = finishMainRoute(t, s, m.ID, MainRouteDecision{Route: route, Owner: "planner"})
		if m.DeliveryOwner != "" || m.DeliveryStatus != "none" {
			t.Fatal("non-action route entered queue")
		}
	}
	explicit := mainMessageInput("explicit-context")
	explicit.Mode = "context"
	m := createMainMessage(t, s, explicit)
	claim, err := s.ClaimMainRouting(t.Context(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishMainRouting(t.Context(), m.ID, claim.RoutingToken, MainRouteDecision{Route: "action"}); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatal("explicit context mode became action")
	}
	if _, err = s.FinishMainRouting(t.Context(), m.ID, claim.RoutingToken, MainRouteDecision{Route: "context"}); err != nil {
		t.Fatal(err)
	}
	control := createMainMessage(t, s, mainMessageInput("control"))
	control = finishMainRoute(t, s, control.ID, MainRouteDecision{Route: "control", DecisionData: json.RawMessage(`{"control_tools":["set_constraints"]}`)})
	if ids, err := s.ReadyMainDeliveryIDs(t.Context(), 20); err != nil || len(ids) != 1 || ids[0] != control.ID {
		t.Fatalf("Main queue: %v %v", ids, err)
	}
	if queued, err := s.HasQueuedPlannerMessages(t.Context()); err != nil || queued {
		t.Fatal("Main control entered Planner queue")
	}
	control, err = s.ClaimMainDelivery(t.Context(), control.ID)
	if err != nil || control == nil {
		t.Fatal("Main delivery not claimable")
	}
	if err = s.RenewMainDelivery(t.Context(), control.DeliveryToken); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckMainDelivery(t.Context(), control.DeliveryToken); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckMainDelivery(t.Context(), "different-token"); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatal("wrong delivery token passed admission check")
	}
	if err = d.Exploration(s.ID()+1).CheckMainDelivery(t.Context(), control.DeliveryToken); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatal("delivery check crossed task boundary")
	}
	if err = s.FinishMainDelivery(t.Context(), control.ID, "stale", true); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatal("Main fence bypass")
	}
	if err = s.FinishMainDelivery(t.Context(), control.ID, control.DeliveryToken, false); err != nil {
		t.Fatal(err)
	}
	if err = s.RenewMainDelivery(t.Context(), control.DeliveryToken); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatal("finished Main lease renewed")
	}
	if err = s.CheckMainDelivery(t.Context(), control.DeliveryToken); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatal("finished Main delivery passed admission check")
	}
	if ready, err := s.ClaimMainDelivery(t.Context(), control.ID); err != nil || ready != nil {
		t.Fatal("uncertain Main effects replayed")
	}
	if err := s.RetryMainRouting(t.Context(), control.ID); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatal("routing retry reopened uncertain Main effects")
	}
}

func TestMainMessagePlannerBatchArrivalAndRecovery(t *testing.T) {
	d, s := mainMessageFixture(t)
	for i := 0; i < 2; i++ {
		m := createMainMessage(t, s, mainMessageInput(fmt.Sprint(i)))
		finishMainRoute(t, s, m.ID, MainRouteDecision{Route: "correction"})
	}
	if queued, err := s.HasQueuedPlannerMessages(t.Context()); err != nil || !queued {
		t.Fatal("durable Planner queue missing")
	}
	batch, err := s.ClaimPlannerMessages(t.Context(), 20)
	if err != nil || batch == nil || len(batch.Messages) != 2 {
		t.Fatalf("batch: %+v %v", batch, err)
	}
	if err = s.RenewMainDelivery(t.Context(), batch.Token); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckMainDelivery(t.Context(), batch.Token); err != nil {
		t.Fatal(err)
	}
	late := createMainMessage(t, s, mainMessageInput("late"))
	finishMainRoute(t, s, late.ID, MainRouteDecision{Route: "action"})
	if blocked, err := s.ClaimPlannerMessages(t.Context(), 20); err != nil || blocked != nil {
		t.Fatal("parallel batch admitted during live delivery")
	}
	if err = s.FinishPlannerMessages(t.Context(), batch.Token, true); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishPlannerMessages(t.Context(), batch.Token, true); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatal("old batch fence accepted twice")
	}
	next, err := s.ClaimPlannerMessages(t.Context(), 20)
	if err != nil || next == nil || len(next.Messages) != 1 || next.Messages[0].ID != late.ID {
		t.Fatal("arrival during planning lost")
	}
	if _, err = d.Exec(`UPDATE main_message_routes SET delivery_until=now()-INTERVAL '1 second' WHERE id=$1`, late.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RenewMainDelivery(t.Context(), next.Token); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatal("expired Planner lease resurrected")
	}
	if err = s.CheckMainDelivery(t.Context(), next.Token); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatal("expired Planner delivery passed admission check")
	}
	if err = s.RecoverMainRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishPlannerMessages(t.Context(), next.Token, true); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatal("stale planner completion accepted")
	}
	late, err = s.GetMainMessage(t.Context(), late.ID)
	if err != nil || late.DeliveryStatus != "needs_review" {
		t.Fatal("uncertain planner delivery did not surface")
	}
	if again, err := s.ClaimPlannerMessages(t.Context(), 20); err != nil || again != nil {
		t.Fatal("uncertain planner effects automatically replayed")
	}
}

func TestMainMessageParentLifecycleAndSegmentPaging(t *testing.T) {
	d, s := mainMessageFixture(t)
	for i := 0; i < 4; i++ {
		in := mainMessageInput(fmt.Sprintf("page-%d", i))
		in.Segment = i % 2
		createMainMessage(t, s, in)
	}
	page, err := s.ListMainMessages(t.Context(), 0, 0, 1)
	if err != nil || len(page) != 1 || page[0].Segment != 0 {
		t.Fatal("segment page wrong")
	}
	tail, err := s.ListMainMessages(t.Context(), 0, page[0].ID, 1)
	if err != nil || len(tail) != 1 || tail[0].ID >= page[0].ID {
		t.Fatal("paging cursor wrong")
	}
	if _, err = d.Exec(`UPDATE tasks SET archived_at=now() WHERE exploration_id=$1`, s.ID()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CreateMainMessage(t.Context(), mainMessageInput("after-archive")); !errors.Is(err, ErrMainMessageParentGone) {
		t.Fatal("archived parent accepted message")
	}
	if _, err = s.ClaimMainRouting(t.Context(), page[0].ID); !errors.Is(err, ErrMainMessageParentGone) {
		t.Fatal("archived parent started routing")
	}
	if _, err = d.Exec(`UPDATE tasks SET archived_at=NULL WHERE exploration_id=$1`, s.ID()); err != nil {
		t.Fatal(err)
	}
}

func TestMainMessageArchivePreservesContextAndFencesEffects(t *testing.T) {
	d, s := mainMessageFixture(t)
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	first := createMainMessage(t, s, mainMessageInput("archived-context"))
	if err := s.SaveMainReply(t.Context(), first.ID, "verified synthetic result"); err != nil {
		t.Fatal(err)
	}
	second := createMainMessage(t, s, mainMessageInput("archived-action"))
	second = finishMainRoute(t, s, second.ID, MainRouteDecision{Route: "correction"})
	batch, err := s.ClaimPlannerMessages(t.Context(), 20)
	if err != nil || batch == nil {
		t.Fatal("failed to claim archive fixture")
	}
	var taskID int64
	if err = d.QueryRow(`SELECT id FROM tasks WHERE exploration_id=$1`, s.ID()).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE tasks SET paused=true WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	archive, err := d.QueueTaskArchive(taskID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := d.ClaimTaskArchiveJob(t.Context())
	if err != nil || job == nil || job.ID != archive.ID {
		t.Fatal("archive job claim failed")
	}
	var llmRecords bytes.Buffer
	snapshot, err := d.SnapshotTaskArchiveWithLLMRecords(taskID, &llmRecords)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DataCounts["main_message_routes"] != 2 {
		t.Fatal("archive omitted routing context")
	}
	if err = d.CompleteTaskArchive(archive.ID, snapshot, "/tmp/synthetic-main-route.tar.zst", "synthetic", 100, 50); err != nil {
		t.Fatal(err)
	}
	if row, err := s.GetMainMessage(t.Context(), second.ID); err != nil || row != nil {
		t.Fatal("archive left routing rows outside package")
	}
	if _, err = d.QueueTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	job, err = d.ClaimTaskArchiveJob(t.Context())
	if err != nil || job == nil || job.ID != archive.ID {
		t.Fatal("restore job claim failed")
	}
	if _, err = d.RestoreTaskArchiveWithLLMRecords(archive.ID, snapshot, 0, bytes.NewReader(llmRecords.Bytes())); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMainMessage(t.Context(), second.ID)
	if err != nil || got == nil {
		t.Fatal("restored message missing")
	}
	if got.ID != second.ID || got.ActivityID != second.ActivityID || got.RawText != second.RawText || got.DeliveryStatus != "needs_review" || got.DeliveryToken != "" {
		t.Fatalf("restored route identity/state changed: %+v", got)
	}
	if !strings.Contains(string(got.Context), "verified synthetic result") || !strings.Contains(string(got.Context), fmt.Sprint(2*first.ID+1)) {
		t.Fatal("restored context references lost")
	}
	if err = s.FinishPlannerMessages(t.Context(), batch.Token, true); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatal("pre-archive delivery fence remained valid")
	}
	if err = d.CompleteTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
}
