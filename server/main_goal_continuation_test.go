package server

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestContinueGoalRegistrationUsesSameDurableRequest(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	if err := s.m.SetTaskStatus(task.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	s.taskAgents = map[string]*taskAgentBundle{}
	s.provByProfile = map[int64]*provEntry{}
	s.llmOn = true
	s.llmCfg = agent.Config{Format: llm.FormatOpenAI, Model: "fixture", Stream: false}
	var mu sync.Mutex
	calls := 0
	s.llmProv = retestProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(req.Tools) == 0 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(`{"kind":"control","summary":"Continue and register the synthetic report goal","control_tools":["continue_task","set_goals"],"context_ids":[],"proposal_id":0,"requires_context":false,"clarification":"","one_off":false}`)}}, "end_turn", llm.Usage{}, nil
		}
		found := map[string]bool{}
		for _, tool := range req.Tools {
			found[tool.Name] = true
		}
		for _, name := range []string{"continue_task", "set_goals", "add_intent", "list_goals"} {
			if !found[name] {
				t.Errorf("stable/assigned tool missing: %s", name)
			}
		}
		calls++
		name, input := "", json.RawMessage(`{}`)
		switch calls {
		case 1:
			name = "continue_task"
		case 2, 3:
			// Retry the same goal in the same message: one persistent node only.
			name, input = "set_goals", json.RawMessage(`{"text":"Deliver the verified synthetic report"}`)
		case 4:
			// Visible does not authorize dispatch in a goal-only control request.
			name, input = "add_intent", json.RawMessage(`{"summary":"Unauthorized extra fixture step"}`)
		default:
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("Synthetic goal recorded and handed to Planner.")}}, "end_turn", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: name + string(rune('a'+calls)), Name: name, Input: input}}}, "tool_use", llm.Usage{}, nil
	}}
	w := plannerBridgeRequest(s, task, false, "Continue and register this goal: deliver the verified synthetic report")
	if w.Code != 202 {
		t.Fatalf("request: %d %s", w.Code, w.Body)
	}
	waitMainFixture(t, s, task)
	requests, err := task.Store.ListWorkRequests(t.Context(), 10)
	if err != nil || len(requests) != 1 || len(requests[0].Nodes) != 1 || requests[0].Nodes[0].Kind != "goal" || requests[0].Nodes[0].NodeID == 0 {
		t.Fatalf("registration not durable/idempotent: %+v %v", requests, err)
	}
	message, err := task.Store.GetMainMessage(t.Context(), requests[0].ID)
	if err != nil || message.DeliveryOwner != "planner" || message.DeliveryStatus != "queued" {
		t.Fatalf("same source not handed off: %+v %v", message, err)
	}
	intents, err := task.Store.ListByKind(db.KindIntent, 100)
	if err != nil || len(intents) != 0 {
		t.Fatalf("unassigned visible tool dispatched: %+v %v", intents, err)
	}
	if task.lifecycleSnapshot().Status != "running" {
		t.Fatal("task not continued")
	}
	if completed, err := s.m.SetTaskStatusGuarded(task.ID, "done", ""); err != nil || completed {
		t.Fatalf("unfinished request allowed completion: %v %v", completed, err)
	}
	batch, err := task.Store.ClaimPlannerMessages(t.Context(), 8)
	if err != nil || batch == nil || len(batch.Messages) != 1 || batch.Messages[0].ID != message.ID {
		t.Fatalf("Planner did not inherit source: %+v %v", batch, err)
	}
}

func TestMainContinuationWriteFenceChecksAdmissionAndToolAuthority(t *testing.T) {
	for _, blocked := range []string{"before_continue", "blocked_receipt", "paused", "queued", "terminal", "unavailable", "settling"} {
		t.Run(blocked, func(t *testing.T) {
			s, task := plannerBridgeFixture(t)
			m := mainLifecycleFixtureMessage(t, task, "compound", []string{"continue_task", "set_goals"})
			m, err := task.Store.ClaimMainDelivery(t.Context(), m.ID)
			if err != nil || m == nil {
				t.Fatal(err)
			}
			policy := s.mainMutationPolicy(task, m)
			if blocked != "before_continue" {
				result, err := policy.Execute(t.Context(), "continue_task", json.RawMessage(`{}`), func() (actool.Result, error) {
					receipt := taskControlResult{ID: task.ID, Action: "restart", Status: "running"}
					if blocked == "blocked_receipt" {
						receipt.BlockedReason = "Saved work could not be queued for review."
					}
					raw, _ := json.Marshal(receipt)
					return actool.Text(string(raw)), nil
				})
				if err != nil || result.IsError {
					t.Fatalf("continue fence: %+v %v", result, err)
				}
			}
			switch blocked {
			case "paused":
				task.updateLifecycle(func(st *taskLifecycleState) { st.Paused = true })
			case "queued":
				task.updateLifecycle(func(st *taskLifecycleState) { st.Queued = true })
			case "terminal":
				task.updateLifecycle(func(st *taskLifecycleState) { st.Status = "timeout" })
			case "unavailable":
				s.engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return nil, nil })
			case "settling":
				s.engine.settling.Store(task.ID, true)
			}
			called := false
			result, err := policy.Execute(t.Context(), "set_goals", json.RawMessage(`{}`), func() (actool.Result, error) { called = true; return actool.Text("unexpected"), nil })
			if err != nil || !result.IsError || called {
				t.Fatalf("blocked write executed: %+v %v", result, err)
			}
		})
	}
	s, task := plannerBridgeFixture(t)
	m := mainLifecycleFixtureMessage(t, task, "pure", []string{"continue_task"})
	m, err := task.Store.ClaimMainDelivery(t.Context(), m.ID)
	if err != nil || m == nil {
		t.Fatal(err)
	}
	for _, name := range []string{"set_goals", "add_intent", "set_constraints", "attach_work_request_node"} {
		called := false
		result, err := s.mainMutationPolicy(task, m).Execute(t.Context(), name, json.RawMessage(`{}`), func() (actool.Result, error) { called = true; return actool.Text("unexpected"), nil })
		if err != nil || !result.IsError || called {
			t.Fatalf("pure Continue authorized %s", name)
		}
	}
	requests, err := task.Store.ListWorkRequests(t.Context(), 10)
	if err != nil || len(requests) != 0 {
		t.Fatalf("pure Continue adopted work: %+v %v", requests, err)
	}
}

func TestMainPreparationWaitsForOrdinaryPlannerRound(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	m := makeMainOwnedFixture(t, task)
	task.planningMu.Lock()
	s.runMainMessage(t.Context(), task, m, "")
	task.planningMu.Unlock()
	stored, err := task.Store.GetMainMessage(t.Context(), m.ID)
	if err != nil || stored.DeliveryStatus != "queued" || stored.DeliveryToken != "" {
		t.Fatalf("Main overlapped Planner: %+v %v", stored, err)
	}
}
