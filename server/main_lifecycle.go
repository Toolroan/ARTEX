package server

import (
	"encoding/json"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// One explicit lifecycle operation may accompany current goal registration.
// Unrelated controls and malformed decisions never authorize continuation.
func mainLifecycleTool(msg *db.MainMessage) string {
	if msg == nil || msg.Route != "control" || msg.RoutingStatus != "routed" || msg.DeliveryOwner != "main" {
		return ""
	}
	var decision agent.MainMessageRoute
	if json.Unmarshal(msg.DecisionData, &decision) != nil || decision.Kind != "control" {
		return ""
	}
	lifecycle := ""
	seen := map[string]bool{}
	for _, name := range decision.ControlTools {
		if seen[name] {
			return ""
		}
		seen[name] = true
		switch name {
		case "wake_planner", "continue_task":
			if lifecycle != "" {
				return ""
			}
			lifecycle = name
		case "set_goals", "add_intent":
		default:
			return ""
		}
	}
	if seen["add_intent"] && !seen["set_goals"] {
		return ""
	}
	return lifecycle
}

func (s *Server) mainMessageWaitingReason(t *Task, msg *db.MainMessage) string {
	if mainLifecycleTool(msg) != "" && !s.engine.IsDeleting(t.ID) {
		// Let the specific canonical operation explain a blocked state. In
		// particular a timed-out task must not block its own Continue command.
		return ""
	}
	return s.mainWaitingReason(t)
}

func (s *Server) mainRoutingLifecycle(t *Task) string {
	state := t.lifecycleSnapshot()
	if isTerminalStatus(state.Status) {
		return state.Status
	}
	if state.Queued {
		return "queued"
	}
	if state.Paused || s.engine.IsPaused(t.ID) {
		return "paused"
	}
	return state.Status
}

func terminalInstructionReason(status string) string {
	switch status {
	case "timeout":
		return "Task reached its time or run limit. Instruction saved; Planner cannot run until you Continue task."
	case "failed":
		return "Task failed. Instruction saved; Planner cannot run until you Continue task."
	default:
		return "Task completed. Instruction saved; Planner cannot run until you Continue task."
	}
}

func applyInstructionBlocker(receipt *mainMessageReceipt, status string, paused bool) {
	if isTerminalStatus(status) {
		receipt.Delivery, receipt.Reason = "blocked", terminalInstructionReason(status)
		receipt.TaskStatus, receipt.RequiredAction = status, "restart"
	} else if paused {
		receipt.TaskStatus, receipt.RequiredAction = "paused", "resume"
	}
}
