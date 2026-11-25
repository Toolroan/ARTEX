package agent

import (
	"context"

	actool "github.com/Autumn-27/norma/tool"
)

func plannerInboxConstraintTool(ctx context.Context, ts *ToolSet) actool.CoreTool {
	batch, _ := ctx.Value(plannerInboxKey{}).(plannerInboxInput)
	for _, item := range batch.Instructions {
		if item.ID > 0 && item.Kind == "correction" && len(item.ControlTools) == 1 && item.ControlTools[0] == "set_constraints" {
			return ts.setConstraints()
		}
	}
	return nil
}

// Only a typed correction can grant this administrative operation to Planner.
// Canonicalize after augmentation/resolution, while retaining binding drops.
func enforcePlannerConstraintGrant(tools []actool.CoreTool, canonical actool.CoreTool) []actool.CoreTool {
	out := make([]actool.CoreTool, 0, len(tools))
	used := false
	for _, tool := range tools {
		if tool.Name() != "set_constraints" {
			out = append(out, tool)
			continue
		}
		if canonical != nil && !used {
			out = append(out, guardPanic(canonical))
			used = true
		}
	}
	return out
}
