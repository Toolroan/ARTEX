package server

import "fmt"

// applyTaskLifecycleControl serves explicit operator wake/restart requests.
// Ordinary chat routing never calls it. The locked form also serves Main's
// ownership fence, which already holds concMu; neither form holds mutationMu.
func (s *Server) applyTaskLifecycleControl(t *Task, action string) (taskControlResult, error) {
	s.concMu.Lock()
	defer s.concMu.Unlock()
	return s.applyTaskLifecycleControlLocked(t, action)
}

// Caller must hold concMu. Restart continues the existing task and its durable
// inbox through normal admission; it does not reset/replay old intents, reopen
// goals, or grant a fresh token/turn budget. Timeout admission renews only the
// configured wall-clock allowance. A repeated command while running/queued is
// a no-op, so it cannot repeatedly reset that allowance or disturb FIFO order.
// Explicit continuation of an ended task also clears a prior user pause through
// normal admission. A still-running paused task requires the separate Resume.
func (s *Server) applyTaskLifecycleControlLocked(t *Task, action string) (taskControlResult, error) {
	out := taskControlResult{Action: action}
	blocked := func(reason string) (taskControlResult, error) {
		out.BlockedReason = reason
		return out, fmt.Errorf("%s", reason)
	}
	if t == nil {
		return blocked("Task was not found.")
	}
	out.ID = t.ID
	state := t.lifecycleSnapshot()
	terminal := isTerminalStatus(state.Status)
	out.Paused, out.Queued, out.Status = state.Paused, state.Queued, state.Status
	if state.Paused && !terminal {
		out.Status = "paused"
	} else if state.Queued && !terminal {
		out.Status = "queued"
	}
	current, exists := s.m.Task(t.ID)
	if !exists || current != t || s.engine.IsDeleting(t.ID) {
		return blocked("Task is being deleted; lifecycle control is unavailable.")
	}
	if action != "wake" && action != "restart" {
		return blocked("Action must be wake or restart.")
	}
	if !s.engine.beginTaskOperation(t.ID) {
		return blocked("Task is being deleted or the server is shutting down.")
	}
	defer s.engine.decInflight(t.ID)
	if state.Paused && !terminal {
		return blocked("Task is paused; use Resume to continue it explicitly.")
	}
	if !terminal && s.engine.isSettling(t.ID) {
		return blocked("Task is winding down; wait for settlement to finish.")
	}
	if action == "wake" {
		switch {
		case terminal:
			return blocked("Task has ended; use Continue task to start another eligible run.")
		case state.Queued:
			return blocked("Task is queued; it will start when admitted in queue order.")
		case s.engine.IsPaused(t.ID):
			return blocked("Task execution is paused; no Planner wake was sent.")
		case !s.engine.Started(t.ID):
			return blocked("Task engine has not started; no Planner wake was sent.")
		case !s.engine.ReadyFor(t):
			return blocked("No runnable Planner/Worker model is configured; no Planner wake was sent.")
		case s.engine.budgetBlocksClaim(t.ID):
			return blocked("Task budget does not allow further worker dispatch; waking Planner does not reset recorded spend.")
		}
		// Notify cannot bypass the Planner's lifecycle/budget gates or create an
		// intent. This is a wake signal, not proof that an inbox item was reviewed.
		t.Notify()
		out.WakeSent = true
		return out, nil
	}
	if !terminal {
		if !state.Queued && s.engine.IsPaused(t.ID) {
			return blocked("Task execution is paused; use Resume instead of restarting it.")
		}
		out.AlreadyActive = true
		if !state.Queued && !s.engine.ReadyFor(t) {
			out.BlockedReason = "No runnable Planner/Worker model is configured."
		} else if !state.Queued && !s.engine.Started(t.ID) {
			out.BlockedReason = "Task engine has not started."
		} else if s.engine.budgetBlocksClaim(t.ID) {
			out.BlockedReason = "Task budget does not allow further worker dispatch; continuing does not reset recorded spend."
		}
		return out, nil
	}
	queued, err := s.admitTaskWhenLocked(t, "resume", false)
	if err != nil {
		return blocked("Task admission could not be completed. Check the current task status before retrying.")
	}
	state = t.lifecycleSnapshot()
	out.Paused, out.Queued, out.Status = state.Paused, queued, state.Status
	if queued {
		out.Status = "queued"
	}
	if !s.engine.ReadyFor(t) {
		out.BlockedReason = "No runnable Planner/Worker model is configured; the saved instructions remain pending."
	} else if s.engine.budgetBlocksClaim(t.ID) {
		out.BlockedReason = "Task budget does not allow further worker dispatch; continuing does not reset recorded spend."
	}
	return out, nil
}
