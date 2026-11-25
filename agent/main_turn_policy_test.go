package agent

import (
	"context"
	"encoding/json"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestMainTurnReadOnlyRejectsActualModelMutations(t *testing.T) {
	old := ToolResolve()
	defer SetToolResolve(old)
	var replacements atomic.Int64
	SetToolResolve(func(_ context.Context, _ string, in []actool.CoreTool) []actool.CoreTool {
		return append(in, mainRoleProbe("add_intent", &replacements), mainRoleProbe("Bash", &replacements))
	})
	for _, mode := range []string{"readonly", "context", "action", "correction", "clarify", "pending", "unrouted"} {
		t.Run(mode, func(t *testing.T) {
			p := &mainRoleProvider{}
			for _, name := range []string{"add_hint", "add_intent", "set_goals", "set_constraints", "add_task_scope", "add_company_scope", "Bash"} {
				p.calls = append(p.calls, llm.ContentBlock{Name: name, Input: json.RawMessage(`{}`)})
			}
			ctx := WithMainTurnPolicy(t.Context(), MainTurnPolicy{Mode: mode, ControlTools: []string{"set_goals"}, Execute: func(context.Context, string, json.RawMessage, func() (actool.Result, error)) (actool.Result, error) {
				t.Fatal("read-only turn reached a write fence")
				return actool.Text(""), nil
			}})
			main := NewMainAgent(p, "fixture", t.TempDir(), nil, 0, 3)
			if _, err := main.Chat(ctx, 0, 0, nil, &db.ExplorationStore{}, "fixture", "What's the status?", passHooks{}, nil, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if len(p.reqs) != 2 {
				t.Fatalf("requests=%d", len(p.reqs))
			}
			for _, tool := range p.reqs[0].Tools {
				if !mainReadTool(tool.Name) {
					t.Fatalf("write exposed: %s", tool.Name)
				}
			}
			for _, message := range p.reqs[1].Messages {
				for _, block := range message.Content {
					if block.Type == llm.BlockToolResult && !block.IsError {
						t.Fatal("model mutation succeeded")
					}
				}
			}
		})
	}
	if replacements.Load() != 0 {
		t.Fatal("resolver bypassed policy")
	}
}

func TestMainTurnMutationRequiresCorrectOwnerAndLiveFence(t *testing.T) {
	for _, mode := range []string{"control", "main_oneoff"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			trusted := []actool.CoreTool{mainRoleProbe("graph_overview", &calls), mainRoleProbe("set_constraints", &calls), mainRoleProbe("set_goals", &calls), mainRoleProbe("add_intent", &calls), mainRoleProbe("add_hint", &calls)}
			allow := false
			policy := MainTurnPolicy{Mode: mode, ControlTools: []string{"set_constraints", "Bash"}, Execute: func(_ context.Context, name string, _ json.RawMessage, run func() (actool.Result, error)) (actool.Result, error) {
				if !allow {
					return actool.Errorf("task is now paused"), nil
				}
				return run()
			}}
			opts := agentcore.Options{Tools: trusted}
			enforceMainAgentRole(&opts, trusted)
			enforceMainTurnPolicy(WithMainTurnPolicy(t.Context(), policy), &opts)
			want := "set_constraints"
			if mode == "main_oneoff" {
				want = "add_intent"
			}
			var mutation actool.CoreTool
			for _, tool := range opts.Tools {
				if tool.Name() == want {
					mutation = tool
				} else if tool.Name() != "graph_overview" {
					t.Fatalf("unexpected tool %s", tool.Name())
				}
			}
			if mutation == nil {
				t.Fatal("authorized tool absent")
			}
			res, err := mutation.Call(t.Context(), json.RawMessage(`{}`), nil)
			if err != nil || !res.IsError || calls.Load() != 0 {
				t.Fatal("stale lifecycle fence failed")
			}
			allow = true
			res, err = mutation.Call(t.Context(), json.RawMessage(`{}`), nil)
			if err != nil || res.IsError || calls.Load() != 1 {
				t.Fatal("admitted canonical operation did not run")
			}
			policy.Execute = nil
			opts.Tools = trusted
			enforceMainTurnPolicy(WithMainTurnPolicy(t.Context(), policy), &opts)
			if names := func() []string {
				var n []string
				for _, x := range opts.Tools {
					n = append(n, x.Name())
				}
				return n
			}(); !slices.Equal(names, []string{"graph_overview"}) {
				t.Fatalf("no fence still has writes: %v", names)
			}
		})
	}
}
