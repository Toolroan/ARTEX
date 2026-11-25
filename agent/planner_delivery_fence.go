package agent

import (
	"context"
	"encoding/json"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

type plannerDeliveryFenceKey struct{}

// The server checks the current durable inbox lease. Tool handlers check again
// after any target-approval wait, immediately before admitting the operation.
// An already-admitted operation may finish; this does not reset any run budget.
func WithPlannerDeliveryFence(ctx context.Context, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, plannerDeliveryFenceKey{}, check)
}

type plannerDeliveryFencedTool struct {
	actool.CoreTool
	check func(context.Context) error
}

const plannerDeliveryDenied = "Planner instruction delivery is no longer admitted; no new tool action was started. Review the delivery receipt before retrying."

func (t plannerDeliveryFencedTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if ctx.Err() != nil || t.check(ctx) != nil {
		return actool.Errorf(plannerDeliveryDenied), nil
	}
	return t.CoreTool.Call(ctx, input, tc)
}

func applyPlannerDeliveryFence(ctx context.Context, opts *agentcore.Options) {
	check, _ := ctx.Value(plannerDeliveryFenceKey{}).(func(context.Context) error)
	if check == nil {
		return
	}
	tools := make([]actool.CoreTool, 0, len(opts.Tools))
	for _, tool := range opts.Tools {
		tools = append(tools, guardPanic(plannerDeliveryFencedTool{CoreTool: tool, check: check}))
	}
	opts.Tools = tools
	opts.Hooks = plannerDeliveryHooks{inner: opts.Hooks, check: check}
}

// SDK-injected tools are registered after opts.Tools. Checking after the inner
// hook covers those too, including an approval wait that outlives the lease.
type plannerDeliveryHooks struct {
	inner harness.HookRunner
	check func(context.Context) error
}

func (h plannerDeliveryHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	var updated []byte
	if h.inner != nil {
		block, message, replacement := h.inner.PreToolUse(ctx, name, input)
		if block {
			return block, message, replacement
		}
		updated = replacement
	}
	if ctx.Err() != nil || h.check(ctx) != nil {
		return true, plannerDeliveryDenied, updated
	}
	return false, "", updated
}

func (h plannerDeliveryHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if h.inner != nil {
		h.inner.PostToolUse(ctx, name, input, result, isErr)
	}
}

func (h plannerDeliveryHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if h.inner != nil {
		return h.inner.Stop(ctx, messages)
	}
	return false, nil, ""
}
