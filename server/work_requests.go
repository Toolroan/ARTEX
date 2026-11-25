package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Bind write authority to the server's current delivery, never to model-supplied
// tokens. Records expose existing IDs so Main and Planner operate on one plan.
func bindWorkRequests(ctx context.Context, t *Task, messages []db.MainMessage) (context.Context, error) {
	read, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	var requests []db.WorkRequest
	var grants []db.WorkRequestGrant
	for _, message := range messages {
		request, err := t.Store.GetWorkRequest(read, message.ID)
		if err != nil {
			return ctx, err
		}
		if request == nil {
			continue // historical messages are not silently adopted or replayed
		}
		requests = append(requests, *request)
		if message.DeliveryStatus == "inflight" && message.DeliveryToken != "" {
			grants = append(grants, db.WorkRequestGrant{RequestID: request.ID, Revision: request.Revision, DeliveryToken: message.DeliveryToken, Owner: message.DeliveryOwner})
		}
	}
	if len(messages) == 0 {
		all, err := t.Store.ListWorkRequests(read, 200)
		if err != nil {
			return ctx, err
		}
		for _, request := range all {
			if request.State != "completed" && request.State != "cancelled" {
				requests = append(requests, request)
			}
		}
	}
	if len(requests) > 0 {
		// An ordinary round with unresolved requests but no delivery lease
		// may inspect them, not create untracked duplicate work for them.
		ctx = agent.WithWorkRequests(ctx, grants, requests)
	}
	return ctx, nil
}

// Request outcome is a separate projection from message delivery. In
// particular, a presented instruction can still be pending, active or blocked.
func (s *Server) taskWorkRequests(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	ctx, stop := context.WithTimeout(r.Context(), 5*time.Second)
	defer stop()
	requests, err := t.Store.ListWorkRequests(ctx, 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Work requests could not be loaded")
		return
	}
	// The UI needs outcomes and linked work, not another copy of raw chat text.
	for i := range requests {
		requests[i].OperatorText = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": requests})
}

// This explicit operator action authorizes a review of saved progress after
// interruption. It preserves task pause/run limits and never adopts old chat.
func (s *Server) reviewWorkRequest(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("request"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "Invalid work request ID")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, http.StatusConflict, "Task is unavailable")
		return
	}
	defer s.engine.decInflight(t.ID)
	ctx, stop := context.WithTimeout(r.Context(), 5*time.Second)
	defer stop()
	if err := t.Store.ReviewWorkRequest(ctx, id); err != nil {
		writeErr(w, http.StatusConflict, "Request is already being reviewed, resolved, or unavailable in this task")
		return
	}
	t.Notify()
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "waiting_reason": s.plannerWaitingReason(t)})
}
