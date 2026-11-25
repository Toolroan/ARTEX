package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

type workRequestsKey struct{}
type workRequestsContext struct {
	grants   []db.WorkRequestGrant
	requests []db.WorkRequest
}

// WithWorkRequests binds server-selected delivery leases and their public
// request records. An explicitly empty binding refuses new graph writes; an
// absent binding preserves the legacy autonomous task tools. Grants, including
// their delivery tokens, never enter a prompt or model-facing schema.
func WithWorkRequests(ctx context.Context, grants []db.WorkRequestGrant, requests []db.WorkRequest) context.Context {
	bound := workRequestsContext{grants: append([]db.WorkRequestGrant(nil), grants...)}
	// Snapshot nested node slices as well, so later caller changes cannot alter
	// the context presented to this turn.
	bound.requests = append([]db.WorkRequest(nil), requests...)
	for i := range bound.requests {
		bound.requests[i].Nodes = append([]db.WorkRequestNode(nil), bound.requests[i].Nodes...)
	}
	return context.WithValue(ctx, workRequestsKey{}, bound)
}

func workRequestGrant(ctx context.Context, requestID int64) (*db.WorkRequestGrant, error) {
	bound, present := ctx.Value(workRequestsKey{}).(workRequestsContext)
	if !present {
		if requestID != 0 {
			return nil, errors.New("request_id requires a current server-granted work request")
		}
		return nil, nil
	}
	if len(bound.grants) == 0 {
		return nil, errors.New("no current work request lease; do not create untracked work or retry a stale handoff")
	}
	if requestID == 0 {
		if len(bound.grants) != 1 {
			return nil, errors.New("request_id is required because this turn has multiple work requests")
		}
		requestID = bound.grants[0].RequestID
	}
	for _, g := range bound.grants {
		if g.RequestID == requestID && g.RequestID > 0 && g.Revision > 0 && g.DeliveryToken != "" && (g.Owner == "main" || g.Owner == "planner") {
			copy := g
			return &copy, nil
		}
	}
	return nil, errors.New("request_id is not granted to this turn; do not invent request IDs or reuse an expired handoff")
}

func selectWorkRequest(ctx context.Context, raw json.RawMessage) (*db.WorkRequestGrant, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("work request tool input must be an object")
	}
	for _, key := range []string{"delivery_token", "request_revision", "slot_key", "item_id", "grant"} {
		if _, supplied := fields[key]; supplied {
			return nil, errors.New("work request authority and item keys are supplied by the server, not tool arguments")
		}
	}
	for _, batch := range []string{"intents", "goals"} {
		var items []map[string]json.RawMessage
		if value, present := fields[batch]; present && json.Unmarshal(value, &items) == nil {
			for _, item := range items {
				if _, supplied := item["request_id"]; supplied {
					return nil, errors.New("request_id belongs at the top level; every item in one batch belongs to the same request")
				}
			}
		}
	}
	var args struct {
		RequestID *int64 `json:"request_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, errors.New("invalid request_id; use a positive request ID from the current work requests")
	}
	var id int64
	if args.RequestID != nil {
		if *args.RequestID <= 0 {
			return nil, errors.New("request_id must be positive")
		}
		id = *args.RequestID
	}
	return workRequestGrant(ctx, id)
}

func hasWorkRequestGrant(ctx context.Context) bool {
	bound, _ := ctx.Value(workRequestsKey{}).(workRequestsContext)
	return len(bound.grants) > 0
}

func workRequestReminder(ctx context.Context) string {
	bound, present := ctx.Value(workRequestsKey{}).(workRequestsContext)
	if !present {
		return ""
	}
	// This serialization excludes grants/tokens and duplicate operator text:
	// the immutable instruction is already in Main's input or Planner's inbox.
	public := make([]map[string]any, 0, len(bound.requests))
	for _, request := range bound.requests {
		public = append(public, map[string]any{"id": request.ID, "source_message_id": request.SourceMessageID, "route": request.Route, "state": request.State, "revision": request.Revision, "acceptance": request.Acceptance, "resolution": request.Resolution, "review_requested": request.ReviewRequested, "nodes": request.Nodes})
	}
	raw, _ := json.Marshal(public)
	return "\n[Durable work requests and existing linked nodes]\n" + string(raw) + `
Main and Planner work on these SAME request IDs and linked goal/intent IDs. Reuse existing matching nodes; do not re-create Main's work during handoff. For a granted request, set_goals/add_intent automatically allocate stable items and attach created nodes. Pass request_id when there is more than one current request; never invent operation/slot keys or lease tokens. Use attach_work_request_node to adopt existing matching work/evidence instead of duplicating it. Goals describe acceptance criteria; intents describe execution steps. Goal IDs are NOT fact/finding parent_ids. Only create goals required by the current operator request, not autonomous goals. Inspect returned existing IDs and worker results before creating a corrected follow-up. Completion of a model turn, receipt review, or one worker is NOT completion of the request. Use resolve_work_request with observed evidence when acceptance is met, or record active/blocked with a concrete reason. Do not cancel without explicit server-granted operator authority. A turn with no current request lease cannot mutate these requests or create untracked replacement work.`
}

func workRequestAuxTool(name string) bool {
	return name == "attach_work_request_node" || name == "resolve_work_request"
}

// A resolver may remove a binding, but it cannot replace the request-aware
// canonical handlers with untracked writes under the same tool name.
func enforceWorkRequestTools(ctx context.Context, tools, canonical []actool.CoreTool) []actool.CoreTool {
	if _, present := ctx.Value(workRequestsKey{}).(workRequestsContext); !present {
		return tools
	}
	allowed := map[string]actool.CoreTool{}
	for _, tool := range canonical {
		allowed[tool.Name()] = tool
	}
	seen := map[string]bool{}
	out := make([]actool.CoreTool, 0, len(tools))
	for _, tool := range tools {
		name := tool.Name()
		if name != "set_goals" && name != "add_intent" && !workRequestAuxTool(name) && name != "list_work_requests" {
			out = append(out, tool)
			continue
		}
		if original := allowed[name]; original != nil && !seen[name] {
			out = append(out, guardPanic(original))
			seen[name] = true
		}
	}
	return out
}

func (t *ToolSet) workRequestTools() []actool.CoreTool {
	requestField := idp("current durable work request ID; optional only when exactly one request is granted to this turn")
	return []actool.CoreTool{
		t.readExpTool("list_work_requests", "Read this task's durable work requests, acceptance criteria, outcomes and existing linked goal/intent/evidence IDs. Use request_id to inspect one request, or omit it to list recent requests. Reading a request does not grant permission to mutate it.", obj(map[string]any{"request_id": idp("optional request ID in this task"), "limit": intp("maximum requests, default 20, maximum 50")}), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var a struct {
				RequestID int64 `json:"request_id"`
				Limit     int   `json:"limit"`
			}
			if json.Unmarshal(raw, &a) != nil || a.RequestID < 0 {
				return actool.Errorf("Invalid work request arguments"), nil
			}
			if a.RequestID > 0 {
				request, err := t.ts.GetWorkRequest(ctx, a.RequestID)
				if err != nil || request == nil {
					return actool.Errorf("Work request is unavailable in this task"), nil
				}
				return jsonResult(request)
			}
			if a.Limit <= 0 || a.Limit > 50 {
				a.Limit = 20
			}
			requests, err := t.ts.ListWorkRequests(ctx, a.Limit)
			if err != nil {
				return actool.Errorf("Work requests could not be read"), nil
			}
			return jsonResult(map[string]any{"requests": requests})
		}),
		t.writeExpTool("attach_work_request_node", "Attach an existing goal, intent, fact or finding from this task to the currently granted durable work request. Reuse matching work instead of duplicating it. This records request membership; it does not use a goal as intent lineage or re-run the node.", obj(map[string]any{"request_id": requestField, "node_id": idp("existing goal/intent/fact/finding node ID in this task")}, "node_id"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			grant, err := selectWorkRequest(ctx, raw)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if grant == nil {
				return actool.Errorf("No work request is granted to this turn"), nil
			}
			var a struct {
				NodeID int64 `json:"node_id"`
			}
			if json.Unmarshal(raw, &a) != nil || a.NodeID <= 0 {
				return actool.Errorf("node_id must be a positive node ID"), nil
			}
			if err := t.ts.LinkWorkRequestNode(ctx, *grant, a.NodeID); err != nil {
				return actool.Errorf("Could not attach node: " + err.Error()), nil
			}
			return jsonResult(map[string]any{"request_id": grant.RequestID, "node_id": a.NodeID, "attached": true})
		}),
		t.writeExpTool("resolve_work_request", "Record the outcome of the currently granted durable request: active, blocked, completed, or cancelled. Completed requires all linked work settled and acceptance proven by met goals or observed tool_output/operator_input facts. Delivery, empty frontier and worker return alone are not proof. Cancelled requires explicit server-granted operator authorization. Report a concrete reason and applicable evidence IDs.", obj(map[string]any{"request_id": requestField, "state": map[string]any{"type": "string", "enum": []string{"active", "blocked", "completed", "cancelled"}}, "reason": str("how observed outcomes satisfy acceptance, or the concrete unresolved blocker"), "evidence_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "maxItems": 64}}, "state", "reason"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			grant, err := selectWorkRequest(ctx, raw)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if grant == nil {
				return actool.Errorf("No work request is granted to this turn"), nil
			}
			var a struct {
				State       string  `json:"state"`
				Reason      string  `json:"reason"`
				EvidenceIDs []int64 `json:"evidence_ids"`
			}
			if json.Unmarshal(raw, &a) != nil {
				return actool.Errorf("Invalid work request outcome"), nil
			}
			if err := t.ts.ResolveWorkRequest(ctx, *grant, a.State, a.Reason, a.EvidenceIDs); err != nil {
				return actool.Errorf("Request remains unresolved: " + err.Error()), nil
			}
			return jsonResult(map[string]any{"request_id": grant.RequestID, "state": a.State, "reason": a.Reason})
		}),
	}
}

func (t *ToolSet) addRequestIntent(ctx context.Context, grant db.WorkRequestGrant, it intentItem) (int64, bool, error) {
	it.Summary = strings.TrimSpace(it.Summary)
	if it.Summary == "" {
		return 0, false, errors.New("summary must not be empty")
	}
	parents := pidList(it.ParentIDs)
	for _, parent := range parents {
		n, err := t.ts.GetNodeWithSources(parent)
		if err != nil || n == nil || (n.Kind != db.KindFact && n.Kind != db.KindFinding) {
			return 0, false, fmt.Errorf("parent_id %d must be an existing fact/finding; goals and intents are not lineage parents", parent)
		}
	}
	anchors := pidList(it.AssetIDs)
	if t.as != nil && len(anchors) > 0 {
		hits, err := t.as.CheckAssetsIntercept(t.taskID, anchors)
		if err != nil {
			return 0, false, fmt.Errorf("asset interception check failed: %w", err)
		}
		if len(hits) > 0 {
			return 0, false, errors.New("intent assets failed the test-scope check; no work item was created")
		}
	}
	priority := it.Priority
	if priority == 0 {
		priority = 5
	}
	payload := map[string]any{"summary": it.Summary}
	if len(anchors) > 0 {
		payload["asset_ids"] = anchors
	}
	item, err := t.ts.AllocateWorkRequestItem(ctx, grant, "intent", it.Summary)
	if err != nil {
		return 0, false, err
	}
	return t.ts.CreateWorkRequestNode(ctx, grant, db.WorkNodeSpec{ItemID: item.ID, Payload: payload, Priority: priority, Anchors: anchors, ParentIDs: parents, Origin: t.worker})
}

func (t *ToolSet) addRequestGoal(ctx context.Context, grant db.WorkRequestGrant, it goalItem) (int64, bool, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, false, errors.New("text must not be empty")
	}
	payload := map[string]any{"text": text}
	if class := strings.TrimSpace(it.VulnClass); class != "" {
		payload["vulnclass"] = class
	}
	item, err := t.ts.AllocateWorkRequestItem(ctx, grant, "goal", text)
	if err != nil {
		return 0, false, err
	}
	return t.ts.CreateWorkRequestNode(ctx, grant, db.WorkNodeSpec{ItemID: item.ID, Payload: payload, Origin: t.worker})
}
