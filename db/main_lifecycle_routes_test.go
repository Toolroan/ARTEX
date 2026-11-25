package db

import (
	"encoding/json"
	"testing"
)

func TestReadyMainLifecycleDeliveryIDsSelectsUnattemptedLifecycleAndPlanningControls(t *testing.T) {
	_, s := mainMessageFixture(t)
	add := func(key string, tools []string) *MainMessage {
		m := createMainMessage(t, s, mainMessageInput(key))
		data, _ := json.Marshal(map[string]any{"kind": "control", "control_tools": tools})
		return finishMainRoute(t, s, m.ID, MainRouteDecision{Route: "control", Owner: "main", DecisionData: data})
	}
	add("ordinary-before", []string{"set_goals"})
	add("mixed-invalid", []string{"continue_task", "set_constraints"})
	add("two-lifecycle-invalid", []string{"continue_task", "wake_planner", "set_goals"})
	add("duplicate-invalid", []string{"continue_task", "set_goals", "set_goals"})
	add("intent-only-invalid", []string{"continue_task", "add_intent"})
	compound := add("compound", []string{"set_goals", "continue_task", "add_intent"})
	continued := add("continue", []string{"continue_task"})
	wake := add("wake", []string{"wake_planner"})
	ids, err := s.ReadyMainLifecycleDeliveryIDs(t.Context(), 1)
	if err != nil || len(ids) != 1 || ids[0] != compound.ID {
		t.Fatalf("lifecycle blocked behind ordinary work: %v %v", ids, err)
	}
	claimed, err := s.ClaimMainDelivery(t.Context(), compound.ID)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.FinishMainDelivery(t.Context(), claimed.ID, claimed.DeliveryToken, false); err != nil {
		t.Fatal(err)
	}
	ids, err = s.ReadyMainLifecycleDeliveryIDs(t.Context(), 20)
	if err != nil || len(ids) != 2 || ids[0] != continued.ID || ids[1] != wake.ID {
		t.Fatalf("uncertain/mixed controls reselected: %v %v", ids, err)
	}
}
