package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	actool "github.com/Autumn-27/norma/tool"
)

// MainLifecycleControl is bound by the server to the current task and message.
// The model cannot supply a task ID or select a different lifecycle action.
// The current Main mutation fence must admit the operation before it is called.
type MainLifecycleControl func(context.Context, string) (json.RawMessage, error)
type mainLifecycleControlKey struct{}

func WithMainLifecycleControl(ctx context.Context, control MainLifecycleControl) context.Context {
	return context.WithValue(ctx, mainLifecycleControlKey{}, control)
}

func mainLifecycleToolName(name string) bool {
	return name == "wake_planner" || name == "continue_task"
}

func mainLifecycleTools(ctx context.Context) []actool.CoreTool {
	control, _ := ctx.Value(mainLifecycleControlKey{}).(MainLifecycleControl)
	if control == nil {
		return nil
	}
	makeTool := func(name, action, description string) actool.CoreTool {
		schema := obj(map[string]any{})
		schema["additionalProperties"] = false
		return writeTool(name, description, schema, func(callCtx context.Context, raw json.RawMessage) (actool.Result, error) {
			// Both tools have deliberately empty schemas. Reject hidden target IDs,
			// action substitutions, trailing input, and non-object JSON at runtime.
			input := bytes.TrimSpace(raw)
			if len(input) == 0 || input[0] != '{' {
				return actool.Errorf("This task control accepts only an empty object: {}."), nil
			}
			decoder := json.NewDecoder(bytes.NewReader(input))
			decoder.DisallowUnknownFields()
			var args struct{}
			if err := decoder.Decode(&args); err != nil {
				return actool.Errorf("This task control accepts only an empty object: {}."), nil
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				return actool.Errorf("This task control accepts only an empty object: {}."), nil
			}
			if err := callCtx.Err(); err != nil {
				return actool.Result{}, err
			}
			result, err := control(callCtx, action)
			if contextErr := callCtx.Err(); contextErr != nil {
				return actool.Result{}, contextErr
			}
			if err != nil {
				// Keep raw storage/provider errors out of the conversation. Lifecycle
				// blockers are returned in the server's structured result instead.
				return actool.Errorf("Task control could not be completed. Check get_instruction_status or the task controls; do not retry or claim Planner reviewed the instruction."), nil
			}
			if !json.Valid(result) || len(result) == 0 || bytes.TrimSpace(result)[0] != '{' {
				return actool.Errorf("Task control returned no verified receipt. Check task status before retrying."), nil
			}
			return actool.Text(string(result)), nil
		})
	}
	return []actool.CoreTool{
		makeTool("wake_planner", "wake", "Wake Planner for this task after an explicit operator request. This only signals an already admitted running task; it does not resume a paused or ended task, change scope, create work, or confirm that Planner reviewed anything. Call once with {} and report the server's actual receipt, including any blocker."),
		makeTool("continue_task", "restart", "Continue this ended task after an explicit operator request to continue or wake it. Admission preserves the concurrency queue and configured limits; it does not change goals, replay old worker intents, or confirm that Planner or workers applied any instruction. An unfinished user-paused task still requires Resume task; continuing an ended task clears any pause flag left by its previous run. Call once with {} and report the server's actual queued/running/blocked receipt."),
	}
}
