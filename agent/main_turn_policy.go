package agent

import (
	"context"
	"encoding/json"

	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

// MainTurnWriteFunc is the server's current lifecycle/ownership fence. It must
// run the supplied canonical operation only while its admission remains valid.
// Classification alone never grants a mutation. The callback can also record
// the resulting intent/administrative operation against the durable message.
type MainTurnWriteFunc func(context.Context, string, json.RawMessage, func() (actool.Result, error)) (actool.Result, error)

type MainTurnPolicy struct {
	Mode         string // readonly, control, main_oneoff; unknown modes fail closed
	ControlTools []string
	Execute      MainTurnWriteFunc
}

type mainTurnPolicyKey struct{}

func WithMainTurnPolicy(ctx context.Context, policy MainTurnPolicy) context.Context {
	policy.ControlTools = append([]string(nil), policy.ControlTools...)
	return context.WithValue(ctx, mainTurnPolicyKey{}, policy)
}

func mainAdministrativeTool(name string) bool {
	switch name {
	case "set_goals", "set_constraints", "add_task_scope", "add_company_scope":
		return true
	}
	return false
}

func mainReadTool(name string) bool {
	switch name {
	case "graph_overview", "list_findings", "list_facts", "node_detail", "expand_digest", "get_worker_output", "get_worker_trace", "search_all_worker_traces", "list_assets", "list_untested_assets", "list_hints", "get_main_conversation":
		return true
	}
	return false
}

type mainFencedTool struct {
	actool.CoreTool
	execute MainTurnWriteFunc
}

func (t mainFencedTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return t.execute(ctx, t.Name(), input, func() (actool.Result, error) {
		return t.CoreTool.Call(ctx, input, tc)
	})
}

// Called after canonical role reconstruction: resolver additions cannot bypass
// this per-turn policy. An untagged internal/legacy caller keeps the existing
// capability set; every production chat is tagged by the server.
func enforceMainTurnPolicy(ctx context.Context, opts *agentcore.Options) {
	policy, tagged := ctx.Value(mainTurnPolicyKey{}).(MainTurnPolicy)
	if !tagged {
		return
	}
	controls := map[string]bool{}
	for _, name := range policy.ControlTools {
		if mainAdministrativeTool(name) {
			controls[name] = true
		}
	}
	allowed := make([]actool.CoreTool, 0, len(opts.Tools))
	for _, tool := range opts.Tools {
		if mainReadTool(tool.Name()) {
			allowed = append(allowed, tool)
			continue
		}
		if policy.Execute == nil {
			continue
		}
		if (policy.Mode == "control" && controls[tool.Name()]) || (policy.Mode == "main_oneoff" && tool.Name() == "add_intent") {
			allowed = append(allowed, guardPanic(mainFencedTool{CoreTool: tool, execute: policy.Execute}))
		}
	}
	opts.Tools = allowed // never nil: nil would reintroduce SDK defaults
}

func mainTurnPolicyReminder(ctx context.Context) string {
	policy, tagged := ctx.Value(mainTurnPolicyKey{}).(MainTurnPolicy)
	if !tagged {
		return ""
	}
	switch policy.Mode {
	case "control":
		return "\n[Current message routing] This turn is an explicit administrative request. Only the listed current administrative tools may change state, subject to the server's ownership/lifecycle check. Do not dispatch worker work or infer a new instruction from conversation context."
	case "main_oneoff":
		return "\n[Current message routing] The server assigned this bounded one-off request to Main for worker dispatch. Check existing work and use add_intent only within this request if it is present; the server checks ownership and lifecycle at execution. Do not create goals, change scope, or dispatch duplicate work."
	default:
		return "\n[Current message routing] Main is read-only this turn. The receipt distinguishes conversation context, clarification/pending routing, and an instruction owned by Planner. Saved context does not mean Planner was notified. Answer from evidence, ask the supplied clarification when needed, and report the exact receipt. Do not add hints, dispatch workers, or modify goals/constraints/scope."
	}
}
