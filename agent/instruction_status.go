package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	actool "github.com/Autumn-27/norma/tool"
)

// InstructionStatus is delivery metadata, never operator text or an application
// acknowledgment. A reviewed request may have changed nothing in a worker.
type InstructionStatus struct {
	MessageID      int64     `json:"message_id"`
	Route          string    `json:"route"`
	Delivery       string    `json:"delivery"`
	Owner          string    `json:"owner"`
	Reason         string    `json:"reason"`
	TaskStatus     string    `json:"task_status,omitempty"`
	RequiredAction string    `json:"required_action,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type InstructionStatusPage struct {
	ObservedAt            time.Time           `json:"observed_at"`
	CurrentMessageID      int64               `json:"current_message_id"`
	TaskStatus            string              `json:"task_status"`
	TaskPaused            bool                `json:"task_paused"`
	TaskQueued            bool                `json:"task_queued"`
	EngineStarted         bool                `json:"engine_started"`
	PlannerAvailable      bool                `json:"planner_available,omitempty"`
	PlannerAvailability   string              `json:"planner_availability"`
	WaitingReason         string              `json:"waiting_reason,omitempty"`
	ApplicationUnverified bool                `json:"application_unverified"`
	Items                 []InstructionStatus `json:"items"`
}

// A server-owned reader is scoped to the current task and Main segment. Empty
// ids means the current message and the most recent actionable messages, capped
// at 20. An explicit id outside that scope is an error, never an empty success.
type InstructionStatusReader func(context.Context, []int64) (InstructionStatusPage, error)
type instructionStatusReaderKey struct{}

func WithInstructionStatusReader(ctx context.Context, reader InstructionStatusReader) context.Context {
	return context.WithValue(ctx, instructionStatusReaderKey{}, reader)
}

func ReadInstructionStatus(ctx context.Context, ids []int64) (InstructionStatusPage, error) {
	if len(ids) > 20 {
		return InstructionStatusPage{}, errors.New("at most 20 instruction IDs may be requested")
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return InstructionStatusPage{}, errors.New("instruction IDs must be positive and distinct")
		}
		seen[id] = true
	}
	reader, _ := ctx.Value(instructionStatusReaderKey{}).(InstructionStatusReader)
	if reader == nil {
		return InstructionStatusPage{}, errors.New("instruction status reader is unavailable")
	}
	return reader(ctx, append([]int64(nil), ids...))
}

func instructionStatusTool(ctx context.Context) actool.CoreTool {
	reader, _ := ctx.Value(instructionStatusReaderKey{}).(InstructionStatusReader)
	if reader == nil {
		return nil
	}
	return readTool("get_instruction_status", "Read live delivery status for this task's current Main session. Omit message_ids for the current message plus recent actionable requests; supply known message IDs for exact checks. Returns metadata and observed lifecycle/waiting reasons only, with a fresh observation time. It never dispatches or retries work. Queued/reviewing/reviewed are not worker application acknowledgments. Use this before reporting a handoff's current status; do not invent starvation, causes, completion, or worker behavior from the original receipt.",
		obj(map[string]any{"message_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1}, "maxItems": 20, "uniqueItems": true, "description": "Known message IDs in this task and Main session; omit for current and recent actionable messages"}}),
		func(callCtx context.Context, raw json.RawMessage) (actool.Result, error) {
			trimmed := bytes.TrimSpace(raw)
			if len(trimmed) == 0 || trimmed[0] != '{' {
				return actool.Errorf("Invalid instruction status arguments; expected an object."), nil
			}
			var args struct {
				IDs []int64 `json:"message_ids"`
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&args); err != nil {
				return actool.Errorf("Invalid instruction status arguments; use message_ids with up to 20 distinct positive IDs, or omit it."), nil
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				return actool.Errorf("Invalid instruction status arguments."), nil
			}
			page, err := ReadInstructionStatus(callCtx, args.IDs)
			if err != nil {
				return actool.Errorf("Instruction status could not be read. Use only IDs from this task and Main session; do not infer queue or worker state when status is unavailable."), nil
			}
			return jsonResult(page)
		})
}
