package server

import (
	"context"
	"errors"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Bind status reads to the server-selected task, Main segment, and current
// message. Tool arguments cannot select another conversation or task.
func (s *Server) instructionStatusReader(t *Task, segment int, currentID int64) agent.InstructionStatusReader {
	return func(ctx context.Context, ids []int64) (agent.InstructionStatusPage, error) {
		if s.engine.IsDeleting(t.ID) {
			return agent.InstructionStatusPage{}, errors.New("task is unavailable")
		}
		readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		messages, err := t.Store.MainInstructionStatuses(readCtx, segment, currentID, ids)
		if err != nil {
			return agent.InstructionStatusPage{}, err
		}
		openGoal, activeIntent, err := t.Store.MainInstructionEligibility(readCtx)
		if err != nil {
			return agent.InstructionStatusPage{}, err
		}
		pending, err := t.Store.HasQueuedPlannerMessages(readCtx)
		if err != nil {
			return agent.InstructionStatusPage{}, err
		}
		if err := readCtx.Err(); err != nil {
			return agent.InstructionStatusPage{}, err
		}
		state := t.lifecycleSnapshot()
		paused := state.Paused || (!state.Queued && s.engine.IsPaused(t.ID))
		started := s.engine.Started(t.ID)
		settling := s.engine.isSettling(t.ID) || s.engine.IsDeleting(t.ID)
		plannerWait, mainWait := instructionStatusWaitingReasons(state, paused, started, settling, openGoal, activeIntent, pending)
		page := agent.InstructionStatusPage{
			ObservedAt: time.Now().UTC(), CurrentMessageID: currentID,
			TaskStatus: state.Status, TaskPaused: paused, TaskQueued: state.Queued,
			EngineStarted: started, PlannerAvailability: "unknown",
			WaitingReason: plannerWait, ApplicationUnverified: true,
			Items: make([]agent.InstructionStatus, 0, len(messages)),
		}
		// Do not call snapshotFor/taskRuntimeAvailable or even take taskAgentMu:
		// provider resolution and bundle construction perform contextless DB reads.
		// A cached bundle is not proof of current model availability, either. The
		// delivery row remains authoritative for queued/reviewing/reviewed status.
		for i := range messages {
			msg := &messages[i]
			receipt := instructionStatusReceipt(msg, page.ObservedAt, plannerWait, mainWait)
			if msg.DeliveryOwner == "planner" && (receipt.Delivery == "queued" || receipt.Delivery == "waiting") {
				applyInstructionBlocker(&receipt, state.Status, state.Paused)
			}
			page.Items = append(page.Items, agent.InstructionStatus{
				MessageID: msg.ID, Route: receipt.Route, Delivery: receipt.Delivery, Owner: msg.DeliveryOwner,
				Reason: receipt.Reason, CreatedAt: msg.CreatedAt.UTC(), UpdatedAt: msg.UpdatedAt.UTC(),
				TaskStatus: receipt.TaskStatus, RequiredAction: receipt.RequiredAction,
			})
		}
		if err := readCtx.Err(); err != nil {
			return agent.InstructionStatusPage{}, err
		}
		return page, nil
	}
}

// All inputs are already observed metadata. This conversion intentionally does
// no I/O, so one response cannot repeat contextless eligibility queries for each
// queued item. Provider availability remains explicitly unknown.
func instructionStatusWaitingReasons(state taskLifecycleState, paused, started, settling, openGoal, activeIntent, pending bool) (planner, main string) {
	switch {
	case state.Queued:
		main = "Task is queued; request saved until it starts."
	case paused:
		main = "Task is paused; request saved until it is resumed."
	case settling:
		main = "Task is winding down; request remains saved."
	}
	switch {
	case isTerminalStatus(state.Status):
		planner = terminalInstructionReason(state.Status)
	case state.Queued:
		planner = "Task is queued; waiting until it starts."
	case paused:
		planner = "Task is paused; waiting until it is resumed."
	case settling:
		planner = "Task is winding down; instruction remains saved."
	case !openGoal && !activeIntent && !pending:
		planner = "Task has no open work or pending Planner instructions."
	case !started:
		planner = "Task engine is not running; instruction remains saved."
	}
	return
}

func instructionStatusReceipt(msg *db.MainMessage, observedAt time.Time, plannerWait, mainWait string) mainMessageReceipt {
	r := mainMessageReceipt{MessageID: msg.ID, Route: msg.Route}
	switch {
	case msg.RoutingStatus == "failed":
		r.Delivery, r.Reason = "routing_failed", "Message saved, but routing failed. Retry routing or send an explicit Planner instruction."
	case msg.RoutingStatus != "routed":
		r.Delivery, r.Reason = "routing", "Message saved; checking whether it needs action."
	case msg.Route == "context":
		r.Delivery, r.Reason = "context", "Conversation context only; Planner was not notified."
	case msg.Route == "clarify":
		r.Delivery, r.Reason = "clarify", "This message needs clarification before work can be queued."
	case msg.DeliveryStatus == "needs_review":
		r.Delivery, r.Reason = "needs_review", "Processing was interrupted. Check existing effects before sending another instruction."
	case msg.DeliveryStatus == "inflight" && (msg.DeliveryUntil == nil || !msg.DeliveryUntil.After(observedAt)):
		r.Delivery, r.Reason = "needs_review", "The delivery lease expired or is unavailable. Processing is uncertain; check existing effects before sending another instruction."
	case msg.DeliveryStatus == "presented":
		r.Delivery, r.Reason = "presented", "Reviewed; this is not confirmation that a worker applied it."
	case msg.DeliveryOwner == "main":
		r.Delivery, r.Reason = "main", "This request is assigned to Main; execution has not been confirmed by this receipt."
		if mainWait != "" {
			r.Delivery, r.Reason = "waiting", mainWait
		}
	case msg.DeliveryStatus == "inflight":
		r.Delivery, r.Reason = "reviewing", "Planner is reviewing this instruction."
	default:
		r.Delivery, r.Reason = "queued", "Instruction queued for Planner. Current model availability was not checked."
		if plannerWait != "" {
			r.Delivery, r.Reason = "waiting", plannerWait
		}
	}
	return r
}
