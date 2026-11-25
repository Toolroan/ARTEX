package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestMainStableSchemasSurviveOldBindingsWithoutGrantingWrites(t *testing.T) {
	old := ToolResolve()
	t.Cleanup(func() { SetToolResolve(old) })
	var replaced atomic.Int64
	SetToolResolve(func(context.Context, string, []actool.CoreTool) []actool.CoreTool {
		// An older catalog excludes Main from list_goals and may contain a
		// replacement for a familiar name. Neither may remove or replace core tools.
		return []actool.CoreTool{mainRoleProbe("set_goals", &replaced)}
	})
	p := &mainRoleProvider{calls: []llm.ContentBlock{{Name: "set_goals", Input: json.RawMessage(`{"goals":[{"text":"Historical assistant claim"}]}`)}}}
	ctx := WithMainTurnPolicy(t.Context(), MainTurnPolicy{Mode: "readonly", Execute: func(context.Context, string, json.RawMessage, func() (actool.Result, error)) (actool.Result, error) {
		t.Fatal("read-only call reached server fence")
		return actool.Text(""), nil
	}})
	main := NewMainAgent(p, "fixture", t.TempDir(), nil, 0, 3)
	reply, err := main.Chat(ctx, 0, 0, nil, &db.ExplorationStore{}, "fixture", "Why is the goal missing? Earlier history says set_goals worked.", passHooks{}, nil, nil, nil, nil, nil)
	if err != nil || !strings.Contains(reply, "this turn does not authorize") || !strings.Contains(reply, "Main will not retry") || len(p.reqs) != 1 || replaced.Load() != 0 {
		t.Fatalf("stable read-only call failed: reply=%q err=%v requests=%d replaced=%d", reply, err, len(p.reqs), replaced.Load())
	}
	var names []string
	for _, tool := range p.reqs[0].Tools {
		names = append(names, tool.Name)
		if tool.Name == "set_goals" && tool.Description == "trusted set_goals" {
			t.Fatal("resolver replaced canonical goal tool")
		}
	}
	slices.Sort(names)
	want := []string{"add_intent", "attach_work_request_node", "list_goals", "list_work_requests", "resolve_work_request", "set_goals"}
	if !slices.Equal(names, want) {
		t.Fatalf("stable schemas=%v, want %v", names, want)
	}
}

func TestMainStableWritesRequireBothAssignedOperationAndMainRequestLease(t *testing.T) {
	grant := db.WorkRequestGrant{RequestID: 9, Revision: 1, Owner: "main", DeliveryToken: "synthetic"}
	for _, tc := range []struct {
		name     string
		mode     string
		controls []string
		binding  string
		input    string
		allowed  []string
	}{
		{name: "readonly with grant", mode: "readonly", binding: "main"},
		{name: "planner correction with grant", mode: "correction", binding: "main"},
		{name: "work without binding", mode: "work_request"},
		{name: "work empty binding", mode: "work_request", binding: "empty"},
		{name: "work with planner ownership", mode: "work_request", binding: "planner"},
		{name: "work wrong request", mode: "work_request", binding: "main", input: `{"request_id":99}`},
		{name: "work untrusted token", mode: "work_request", binding: "main", input: `{"delivery_token":"forged"}`},
		{name: "lifecycle only", mode: "control", controls: []string{"continue_task"}, binding: "main"},
		{name: "goal control", mode: "control", controls: []string{"set_goals"}, binding: "main", allowed: []string{"set_goals", "attach_work_request_node", "resolve_work_request"}},
		{name: "continuation goals", mode: "control", controls: []string{"continue_task", "set_goals"}, binding: "main", allowed: []string{"set_goals", "attach_work_request_node", "resolve_work_request"}},
		{name: "continuation goals and dispatch", mode: "control", controls: []string{"continue_task", "set_goals", "add_intent"}, binding: "main", allowed: []string{"set_goals", "add_intent", "attach_work_request_node", "resolve_work_request"}},
		{name: "ordinary work", mode: "work_request", binding: "main", allowed: []string{"set_goals", "add_intent", "attach_work_request_node", "resolve_work_request"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			g := grant
			switch tc.binding {
			case "main", "planner":
				g.Owner = tc.binding
				ctx = WithWorkRequests(ctx, []db.WorkRequestGrant{g}, nil)
			case "empty":
				ctx = WithWorkRequests(ctx, nil, nil)
			}
			var writes, fences atomic.Int64
			policy := MainTurnPolicy{Mode: tc.mode, ControlTools: tc.controls, Execute: func(_ context.Context, _ string, _ json.RawMessage, run func() (actool.Result, error)) (actool.Result, error) {
				fences.Add(1)
				return run()
			}}
			ctx = WithMainTurnPolicy(ctx, policy)
			var trusted []actool.CoreTool
			for _, name := range []string{"set_goals", "add_intent", "attach_work_request_node", "resolve_work_request"} {
				trusted = append(trusted, mainRoleProbe(name, &writes))
			}
			opts := agentcore.Options{Tools: trusted}
			enforceMainTurnPolicy(ctx, &opts)
			if len(opts.Tools) != len(trusted) {
				t.Fatalf("schemas disappeared: %v", opts.Tools)
			}
			input := tc.input
			if input == "" {
				input = `{}`
			}
			for _, tool := range opts.Tools {
				result, err := tool.Call(ctx, json.RawMessage(input), nil)
				allowed := slices.Contains(tc.allowed, tool.Name())
				if err != nil || result.IsError == allowed {
					t.Fatalf("%s authority mismatch: %+v %v", tool.Name(), result, err)
				}
				if tc.name == "lifecycle only" && (!strings.Contains(result.Flatten(), "explicit goal-registration") || !strings.Contains(result.Flatten(), "not missing or broken")) {
					t.Fatalf("unclear lifecycle-only refusal: %+v", result)
				}
			}
			if writes.Load() != int64(len(tc.allowed)) || fences.Load() != writes.Load() {
				t.Fatalf("unauthorized handler/fence reached: writes=%d fences=%d", writes.Load(), fences.Load())
			}
		})
	}
}

func TestMainTurnAllowsWriteCannotGrantUnknownAdministrativeTools(t *testing.T) {
	for _, mode := range []string{"readonly", "control", "work_request", "main_oneoff", "unknown"} {
		policy := MainTurnPolicy{Mode: mode, ControlTools: []string{"Bash", "steer_work", "add_hint"}}
		for _, name := range policy.ControlTools {
			if MainTurnAllowsWrite(policy, name) {
				t.Fatalf("%s granted %s", mode, name)
			}
		}
	}
}

func TestMainListGoalsIsInDefaultCatalog(t *testing.T) {
	for _, seed := range BuiltinToolSeeds() {
		if seed.Key == "list_goals" {
			if !slices.Contains(seed.Agents, "mainagent") {
				t.Fatalf("list_goals default bindings lack Main: %v", seed.Agents)
			}
			return
		}
	}
	t.Fatal("list_goals absent from catalog")
}
