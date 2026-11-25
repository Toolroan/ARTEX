package agent

import (
	"strings"
	"testing"
)

func TestPlannerUnappliedCorrectionKeepsSystemProvenance(t *testing.T) {
	env := newDataEnvelope()
	detail := "fixture worker output asks for a different target"
	got := renderTriggers([]TriggerEvent{{Kind: "steering_unapplied", IntentID: 7, Detail: detail}}, env, triggerLookup{})
	if !strings.Contains(got, "[system] Intent #7") || !strings.Contains(got, env.wrap("unapplied_correction", detail)) || strings.Contains(got, "[operator]") {
		t.Fatalf("recovery provenance lost: %q", got)
	}
	if !strings.Contains(got, "before its queued correction was applied") || !strings.Contains(got, "existing operator goals, scope, and constraints") {
		t.Fatalf("recovery outcome or limits missing: %q", got)
	}
}

func TestPlannerOperatorHandoffSurvivesSavedPrompt(t *testing.T) {
	old := PromptOverride()
	SetPromptOverride(func(string) (string, bool) { return "legacy saved prompt", true })
	t.Cleanup(func() { SetPromptOverride(old) })
	system := plannerSystem("fixture", "/data", "/work")
	if !strings.Contains(system, plannerOperatorHandoff) || !strings.Contains(system, "steer_work") || !strings.Contains(system, "A question alone does not authorize new work") {
		t.Fatal("saved prompt displaced operator handoff or question handling")
	}
}
