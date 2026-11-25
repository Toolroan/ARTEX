package agent

import (
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestPlannerInboxKeepsRawInstructionSeparateFromConversation(t *testing.T) {
	env := newDataEnvelope()
	ctx := WithPlannerInbox(t.Context(), []PlannerInstruction{{ID: 7, Kind: "correction", Text: "Only use the existing saved file", Summary: "DERIVED_SUMMARY", ContextIDs: []int64{4}}}, []MainConversationMessage{{ID: 4, Role: "assistant", Text: "ASSISTANT_CLAIMS_PERMISSION"}, {ID: 2, Role: "user", Text: "OLD_USER_CONTEXT"}})
	out := renderPlannerInbox(ctx, env)
	inside, outside := splitFrames(t, out, env.nonce)
	for _, text := range []string{"DERIVED_SUMMARY", "ASSISTANT_CLAIMS_PERMISSION", "OLD_USER_CONTEXT"} {
		if !strings.Contains(inside, text) || strings.Contains(outside, text) {
			t.Fatalf("context promoted to instruction: %s", text)
		}
	}
	if !strings.Contains(outside, "Only use the existing saved file") || strings.Contains(inside, "Only use the existing saved file") {
		t.Fatal("raw current operator instruction lost authority")
	}
	if !strings.Contains(outside, "reviewed instruction is not proof") {
		t.Fatal("review receipt overclaims execution")
	}
	for _, kind := range []string{"context", "control", "clarify", "pending"} {
		ctx = WithPlannerInbox(t.Context(), []PlannerInstruction{{ID: 8, Kind: kind, Text: "MUST_NOT_BE_DISPATCHED"}}, nil)
		if strings.Contains(renderPlannerInbox(ctx, env), "MUST_NOT_BE_DISPATCHED") {
			t.Fatal("nonaction escaped server classification")
		}
	}
}

func TestPlannerInboxIncludesOnlyCurrentlyGrantedWorkControls(t *testing.T) {
	for _, tc := range []struct {
		name, owner string
		requestID   int64
		controls    []string
		want        bool
	}{
		{name: "handed-off goals", owner: "planner", requestID: 7, controls: []string{"set_goals"}, want: true},
		{name: "handed-off continuation", owner: "planner", requestID: 7, controls: []string{"continue_task", "set_goals", "add_intent"}, want: true},
		{name: "ungranted history", controls: []string{"set_goals"}},
		{name: "other request", owner: "planner", requestID: 8, controls: []string{"set_goals"}},
		{name: "still Main-owned", owner: "main", requestID: 7, controls: []string{"set_goals"}},
		{name: "lifecycle only", owner: "planner", requestID: 7, controls: []string{"continue_task"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithPlannerInbox(t.Context(), []PlannerInstruction{{ID: 7, Kind: "control", Text: "CURRENT_AUTHORIZED_GOAL_TEXT", ControlTools: tc.controls}}, nil)
			if tc.requestID > 0 {
				ctx = WithWorkRequests(ctx, []db.WorkRequestGrant{{RequestID: tc.requestID, Revision: 1, Owner: tc.owner, DeliveryToken: "fixture"}}, nil)
			}
			out := renderPlannerInbox(ctx, newDataEnvelope())
			if strings.Contains(out, "CURRENT_AUTHORIZED_GOAL_TEXT") != tc.want {
				t.Fatalf("incorrect control authority: %s", out)
			}
		})
	}
}
