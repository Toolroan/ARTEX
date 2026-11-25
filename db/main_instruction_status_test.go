package db

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestMainInstructionStatusesFindsActionsBeyondConversationPage(t *testing.T) {
	d, store := mainMessageFixture(t)
	action := createMainMessage(t, store, mainMessageInput("status-action"))
	action = finishMainRoute(t, store, action.ID, MainRouteDecision{Route: "action", Owner: "planner"})
	// Status lookup must not miss an older actionable row just because many
	// context-only turns came after it. These are all synthetic local fixtures.
	_, err := d.Exec(`INSERT INTO main_message_routes(exploration_id,main_seg,request_key,mode,input_source,raw_text,operator_text,route,routing_status) SELECT $1,0,'status-context-'||n,'context','main','DO NOT READ BODY','DO NOT READ BODY','context','routed' FROM generate_series(1,110) n`, store.ID())
	if err != nil {
		t.Fatal(err)
	}
	current := createMainMessage(t, store, mainMessageInput("status-current"))
	current = finishMainRoute(t, store, current.ID, MainRouteDecision{Route: "context"})
	_, err = d.Exec(`UPDATE main_message_routes SET assistant_text='DO NOT READ ASSISTANT',context_snapshot='[{"text":"DO NOT READ CONTEXT"}]',last_error='DO NOT READ ERROR' WHERE exploration_id=$1`, store.ID())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.MainInstructionStatuses(t.Context(), 0, current.ID, nil)
	if err != nil || len(rows) != 2 || rows[0].ID != current.ID || rows[1].ID != action.ID {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.RawText != "" || row.OperatorText != "" || row.AssistantText != "" || row.Context != nil || row.Error != "" || row.DeliveryToken != "" || row.RoutingToken != "" {
			t.Fatalf("metadata read loaded private content: %+v", row)
		}
	}
}

func TestMainInstructionStatusesAreBoundedAndRejectOtherScopes(t *testing.T) {
	d, store := mainMessageFixture(t)
	var current *MainMessage
	for i := range 23 {
		current = createMainMessage(t, store, mainMessageInput(fmt.Sprintf("status-action-%d", i)))
		current = finishMainRoute(t, store, current.ID, MainRouteDecision{Route: "action", Owner: "planner"})
	}
	rows, err := store.MainInstructionStatuses(t.Context(), 0, current.ID, nil)
	if err != nil || len(rows) != 20 || rows[0].ID != current.ID {
		t.Fatalf("unbounded or wrong current status: len=%d err=%v", len(rows), err)
	}
	otherSegmentInput := mainMessageInput("status-other-segment")
	otherSegmentInput.Segment = 1
	otherSegment := createMainMessage(t, store, otherSegmentInput)
	otherTask, err := d.CreateTask("status other task", "synthetic", nil, 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.DeleteTaskCascade(otherTask.ID, false, true) })
	other := createMainMessage(t, d.Exploration(otherTask.ExplorationID), mainMessageInput("status-other-task"))
	for _, ids := range [][]int64{{otherSegment.ID}, {other.ID}, {current.ID, other.ID}, {current.ID, current.ID}, {0}} {
		if _, err := store.MainInstructionStatuses(t.Context(), 0, current.ID, ids); err == nil {
			t.Fatalf("invalid or outside-scope IDs accepted: %v", ids)
		}
	}
	if _, err := store.MainInstructionStatuses(t.Context(), 1, current.ID, nil); err == nil {
		t.Fatal("default status accepted a current message from another segment")
	}
	rows, err = store.MainInstructionStatuses(t.Context(), 0, current.ID, []int64{current.ID})
	if err != nil || len(rows) != 1 || rows[0].DeliveryStatus != "queued" {
		t.Fatalf("valid exact lookup failed: %v %v", rows, err)
	}
	// A read must not claim, replay, or otherwise mutate a queued instruction.
	raw, _ := json.Marshal(rows[0])
	if rows[0].DeliveryUntil != nil || rows[0].DeliveryStatus != "queued" {
		t.Fatalf("read mutated delivery: %s", raw)
	}
}
