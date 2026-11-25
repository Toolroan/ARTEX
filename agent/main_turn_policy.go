package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

// MainTurnWriteFunc is the server's current lifecycle/ownership fence. It must
// run the supplied canonical operation only while its admission remains valid.
// Classification alone never grants a mutation. The callback can also record
// the resulting intent/administrative operation against the durable message.
type MainTurnWriteFunc func(context.Context, string, json.RawMessage, func() (actool.Result, error)) (actool.Result, error)

type MainTurnPolicy struct {
	Mode         string // readonly, control, work_request, main_oneoff; unknown modes fail closed
	ControlTools []string
	Execute      MainTurnWriteFunc
}

type mainTurnPolicyKey struct{}
type mainTurnDenialKey struct{}

type mainTurnDenial struct {
	mu           sync.Mutex
	execution    sync.Mutex
	name, reason string
}

func WithMainTurnPolicy(ctx context.Context, policy MainTurnPolicy) context.Context {
	policy.ControlTools = append([]string(nil), policy.ControlTools...)
	ctx = context.WithValue(ctx, mainTurnPolicyKey{}, policy)
	return context.WithValue(ctx, mainTurnDenialKey{}, &mainTurnDenial{})
}

func recordMainFenceDenial(ctx context.Context, name, reason string) {
	if state, ok := ctx.Value(mainTurnDenialKey{}).(*mainTurnDenial); ok {
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.name == "" {
			state.name, state.reason = name, reason
		}
	}
}

func readMainFenceDenial(ctx context.Context) (string, string) {
	if state, ok := ctx.Value(mainTurnDenialKey{}).(*mainTurnDenial); ok {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.name, state.reason
	}
	return "", ""
}

func mainAdministrativeTool(name string) bool {
	switch name {
	case "set_goals", "set_constraints", "add_task_scope", "add_company_scope", "wake_planner", "continue_task":
		return true
	}
	return false
}

func mainReadTool(name string) bool {
	switch name {
	case "graph_overview", "list_goals", "list_findings", "list_facts", "node_detail", "expand_digest", "get_worker_output", "get_worker_trace", "search_all_worker_traces", "list_assets", "list_untested_assets", "list_hints", "get_main_conversation", "list_artifacts", "get_instruction_status", "list_work_requests":
		return true
	}
	return false
}

type mainFencedTool struct {
	actool.CoreTool
	policy MainTurnPolicy
}

func (t mainFencedTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if state, ok := ctx.Value(mainTurnDenialKey{}).(*mainTurnDenial); ok {
		// Serialize this turn's writes so a sibling cannot pass the latch while
		// an earlier ownership/lifecycle refusal is still being determined.
		state.execution.Lock()
		defer state.execution.Unlock()
	}
	if name, _ := readMainFenceDenial(ctx); name != "" {
		return actool.Errorf("The server already declined a write in this Main turn. No further write will be attempted; check the request's current ownership and lifecycle."), nil
	}
	if reason := mainTurnWriteDenial(ctx, t.policy, t.Name(), input, true); reason != "" {
		return actool.Errorf(reason), nil
	}
	called := false
	result, err := t.policy.Execute(ctx, t.Name(), input, func() (actool.Result, error) {
		called = true
		return t.CoreTool.Call(ctx, input, tc)
	})
	if !called && (err != nil || result.IsError) {
		// A server ownership/lifecycle refusal is authoritative for this turn.
		// Canonical validation errors are different: they may be corrected and
		// retried, including an idempotent retry after an uncertain outcome.
		reason := "The server could not authorize this operation under the current ownership or task lifecycle."
		if err == nil && result.IsError {
			reason = result.Flatten()
		}
		recordMainFenceDenial(ctx, t.Name(), reason)
	}
	return result, err
}

// Shared by the advertised tool's handler and provider guard. During streaming,
// checkInput=false checks authority before inputs arrive; the buffered complete
// response is checked again with its actual request_id before any dispatch.
func mainTurnWriteDenial(ctx context.Context, policy MainTurnPolicy, name string, input json.RawMessage, checkInput bool) string {
	if !MainTurnAllowsWrite(policy, name) {
		if policy.Mode == "control" && mainWorkRequestWrite(name) {
			lifecycleOnly := true
			for _, control := range policy.ControlTools {
				if control == "set_goals" || control == "add_intent" {
					lifecycleOnly = false
				}
			}
			for _, control := range policy.ControlTools {
				if lifecycleOnly && mainLifecycleToolName(control) {
					return fmt.Sprintf("This request authorizes lifecycle continuation only, not %s. No goal or worker work was changed. An explicit goal-registration or bounded work instruction is required; the tool is not missing or broken.", name)
				}
			}
		}
		return fmt.Sprintf("%s is available, but this turn does not authorize that Main write. Inspect the current request and delivery status; do not retry or create replacement work for Planner-owned instructions.", name)
	}
	if policy.Execute == nil {
		return fmt.Sprintf("%s has no current server ownership fence. No work was changed; inspect the request status instead of retrying.", name)
	}
	if mainWorkRequestWrite(name) {
		if !checkInput {
			bound, _ := ctx.Value(workRequestsKey{}).(workRequestsContext)
			for _, grant := range bound.grants {
				if grant.RequestID > 0 && grant.Revision > 0 && grant.Owner == "main" && grant.DeliveryToken != "" {
					return ""
				}
			}
			return "No current Main-owned work request lease was granted. No work was changed; inspect existing request ownership instead of creating untracked work."
		}
		grant, err := selectWorkRequest(ctx, input)
		if err != nil {
			return err.Error()
		}
		if grant == nil {
			return "No current work request lease was granted to Main. No work was changed; inspect existing request ownership instead of creating untracked work."
		}
		if grant.Owner != "main" {
			return "Planner owns this work request. Main cannot create replacement work; inspect the existing linked nodes and delivery status."
		}
	}
	return ""
}

func mainWorkRequestWrite(name string) bool {
	return name == "set_goals" || name == "add_intent" || workRequestAuxTool(name)
}

func mainStableTool(name string) bool {
	return mainWorkRequestWrite(name) || name == "list_goals" || name == "list_work_requests"
}

// MainTurnAllowsWrite is the static operation check shared with the server's
// execution fence. A true result still requires a live delivery lease, lifecycle
// admission and, for goal/intent/request tools, a current work-request grant.
func MainTurnAllowsWrite(policy MainTurnPolicy, name string) bool {
	switch policy.Mode {
	case "work_request":
		return mainWorkRequestWrite(name)
	case "main_oneoff":
		return name == "add_intent"
	case "control":
		for _, control := range policy.ControlTools {
			if control == name && (mainAdministrativeTool(control) || control == "add_intent") {
				return true
			}
			if (control == "set_goals" || control == "add_intent") && workRequestAuxTool(name) {
				return true
			}
		}
	}
	return false
}

// These coordination schemas are code-owned on production Main turns. Older
// catalog bindings must not make a core tool disappear between turns. Write
// authority remains in the per-call policy and durable request lease.
func restoreMainStableTools(ctx context.Context, opts *agentcore.Options, trusted []actool.CoreTool) {
	if _, tagged := ctx.Value(mainTurnPolicyKey{}).(MainTurnPolicy); !tagged {
		return
	}
	present := make(map[string]bool, len(opts.Tools))
	for _, tool := range opts.Tools {
		present[tool.Name()] = true
	}
	for _, tool := range trusted {
		if mainStableTool(tool.Name()) && !present[tool.Name()] {
			opts.Tools = append(opts.Tools, guardPanic(tool))
			present[tool.Name()] = true
		}
	}
}

// Called after canonical role reconstruction: resolver additions cannot bypass
// this per-turn policy. An untagged internal/legacy caller keeps the existing
// capability set except lifecycle controls, which always require a tagged
// administrative owner. Every production chat is tagged by the server.
func enforceMainTurnPolicy(ctx context.Context, opts *agentcore.Options) {
	policy, tagged := ctx.Value(mainTurnPolicyKey{}).(MainTurnPolicy)
	if !tagged {
		allowed := make([]actool.CoreTool, 0, len(opts.Tools))
		for _, tool := range opts.Tools {
			if !mainLifecycleToolName(tool.Name()) && !workRequestAuxTool(tool.Name()) {
				allowed = append(allowed, tool)
			}
		}
		opts.Tools = allowed
		return
	}
	allowed := make([]actool.CoreTool, 0, len(opts.Tools))
	for _, tool := range opts.Tools {
		if mainReadTool(tool.Name()) {
			allowed = append(allowed, tool)
			continue
		}
		if mainWorkRequestWrite(tool.Name()) || (policy.Execute != nil && MainTurnAllowsWrite(policy, tool.Name())) {
			allowed = append(allowed, guardPanic(mainFencedTool{CoreTool: tool, policy: policy}))
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
	case "work_request":
		return "\n[Current message routing] Main owns preparation of this durable work request. Use set_goals for the requested verifiable acceptance criteria and add_intent for concrete execution steps, even when this task already has open goals. Reuse matching linked work; attach existing nodes instead of duplicating them. The server attaches new nodes to this request and hands the SAME request and IDs to Planner after Main finishes. Planner owns follow-through and corrections. Inspect observed evidence before resolving a request; never claim completion from dispatch or a worker return alone. Every write remains subject to the server's ownership, lifecycle and request-revision checks."
	case "control":
		return "\n[Current message routing] This turn is an explicit administrative request. Only the listed current operations may change state, subject to the server's ownership/lifecycle check. Goal/intent/request schemas remain visible, but need an explicitly assigned operation and current request lease. If a lifecycle control is assigned, call that tool alone first and inspect its receipt before separate goal/intent calls. When continuation includes goal registration or bounded dispatch, prepare only that authorized work under the same durable request; reuse existing linked nodes. A lifecycle-only request does not authorize new goals or worker work. Never infer a new instruction from conversation context."
	case "main_oneoff":
		return "\n[Current message routing] The server assigned this bounded one-off request to Main for worker dispatch. Check existing work and use add_intent only with this request's current Main lease; the server checks ownership and lifecycle at execution. A missing lease is an authority limitation, not a missing tool. Do not create goals, change scope, or dispatch duplicate work."
	default:
		return "\n[Current message routing] Main is read-only this turn. Goal/intent/request schemas remain visible so capabilities do not disappear, but their write handlers reject calls without current Main authority and a request lease. Earlier successful calls do not grant this turn permission. Do not call, retry, or promise later execution of an unauthorized write. The receipt distinguishes conversation context, clarification/pending routing, and an instruction owned by Planner. A queued instruction is saved in Planner's durable inbox; it does not depend on Planner discovering a hint in the conversation. Use list_goals, list_work_requests and get_instruction_status for current work and handoff progress before reporting delivery or delays. A receipt does not establish queue starvation, duplicate work, or worker application. Answer from verified evidence and ask the supplied clarification when needed. Do not add hints, dispatch workers, or modify goals/constraints/scope."
	}
}
