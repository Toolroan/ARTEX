package agent

import (
	"context"

	actool "github.com/Autumn-27/norma/tool"
)

type plannerInstructionOnlyKey struct{}

// A live goalless task can still have an explicitly dispatched worker. This
// mode permits its typed correction without autonomous planning or completion.
func WithPlannerInstructionOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, plannerInstructionOnlyKey{}, true)
}

func plannerInstructionOnly(ctx context.Context) bool {
	enabled, _ := ctx.Value(plannerInstructionOnlyKey{}).(bool)
	return enabled
}

func plannerInstructionOnlyTools(tools []actool.CoreTool) []actool.CoreTool {
	out := make([]actool.CoreTool, 0, len(tools))
	for _, tool := range tools {
		if mainReadTool(tool.Name()) || tool.Name() == "list_goals" || tool.Name() == "list_worker_traces" || tool.Name() == "steer_work" || tool.Name() == "kill_work" || tool.Name() == "add_intent" || tool.Name() == "set_constraints" {
			out = append(out, tool)
		}
	}
	return out
}

const plannerInstructionOnlySystem = `You are Planner reviewing explicit typed operator instructions for an active task with no open goals. This is an instruction-only coordination round, not autonomous exploration. Inspect existing workers and apply only the current correction or bounded requested work. Never invent goals, broaden scope, mark the task complete, or dispatch because the frontier is empty. You may steer/stop affected workers or create a necessary corrected follow-up within the specific current request. Existing worker actions may still be in flight; queued steering is not proof it applied. Preserve lifecycle, budget and scope constraints.`

func plannerInstructionOnlyInput(situational, goal, constraints string) string {
	return "Review the current typed inbox instructions. Leave unrelated work alone; no action is required merely because old context was read.\n" + situational +
		"\nReport which instruction IDs were reviewed and the observed changes or reasons for no change. Do not judge task completion or create autonomous follow-up work." + plannerGoalAnchor(goal, constraints)
}
