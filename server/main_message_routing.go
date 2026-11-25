package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
	"github.com/google/uuid"
)

// Conversation storage and instruction delivery are deliberately separate. A
// persisted chat message is not, by itself, permission for Planner to act.
type mainMessageReceipt struct {
	MessageID      int64  `json:"message_id"`
	Route          string `json:"route"`
	Delivery       string `json:"delivery"`
	Reason         string `json:"reason,omitempty"`
	Text           string `json:"text,omitempty"`
	TaskStatus     string `json:"task_status,omitempty"`
	RequiredAction string `json:"required_action,omitempty"`
}

func (s *Server) acceptMainMessage(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "no active task")
		return
	}
	var req struct {
		Message     string           `json:"message"`
		Attachments []chatAttachment `json:"attachments,omitempty"`
		Seg         *int             `json:"seg,omitempty"`
		RequestID   string           `json:"request_id,omitempty"`
		RoutingMode string           `json:"routing_mode,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPlannerChatBodyBytes)
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeErr(w, 400, "Invalid or oversized message")
		return
	}
	if req.RoutingMode == "" {
		req.RoutingMode = "auto"
	}
	if req.RoutingMode != "auto" && req.RoutingMode != "context" && req.RoutingMode != "instruction" {
		writeErr(w, 400, "Invalid routing mode")
		return
	}
	key, err := routingRequestKey(req.RequestID)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	mainDir := filepath.Join(s.m.dir, "tasks", t.ID, "main")
	operator, err := mainPlannerInstruction(req.Message, req.Attachments, mainDir)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	expanded, ok := s.prepareChatMentionMessage(w, req.Message)
	if !ok {
		return
	}
	expanded = composeMainAgentMessage(expanded, req.Attachments, mainDir)
	seg := 0
	if req.Seg != nil {
		seg = *req.Seg
	} else {
		seg, _ = t.Store.CurrentMainSeg()
	}
	if seg < 0 {
		writeErr(w, 400, "Invalid Main session")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "Task is being deleted or server is draining")
		return
	}
	defer s.engine.decInflight(t.ID)
	// Serialize admission with stop/delete and other Main sends. An idempotent
	// retry can still read its receipt while the original turn is running.
	s.chatMu.Lock()
	if s.engine.IsDeleting(t.ID) {
		s.chatMu.Unlock()
		writeErr(w, 409, "Task is being deleted")
		return
	}
	if s.chatBusy[t.ID] {
		existing, err := t.Store.ExistingMainMessage(r.Context(), key)
		s.chatMu.Unlock()
		if err == nil && existing != nil && existing.Segment == seg && existing.Mode == req.RoutingMode && existing.InputSource == "main" && existing.RawText == req.Message && existing.OperatorText == operator {
			writeJSON(w, 202, map[string]any{"status": "accepted", "mode": "llm", "planner_delivery": s.messageReceipt(t, existing)})
			return
		}
		writeErr(w, 409, "The main agent is still processing the previous message")
		return
	}
	activity := userActivityWithAttachments("mainagent", req.Message, req.Attachments)
	msg, created, err := t.Store.CreateMainMessage(r.Context(), db.MainMessageInput{Segment: seg, RequestKey: key, Mode: req.RoutingMode, InputSource: "main", RawText: req.Message, OperatorText: operator, UserActivity: &activity})
	if err != nil {
		s.chatMu.Unlock()
		if errors.Is(err, db.ErrMainMessageConflict) {
			writeErr(w, 409, err.Error())
		} else {
			writeErr(w, 500, "Message was not accepted because it could not be saved")
		}
		return
	}
	if !created {
		s.chatMu.Unlock()
		writeJSON(w, 200, map[string]any{"status": "accepted", "mode": "rule", "planner_delivery": s.messageReceipt(t, msg)})
		return
	}
	ctx, cancel := context.WithCancelCause(s.ctx)
	ctx = intercept.WithReviewContext(ctx, "", intercept.ReviewBackground{Source: intercept.BackgroundUserMessage, Text: req.Message})
	s.chatBusy[t.ID], s.chatCancel[t.ID] = true, cancel
	s.chatMu.Unlock()
	if msg.UserActivity != nil && s.engine.bc != nil {
		s.engine.bc.Publish(t.ID, *msg.UserActivity)
	}
	go func() {
		defer s.finishTaskChat(t.ID, cancel)
		if !s.engine.beginTaskOperation(t.ID) {
			return
		}
		defer s.engine.decInflight(t.ID)
		s.runMainMessage(ctx, t, msg, expanded)
	}()
	writeJSON(w, 202, map[string]any{"status": "accepted", "mode": "llm", "planner_delivery": s.messageReceipt(t, msg)})
}

func routingRequestKey(key string) (string, error) {
	if key == "" {
		return uuid.NewString(), nil
	}
	if len(key) > 128 || strings.TrimSpace(key) != key || strings.ContainsAny(key, "\x00\r\n") {
		return "", errors.New("invalid message request ID")
	}
	for _, r := range key {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:", r)) {
			return "", errors.New("invalid message request ID")
		}
	}
	return key, nil
}

func mainConversationContext(entries []db.MainConversationEntry) []agent.MainConversationMessage {
	out := make([]agent.MainConversationMessage, 0, len(entries))
	for _, entry := range entries {
		out = append(out, agent.MainConversationMessage{ID: entry.ID, Role: entry.Role, Text: entry.Text, Session: entry.Segment, Truncated: entry.Truncated})
	}
	return out
}

func mainStoredContext(msg *db.MainMessage) []agent.MainConversationMessage {
	var entries []db.MainConversationEntry
	_ = json.Unmarshal(msg.Context, &entries)
	return mainConversationContext(entries)
}

// Eligibility is rechecked at delivery and at every Main mutation. A context
// update can never bypass pause, completion, queue or settlement gates.
func (s *Server) plannerWaitingReason(t *Task) string {
	state := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(state.Status):
		return terminalInstructionReason(state.Status)
	case state.Queued:
		return "Task is queued; waiting until it starts."
	case state.Paused || s.engine.IsPaused(t.ID):
		return "Task is paused; waiting until it is resumed."
	case s.engine.isSettling(t.ID):
		return "Task is winding down; instruction remains saved."
	}
	open, err := t.Store.HasOpenGoal()
	if err != nil {
		return "Planner readiness could not be checked; instruction remains saved."
	}
	if !open {
		active, activeErr := t.Store.HasActiveIntent()
		pending, pendingErr := t.Store.HasQueuedPlannerMessages(s.ctx)
		if activeErr != nil || pendingErr != nil {
			return "Planner readiness could not be checked; instruction remains saved."
		}
		if !active && !pending {
			return "Task has no open work or pending Planner instructions."
		}
	}
	if !s.engine.Started(t.ID) {
		return "Task engine is not running; instruction remains saved."
	}
	planner, _ := s.engine.snapshotFor(t)
	if planner == nil {
		return "Planner is unavailable; instruction remains saved."
	}
	return ""
}

func (s *Server) messageReceipt(t *Task, msg *db.MainMessage) mainMessageReceipt {
	r := mainMessageReceipt{MessageID: msg.ID, Route: msg.Route, Text: msg.RawText}
	switch {
	case msg.RoutingStatus == "failed":
		r.Delivery, r.Reason = "routing_failed", "Message saved, but routing failed. Retry routing or send an explicit Planner instruction."
	case msg.RoutingStatus != "routed":
		r.Delivery, r.Reason = "routing", "Message saved; checking whether it needs action."
	case msg.Route == "context":
		r.Delivery, r.Reason = "context", "Conversation context only; Planner was not notified."
	case msg.Route == "clarify":
		r.Delivery, r.Reason = "clarify", msg.Clarification
	case msg.DeliveryStatus == "needs_review":
		r.Delivery, r.Reason = "needs_review", "Processing was interrupted. Check existing effects before sending another instruction."
	case msg.DeliveryStatus == "inflight" && (msg.DeliveryUntil == nil || !msg.DeliveryUntil.After(time.Now())):
		r.Delivery, r.Reason = "needs_review", "The delivery lease expired or is unavailable. Processing is uncertain; check existing effects before sending another instruction."
	case msg.DeliveryStatus == "presented":
		r.Delivery, r.Reason = "presented", "Reviewed; this is not confirmation that a worker applied it."
	case msg.DeliveryOwner == "main":
		r.Delivery, r.Reason = "main", "Main is recording the requested goals or work before Planner takes over."
		if reason := s.mainMessageWaitingReason(t, msg); reason != "" {
			r.Delivery, r.Reason = "waiting", reason
		}
	case msg.DeliveryStatus == "inflight":
		r.Delivery, r.Reason = "reviewing", "Planner is reviewing this instruction."
	default:
		r.Delivery, r.Reason = "queued", "Instruction queued for Planner."
		if reason := s.plannerWaitingReason(t); reason != "" {
			r.Delivery, r.Reason = "waiting", reason
		}
		state := t.lifecycleSnapshot()
		applyInstructionBlocker(&r, state.Status, state.Paused)
	}
	return r
}

func (s *Server) publishMainRouting(t *Task, msg *db.MainMessage) {
	r := s.messageReceipt(t, msg)
	meta, _ := json.Marshal(map[string]any{"main_message_receipt": r})
	seg := msg.Segment
	s.engine.emitActivity(t, db.Activity{Worker: "mainagent", Kind: "routing", Summary: r.Reason, Metadata: meta, MainSeg: &seg})
}

func (s *Server) mainMessageRoutes(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	seg, _ := strconv.Atoi(r.URL.Query().Get("seg"))
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	rows, err := t.Store.ListMainMessages(r.Context(), seg, before, 20)
	if err != nil {
		writeErr(w, 500, "Could not read message routing status")
		return
	}
	items := make([]mainMessageReceipt, 0, len(rows))
	for i := range rows {
		items = append(items, s.messageReceipt(t, &rows[i]))
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) retryMainMessageRoute(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("message"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "Invalid message ID")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "Task is unavailable")
		return
	}
	defer s.engine.decInflight(t.ID)
	if err = t.Store.RetryMainRouting(r.Context(), id); err != nil {
		writeErr(w, 409, "Only a failed, undelivered routing decision can be retried")
		return
	}
	msg, err := t.Store.GetMainMessage(r.Context(), id)
	if err != nil || msg == nil {
		writeErr(w, 500, "Routing retry saved; status is temporarily unavailable")
		return
	}
	writeJSON(w, 202, s.messageReceipt(t, msg))
}

// A short renewable lease lets restart recovery identify uncertain deliveries
// promptly. Losing a lease cancels the active model; effects are never replayed.
func startMainDeliveryHeartbeat(ctx context.Context, t *Task, token string) (context.Context, func()) {
	if token == "" {
		return ctx, func() {}
	}
	child, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-child.Done():
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(child, 5*time.Second)
				err := t.Store.RenewMainDelivery(check, token)
				stop()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return child, func() { cancel(); <-done }
}

func (s *Server) saveFinalMainReply(ctx context.Context, t *Task, msg *db.MainMessage, text string) {
	if text == "" || msg.RoutingStatus != "routed" || (msg.DeliveryOwner == "main" && msg.DeliveryStatus == "queued") {
		return
	}
	var err error
	for range 2 {
		persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = t.Store.SaveMainReply(persist, msg.ID, text)
		stop()
		if err == nil {
			return
		}
	}
	seg := msg.Segment
	s.engine.emitActivity(t, db.Activity{Worker: "mainagent", Kind: "text", IsError: true, MainSeg: &seg, Summary: "The reply could not be saved as shared context. Please include the full request rather than referring to this reply."})
}

func (s *Server) routeMainMessage(ctx context.Context, t *Task, id int64) (*db.MainMessage, error) {
	msg, err := t.Store.ClaimMainRouting(ctx, id)
	if err != nil || msg == nil {
		return msg, err
	}
	var route agent.MainMessageRoute
	switch msg.Mode {
	case "context":
		route = agent.MainMessageRoute{Kind: "context"}
	case "instruction":
		route = agent.MainMessageRoute{Kind: "action"}
	default:
		if !s.taskRuntimeAvailable(t, "mainagent") {
			err = errors.New("Message router is unavailable")
		} else {
			rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			rctx = agent.WithRunInfo(rctx, agent.RunInfo{TaskID: routingTaskID(t.ID), ExplorationID: t.ExpID})
			rctx = transcript.WithSessionID(rctx, fmt.Sprintf("exp%d-main-route-%d", t.ExpID, msg.ID))
			s.engine.BeginLLMCall(t.ID)
			runtime := s.agentsForTask(t).mainRuntime
			if runtime == nil {
				err = errors.New("Message router is unavailable")
			} else {
				route, err = agent.RouteMainMessage(rctx, runtime, agent.MainMessageRouteInput{
					MessageID: msg.ID, Text: msg.OperatorText, Context: mainStoredContext(msg), Lifecycle: s.mainRoutingLifecycle(t),
				})
			}
			s.engine.EndLLMCall(t.ID)
			cancel()
		}
	}
	if err != nil {
		// Errors remain explicit and durable; no fallback promotes arbitrary chat
		// into an instruction. Do not include provider payloads/secrets in receipts.
		persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if saveErr := t.Store.FailMainRouting(persist, msg.ID, msg.RoutingToken, "Routing did not complete"); saveErr != nil {
			return nil, saveErr
		}
		return t.Store.GetMainMessage(persist, msg.ID)
	}
	owner := ""
	if route.Kind == "action" || route.Kind == "correction" {
		owner = "planner"
	}
	if route.Kind == "control" {
		owner = "main"
	}
	// Main records ordinary explicit requests before handing the same durable
	// request to Planner. Existing goals do not remove Main's planning authority.
	if route.Kind == "action" && msg.InputSource == "main" {
		owner = "main"
	}
	decision, _ := json.Marshal(route)
	msg, err = t.Store.FinishMainRouting(ctx, msg.ID, msg.RoutingToken, db.MainRouteDecision{
		Route: route.Kind, Summary: route.Summary, Clarification: route.Clarification, Owner: owner, DecisionData: decision,
	})
	if err == nil && msg != nil && msg.DeliveryOwner == "planner" {
		t.Notify()
	}
	return msg, err
}

func routingTaskID(id string) int64 { n, _ := strconv.ParseInt(id, 10, 64); return n }

func conversationReader(t *Task, seg int) agent.MainConversationReader {
	return func(ctx context.Context, before int64, limit int) (agent.MainConversationPage, error) {
		entries, err := t.Store.RecentMainConversation(ctx, seg, before, limit)
		if err != nil {
			return agent.MainConversationPage{}, err
		}
		var next int64
		for _, entry := range entries {
			if entry.MessageID > 0 && (next == 0 || entry.MessageID < next) {
				next = entry.MessageID
			}
		}
		if next == before {
			next = 0
		}
		return agent.MainConversationPage{Messages: mainConversationContext(entries), NextCursor: next}, nil
	}
}

func plannerInboxContext(ctx context.Context, t *Task, batch *db.MainPlannerBatch) context.Context {
	var instructions []agent.PlannerInstruction
	var conversation []agent.MainConversationMessage
	seen := map[int64]bool{}
	seg := 0
	if batch != nil {
		for i := range batch.Messages {
			msg := &batch.Messages[i]
			seg = msg.Segment
			var decision agent.MainMessageRoute
			_ = json.Unmarshal(msg.DecisionData, &decision)
			instructions = append(instructions, agent.PlannerInstruction{ID: msg.ID, Kind: msg.Route, Text: msg.OperatorText, Summary: msg.Summary, ContextIDs: decision.ContextIDs, ControlTools: decision.ControlTools})
			for _, item := range mainStoredContext(msg) {
				if !seen[item.ID] {
					seen[item.ID] = true
					conversation = append(conversation, item)
				}
			}
		}
	} else {
		seg, _ = t.Store.CurrentMainSeg()
	}
	ctx = agent.WithMainConversationReader(ctx, conversationReader(t, seg))
	return agent.WithPlannerInbox(ctx, instructions, conversation)
}

func (s *Server) mainMutationPolicy(t *Task, msg *db.MainMessage) agent.MainTurnPolicy {
	policy := agent.MainTurnPolicy{Mode: "readonly"}
	if msg.RoutingStatus != "routed" || msg.DeliveryOwner != "main" || msg.DeliveryStatus != "inflight" {
		return policy
	}
	var decision agent.MainMessageRoute
	if json.Unmarshal(msg.DecisionData, &decision) != nil {
		return policy
	}
	if msg.Route == "control" {
		policy.Mode, policy.ControlTools = "control", decision.ControlTools
	} else if msg.Route == "action" {
		policy.Mode = "work_request"
	}
	var mutationMu sync.Mutex
	var lifecycleCalled bool
	var lifecycleSucceeded bool
	policy.Execute = func(ctx context.Context, tool string, input json.RawMessage, run func() (actool.Result, error)) (actool.Result, error) {
		mutationMu.Lock()
		defer mutationMu.Unlock()
		if !agent.MainTurnAllowsWrite(policy, tool) {
			return actool.Errorf(fmt.Sprintf("This current message does not authorize %s. Goal and intent tools remain available; send an explicit work or goal-registration instruction to use them. Planner-owned instructions must be handled by Planner.", tool)), nil
		}
		s.concMu.Lock()
		defer s.concMu.Unlock()
		current, err := t.Store.GetMainMessage(ctx, msg.ID)
		if err != nil {
			return actool.Errorf("Message ownership could not be verified"), nil
		}
		if current == nil || current.DeliveryOwner != "main" || current.DeliveryStatus != "inflight" || current.DeliveryToken != msg.DeliveryToken || current.DeliveryUntil == nil || !current.DeliveryUntil.After(time.Now()) {
			return actool.Errorf("This message no longer owns a Main action"), nil
		}
		if tool == "wake_planner" || tool == "continue_task" {
			if mainLifecycleTool(current) != tool || lifecycleCalled {
				return actool.Errorf("This message does not authorize another task-control operation. Check its current status."), nil
			}
			lifecycleCalled = true
			// The canonical callback uses the locked scheduler helper. It checks
			// the specific wake/restart lifecycle instead of the ordinary-write gate.
			result, runErr := run()
			var receipt taskControlResult
			action := "wake"
			if tool == "continue_task" {
				action = "restart"
			}
			lifecycleSucceeded = runErr == nil && !result.IsError && json.Unmarshal([]byte(result.Flatten()), &receipt) == nil &&
				receipt.ID == t.ID && receipt.Action == action && receipt.BlockedReason == "" && receipt.Error == "" &&
				!receipt.Paused && !receipt.Queued && !isTerminalStatus(receipt.Status)
			return result, runErr
		}
		state := t.lifecycleSnapshot()
		if mainLifecycleTool(current) != "" {
			if !lifecycleSucceeded {
				return actool.Errorf("Run the authorized task-control operation first. Goal registration remains saved under this request."), nil
			}
			if isTerminalStatus(state.Status) || !s.engine.Started(t.ID) || !s.engine.ReadyFor(t) || s.engine.budgetBlocksClaim(t.ID) {
				return actool.Errorf("Continuation has not admitted a runnable task. The requested work remains saved; check task status and its blocker before claiming goals were registered."), nil
			}
		}
		if state.Paused || state.Queued || s.engine.IsPaused(t.ID) || s.engine.IsDeleting(t.ID) || s.engine.isSettling(t.ID) {
			return actool.Errorf("Task lifecycle changed; the request remains saved and no action was started"), nil
		}

		return run()
	}
	return policy
}

func (s *Server) mainWaitingReason(t *Task) string {
	state := t.lifecycleSnapshot()
	if state.Paused || s.engine.IsPaused(t.ID) {
		return "Task is paused; request saved until it is resumed."
	}
	if state.Queued {
		return "Task is queued; request saved until it starts."
	}
	if s.engine.isSettling(t.ID) || s.engine.IsDeleting(t.ID) {
		return "Task is winding down; request remains saved."
	}
	return ""
}

func (s *Server) runMainMessage(ctx context.Context, t *Task, msg *db.MainMessage, expanded string) {
	if msg.RoutingStatus != "routed" {
		var err error
		msg, err = s.routeMainMessage(ctx, t, msg.ID)
		if err != nil || msg == nil {
			return
		}
	}
	s.publishMainRouting(t, msg)
	if msg.InputSource != "main" {
		return
	}
	if msg.DeliveryOwner == "main" && (msg.DeliveryStatus == "queued" || msg.DeliveryStatus == "inflight") {
		request, readErr := t.Store.GetWorkRequest(ctx, msg.ID)
		if readErr != nil {
			return
		}
		if request != nil {
			// Ordinary Planner rounds can have no inbox lease. Serialize their
			// entire round with Main preparation as well as the durable DB claims.
			if !t.planningMu.TryLock() {
				return // the queued source is picked up by routing recovery
			}
			defer func() { t.planningMu.Unlock(); t.Notify() }()
		}
	}
	if msg.RoutingStatus != "routed" {
		seg := msg.Segment
		s.engine.emitActivity(t, db.Activity{Worker: "mainagent", Kind: "text", Summary: s.messageReceipt(t, msg).Reason, MainSeg: &seg})
		return
	}
	if msg.DeliveryOwner == "main" && msg.DeliveryStatus == "queued" && s.mainMessageWaitingReason(t, msg) == "" {
		claimed, err := t.Store.ClaimMainDelivery(ctx, msg.ID)
		if err != nil || claimed == nil {
			return
		}
		msg = claimed
	}
	if !s.taskRuntimeAvailable(t, "mainagent") {
		seg := msg.Segment
		reply := s.messageReceipt(t, msg).Reason
		s.engine.emitActivity(t, db.Activity{Worker: "mainagent", Kind: "text", Summary: reply, MainSeg: &seg})
		s.saveFinalMainReply(ctx, t, msg, reply)
		if msg.DeliveryStatus == "inflight" && msg.DeliveryOwner == "main" {
			persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = t.Store.FinishMainDelivery(persist, msg.ID, msg.DeliveryToken, false)
			stop()
		}
		return
	}
	if expanded == "" {
		expanded = msg.OperatorText
	}
	receipt, _ := json.Marshal(s.messageReceipt(t, msg))
	agentMsg := expanded + "\n\n[Server message-routing receipt]\n" + string(receipt) + "\nConversation context is not a new instruction. Follow the current receipt and available tools. Never duplicate a Planner-owned request."
	if msg.Route == "clarify" {
		agentMsg += "\nAsk this clarification before any work: " + msg.Clarification
	}
	seg := msg.Segment
	emit := func(rec db.Activity) { rec.MainSeg = &seg; s.engine.emitActivity(t, rec) }
	gctx := intercept.WithTaskContext(ctx, t.ID, "mainagent", func(a db.Activity) { a.Worker = "mainagent"; emit(a) })
	gctx = intercept.WithTargetScope(gctx, t.targetScope(intercept.DefaultTargetWaitBound))
	// All production Main turns are bound, even read-only/lifecycle messages
	// without a request. Visible tools must never fall back to untracked writes.
	gctx = agent.WithWorkRequests(gctx, nil, nil)
	gctx, requestErr := bindWorkRequests(gctx, t, []db.MainMessage{*msg})
	if requestErr != nil {
		emit(db.Activity{Worker: "mainagent", Kind: "text", IsError: true, Summary: "Request ownership could not be loaded; the message remains saved."})
		return
	}
	gctx = agent.WithMainTurnPolicy(gctx, s.mainMutationPolicy(t, msg))
	gctx = agent.WithMainConversationReader(gctx, conversationReader(t, seg))
	gctx = agent.WithInstructionStatusReader(gctx, s.instructionStatusReader(t, seg, msg.ID))
	gctx = agent.WithMainLifecycleControl(gctx, func(callCtx context.Context, action string) (json.RawMessage, error) {
		if err := callCtx.Err(); err != nil {
			return nil, err
		}
		// mainMutationPolicy holds concMu and has checked this durable message's
		// ownership, exact permitted tool and live delivery lease.
		result, err := s.applyTaskLifecycleControlLocked(t, action)
		if err != nil && result.BlockedReason == "" {
			return nil, err
		}
		return json.Marshal(result)
	})
	gctx, stopHeartbeat := startMainDeliveryHeartbeat(gctx, t, msg.DeliveryToken)
	s.engine.BeginLLMCall(t.ID)
	reply, err := s.agentsForTask(t).main.Chat(gctx, routingTaskID(t.ID), seg, s.m.Assets(), t.Store, t.Goal, agentMsg, t.TargetGuard.Hooks(), emit, t.Notify, func() {
		// Canonical mutations run under concMu; a concurrent pause cannot be undone.
		if s.mainWaitingReason(t) == "" {
			if _, resumeErr := s.admitTaskWhenLocked(t, "resume", false); resumeErr != nil {
				emit(db.Activity{Worker: "mainagent", Kind: "text", IsError: true, Summary: "Work was saved, but task admission failed; review task status."})
			}
		}
	}, t.NotifyGoal, t.NotifyHint)
	s.engine.EndLLMCall(t.ID)
	healthyDelivery := gctx.Err() == nil
	stopHeartbeat()
	if err != nil && ctx.Err() == nil {
		emit(db.Activity{Worker: "mainagent", Kind: "text", IsError: true, Summary: "Main could not finish this turn; the message and its routing status remain saved."})
	}
	if err == nil && healthyDelivery {
		s.saveFinalMainReply(ctx, t, msg, reply)
	}
	if msg.DeliveryOwner == "main" && msg.DeliveryStatus == "inflight" {
		persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		var finishErr error
		request, readErr := t.Store.GetWorkRequest(persist, msg.ID)
		if readErr != nil {
			finishErr = readErr
		} else if request != nil && err == nil && healthyDelivery && request.State != "completed" && request.State != "cancelled" {
			finishErr = t.Store.HandoffMainRequest(persist, msg.ID, msg.DeliveryToken)
			if finishErr == nil {
				// The explicit action itself is durable work, including when Main
				// did not manage to create a goal. Admit its Planner handoff using
				// the same lifecycle checks as Main's ordinary graph mutations.
				s.concMu.Lock()
				if mainLifecycleTool(msg) == "" && s.mainWaitingReason(t) == "" {
					_, finishErr = s.admitTaskWhenLocked(t, "resume", false)
				}
				s.concMu.Unlock()
				t.Notify()
			}
		} else {
			finishErr = t.Store.FinishMainDelivery(persist, msg.ID, msg.DeliveryToken, err == nil && healthyDelivery)
		}
		stop()
		if finishErr != nil {
			emit(db.Activity{Worker: "mainagent", Kind: "text", IsError: true, Summary: "Delivery receipt could not be saved; check existing effects before retrying."})
		}
	}
}

// Recovery retries only the tool-free routing phase. A delivery which may have
// made changes becomes needs_review rather than silently replaying effects.
func (s *Server) runMainRoutingRecovery() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	slots := make(chan struct{}, 2)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		for _, t := range s.m.List() {
			if s.ctx.Err() != nil {
				return
			}
			if !s.engine.beginTaskOperation(t.ID) {
				continue
			}
			_ = t.Store.RecoverMainRouting(s.ctx)
			_ = t.Store.MarkFinishedWorkRequestsForReview(s.ctx)
			ids, err := t.Store.PendingMainRoutingIDs(s.ctx, 1)
			if pending, readErr := t.Store.HasQueuedPlannerMessages(s.ctx); readErr == nil && pending && s.plannerWaitingReason(t) == "" {
				t.Notify()
			}
			if err == nil && len(ids) == 0 {
				ids, err = t.Store.ReadyMainLifecycleDeliveryIDs(s.ctx, 1)
				if err == nil && len(ids) == 0 && s.mainWaitingReason(t) == "" {
					ids, err = t.Store.ReadyMainDeliveryIDs(s.ctx, 1)
				}
			}
			s.engine.decInflight(t.ID)
			if err != nil || len(ids) == 0 {
				continue
			}
			select {
			case slots <- struct{}{}:
			default:
				continue
			}
			s.chatMu.Lock()
			if s.chatBusy[t.ID] {
				s.chatMu.Unlock()
				<-slots
				continue
			}
			ctx, cancel := context.WithCancelCause(s.ctx)
			s.chatBusy[t.ID], s.chatCancel[t.ID] = true, cancel
			s.chatMu.Unlock()
			go func(task *Task, id int64) {
				defer func() { <-slots; s.finishTaskChat(task.ID, cancel) }()
				if !s.engine.beginTaskOperation(task.ID) {
					return
				}
				defer s.engine.decInflight(task.ID)
				msg, err := task.Store.GetMainMessage(ctx, id)
				if err == nil && msg != nil {
					s.runMainMessage(ctx, task, msg, "")
				}
			}(t, ids[0])
		}
	}
}
