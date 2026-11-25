package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

func agentWorkRequestFixture(t *testing.T, store *db.ExplorationStore, key string) (db.WorkRequestGrant, db.WorkRequest) {
	t.Helper()
	m, _, err := store.CreateMainMessage(t.Context(), db.MainMessageInput{RequestKey: key, Mode: "auto", InputSource: "main", RawText: "Analyze this synthetic saved file", OperatorText: "Analyze this synthetic saved file"})
	if err != nil {
		t.Fatal(err)
	}
	m, err = store.ClaimMainRouting(t.Context(), m.ID)
	if err != nil || m == nil {
		t.Fatalf("routing claim: %+v %v", m, err)
	}
	m, err = store.FinishMainRouting(t.Context(), m.ID, m.RoutingToken, db.MainRouteDecision{Route: "action", Owner: "main", DecisionData: json.RawMessage(`{"kind":"action","one_off":false}`)})
	if err != nil {
		t.Fatal(err)
	}
	m, err = store.ClaimMainDelivery(t.Context(), m.ID)
	if err != nil || m == nil {
		t.Fatalf("delivery claim: %+v %v", m, err)
	}
	request, err := store.GetWorkRequest(t.Context(), m.ID)
	if err != nil || request == nil {
		t.Fatalf("request missing: %+v %v", request, err)
	}
	return db.WorkRequestGrant{RequestID: m.ID, Revision: request.Revision, DeliveryToken: m.DeliveryToken, Owner: "main"}, *request
}

func callWorkTool(t *testing.T, ctx context.Context, tool actool.CoreTool, input any) actool.Result {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Call(ctx, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func workToolByName(t *testing.T, tools []actool.CoreTool, name string) actool.CoreTool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %s missing", name)
	return nil
}

func workCreatedResult(t *testing.T, result actool.Result) (int64, bool) {
	t.Helper()
	var got struct {
		ID      int64 `json:"id"`
		Created bool  `json:"created"`
	}
	if result.IsError || json.Unmarshal([]byte(result.Flatten()), &got) != nil || got.ID <= 0 {
		t.Fatalf("bad creation result: %+v", result)
	}
	return got.ID, got.Created
}

func TestWorkRequestCreationReusesSameNodesAcrossMainPlannerHandoff(t *testing.T) {
	store := guardTaskStore(t)
	grant, request := agentWorkRequestFixture(t, store, "agent-request-handoff")
	ctx := WithWorkRequests(t.Context(), []db.WorkRequestGrant{grant}, []db.WorkRequest{request})
	main := NewToolSet(store, "human")
	var goalsNotified, resumed int
	main.SetNotifyGoal(func([]string) { goalsNotified++ })
	main.SetResumeTask(func() { resumed++ })
	goalInput := map[string]any{"text": "Synthetic totals match the saved fixture"}
	goalID, created := workCreatedResult(t, callWorkTool(t, ctx, main.setGoals(), goalInput))
	if !created {
		t.Fatal("first goal was not created")
	}
	if id, created := workCreatedResult(t, callWorkTool(t, ctx, main.setGoals(), goalInput)); id != goalID || created {
		t.Fatal("retry duplicated goal")
	}
	if goalsNotified != 1 || resumed != 1 {
		t.Fatalf("duplicate goal changed scheduling: notifications=%d resumes=%d", goalsNotified, resumed)
	}
	bad := callWorkTool(t, ctx, main.addIntent(), map[string]any{"summary": "A goal is not factual evidence", "parent_ids": []int64{goalID}})
	if !bad.IsError {
		t.Fatal("goal ID was accepted as an intent fact parent")
	}
	intentInput := map[string]any{"summary": fmt.Sprintf("Read the saved fixture and validate totals; serves goal #%d", goalID)}
	intentID, created := workCreatedResult(t, callWorkTool(t, ctx, main.addIntent(), intentInput))
	if !created {
		t.Fatal("first intent not created")
	}
	if err := store.HandoffMainRequest(t.Context(), grant.RequestID, grant.DeliveryToken); err != nil {
		t.Fatal(err)
	}
	batch, err := store.ClaimPlannerMessages(t.Context(), 8)
	if err != nil || batch == nil || len(batch.Messages) != 1 {
		t.Fatalf("claim handed-off request: %+v %v", batch, err)
	}
	latest, err := store.GetWorkRequest(t.Context(), request.ID)
	if err != nil || latest == nil || len(latest.Nodes) != 2 {
		t.Fatalf("linked graph missing: %+v %v", latest, err)
	}
	plannerGrant := db.WorkRequestGrant{RequestID: request.ID, Revision: latest.Revision, Owner: "planner", DeliveryToken: batch.Token}
	pctx := WithWorkRequests(t.Context(), []db.WorkRequestGrant{plannerGrant}, []db.WorkRequest{*latest})
	planner := NewToolSet(store, "planner")
	if id, created := workCreatedResult(t, callWorkTool(t, pctx, planner.setGoals(), goalInput)); id != goalID || created {
		t.Fatal("Planner duplicated Main's goal")
	}
	if id, created := workCreatedResult(t, callWorkTool(t, pctx, planner.addIntent(), intentInput)); id != intentID || created {
		t.Fatal("Planner duplicated Main's intent")
	}
	if result := callWorkTool(t, ctx, main.addIntent(), map[string]any{"summary": "stale Main write"}); !result.IsError {
		t.Fatal("stale Main lease created another intent")
	}
	goals, _ := store.ListByKind(db.KindGoal, 20)
	intents, _ := store.ListByKind(db.KindIntent, 20)
	if len(goals) != 1 || len(intents) != 1 {
		t.Fatalf("duplicate/orphan graph nodes: goals=%d intents=%d", len(goals), len(intents))
	}
	if result := callWorkTool(t, pctx, workToolByName(t, planner.workRequestTools(), "resolve_work_request"), map[string]any{"state": "completed", "reason": "worker was dispatched"}); !result.IsError {
		t.Fatal("dispatch alone completed request")
	}
}

func TestWorkRequestSelectionCannotInventAuthorityOrFallback(t *testing.T) {
	store := guardTaskStore(t)
	g1, r1 := agentWorkRequestFixture(t, store, "agent-request-first")
	if err := store.HandoffMainRequest(t.Context(), g1.RequestID, g1.DeliveryToken); err != nil {
		t.Fatal(err)
	}
	g2, r2 := agentWorkRequestFixture(t, store, "agent-request-second")
	if err := store.HandoffMainRequest(t.Context(), g2.RequestID, g2.DeliveryToken); err != nil {
		t.Fatal(err)
	}
	batch, err := store.ClaimPlannerMessages(t.Context(), 8)
	if err != nil || batch == nil || len(batch.Messages) != 2 {
		t.Fatalf("claim multi-request Planner batch: %+v %v", batch, err)
	}
	g1.Owner, g1.DeliveryToken = "planner", batch.Token
	g2.Owner, g2.DeliveryToken = "planner", batch.Token
	tools := NewToolSet(store, "planner")
	ctx := WithWorkRequests(t.Context(), []db.WorkRequestGrant{g1, g2}, []db.WorkRequest{r1, r2})
	for _, input := range []map[string]any{{"text": "ambiguous request"}, {"text": "forged request", "request_id": r2.ID + 100000, "delivery_token": g2.DeliveryToken}, {"text": "invalid ID", "request_id": 0}, {"text": "invalid type", "request_id": "1"}} {
		if result := callWorkTool(t, ctx, tools.setGoals(), input); !result.IsError {
			t.Fatalf("ungranted write accepted: %v", input)
		}
	}
	workCreatedResult(t, callWorkTool(t, ctx, tools.setGoals(), map[string]any{"text": "Second request acceptance", "request_id": r2.ID}))
	first, _ := store.GetWorkRequest(t.Context(), r1.ID)
	second, _ := store.GetWorkRequest(t.Context(), r2.ID)
	if len(first.Nodes) != 0 || len(second.Nodes) != 1 {
		t.Fatal("explicit request ID linked to wrong request")
	}
	empty := WithWorkRequests(t.Context(), nil, []db.WorkRequest{r1})
	if result := callWorkTool(t, empty, tools.addIntent(), map[string]any{"summary": "unleased replacement work"}); !result.IsError {
		t.Fatal("bound-empty context fell back to legacy write")
	}
	if result := callWorkTool(t, t.Context(), tools.addIntent(), map[string]any{"summary": "forged legacy request", "request_id": r1.ID}); !result.IsError {
		t.Fatal("legacy context accepted model-provided request authority")
	}
	if result := callWorkTool(t, t.Context(), tools.addIntent(), map[string]any{"summary": "legacy autonomous work"}); result.IsError {
		t.Fatalf("legacy untracked tool broken: %+v", result)
	}
}

func TestWorkRequestResolutionRequiresEvidenceAndAllowsExistingNodeReuse(t *testing.T) {
	store := guardTaskStore(t)
	grant, request := agentWorkRequestFixture(t, store, "agent-request-observed")
	ctx := WithWorkRequests(t.Context(), []db.WorkRequestGrant{grant}, []db.WorkRequest{request})
	tools := NewToolSet(store, "human").workRequestTools()
	resolve := workToolByName(t, tools, "resolve_work_request")
	for _, input := range []map[string]any{{"state": "completed", "reason": "I answered"}, {"state": "cancelled", "reason": "I chose to abandon it"}} {
		if result := callWorkTool(t, ctx, resolve, input); !result.IsError {
			t.Fatalf("unsupported resolution accepted: %v", input)
		}
	}
	factID := addGuardNode(t, store, db.KindFact, map[string]any{"summary": "Synthetic saved fixture contains 3 rows", "confidence": "observed", "source": "tool_output"}, "confirmed")
	attach := workToolByName(t, tools, "attach_work_request_node")
	for range 2 {
		if result := callWorkTool(t, ctx, attach, map[string]any{"node_id": factID}); result.IsError {
			t.Fatalf("attach failed: %+v", result)
		}
	}
	if result := callWorkTool(t, ctx, resolve, map[string]any{"state": "completed", "reason": "Observed fixture row count satisfies the requested check", "evidence_ids": []int64{factID}}); result.IsError {
		t.Fatalf("observed resolution refused: %+v", result)
	}
	got, err := store.GetWorkRequest(t.Context(), request.ID)
	if err != nil || got.State != "completed" || len(got.Nodes) != 1 {
		t.Fatalf("resolution or dedup wrong: %+v %v", got, err)
	}
}

func TestWorkRequestContextAndToolPolicyStayScoped(t *testing.T) {
	grant := db.WorkRequestGrant{RequestID: 7, Revision: 1, DeliveryToken: "SECRET_LEASE", Owner: "main"}
	request := db.WorkRequest{ID: 7, OperatorText: "DUPLICATE_OPERATOR_TEXT", Acceptance: "Check the saved fixture", Nodes: []db.WorkRequestNode{{ID: 9, NodeID: 10, Kind: "goal", Summary: "Original"}}}
	ctx := WithWorkRequests(t.Context(), []db.WorkRequestGrant{grant}, []db.WorkRequest{request})
	request.Nodes[0].Summary = "MUTATED_AFTER_BIND"
	text := workRequestReminder(ctx)
	if strings.Contains(text, "SECRET_LEASE") || strings.Contains(text, "DUPLICATE_OPERATOR_TEXT") || strings.Contains(text, "MUTATED_AFTER_BIND") || !strings.Contains(text, `"node_id":10`) {
		t.Fatalf("unsafe or missing context: %s", text)
	}
	var writes atomic.Int64
	trusted := []actool.CoreTool{mainRoleProbe("set_goals", &writes), mainRoleProbe("add_intent", &writes), mainRoleProbe("attach_work_request_node", &writes), mainRoleProbe("resolve_work_request", &writes), mainRoleProbe("set_constraints", &writes)}
	for _, mode := range []string{"readonly", "work_request"} {
		policyCtx := WithMainTurnPolicy(ctx, MainTurnPolicy{Mode: mode, Execute: func(_ context.Context, _ string, _ json.RawMessage, run func() (actool.Result, error)) (actool.Result, error) {
			return run()
		}})
		opts := agentcore.Options{Tools: trusted}
		enforceMainTurnPolicy(policyCtx, &opts)
		if len(opts.Tools) != 4 || workToolByName(t, opts.Tools, "set_goals") == nil {
			t.Fatal("Main goal/intent/request schemas changed with routing")
		}
		for _, tool := range opts.Tools {
			result, err := tool.Call(policyCtx, json.RawMessage(`{}`), nil)
			if err != nil || result.IsError != (mode == "readonly") {
				t.Fatalf("wrong write authority for %s/%s: %+v %v", mode, tool.Name(), result, err)
			}
		}
	}
	if writes.Load() != 4 {
		t.Fatal("read-only stable schemas reached write handlers")
	}
	withGrant := plannerInstructionOnlyTools(ctx, trusted)
	if len(withGrant) != 5 {
		t.Fatalf("instruction-only request capabilities wrong: %d", len(withGrant))
	}
	withoutGrant := plannerInstructionOnlyTools(t.Context(), trusted)
	for _, tool := range withoutGrant {
		if tool.Name() == "set_goals" || workRequestAuxTool(tool.Name()) {
			t.Fatal("legacy instruction round acquired request writes")
		}
	}
}

func TestWorkRequestWorkerControlsLinkExistingIntentBeforeEffect(t *testing.T) {
	for _, name := range []string{"steer_work", "kill_work"} {
		t.Run(name, func(t *testing.T) {
			store := guardTaskStore(t)
			grant, request := agentWorkRequestFixture(t, store, "control-"+name)
			iid, err := store.AddIntent(map[string]any{"summary": "Existing synthetic worker"}, 1, nil, "planner")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetIntentState(iid, "running"); err != nil {
				t.Fatal(err)
			}
			if err := store.HandoffMainRequest(t.Context(), grant.RequestID, grant.DeliveryToken); err != nil {
				t.Fatal(err)
			}
			batch, err := store.ClaimPlannerMessages(t.Context(), 8)
			if err != nil || batch == nil {
				t.Fatalf("claim: %v", err)
			}
			grant.Owner, grant.DeliveryToken = "planner", batch.Token
			ctx := WithWorkRequests(t.Context(), []db.WorkRequestGrant{grant}, []db.WorkRequest{request})
			ts := NewToolSet(store, "planner")
			calls := 0
			check := func(id int64) error {
				calls++
				w, err := store.GetWorkRequest(t.Context(), request.ID)
				if err != nil || len(w.Nodes) != 1 || w.Nodes[0].NodeID != id || w.State != "active" {
					t.Fatalf("control ran before durable link: %+v %v", w, err)
				}
				return nil
			}
			ts.killWork = check
			ts.steerWork = func(id int64, _ string) error { return check(id) }
			tool := ts.steerWorkTool()
			if name == "kill_work" {
				tool = ts.killWorkTool()
			}
			input := map[string]any{"intent_id": iid, "message": "Use the corrected synthetic input"}
			if result := callWorkTool(t, ctx, tool, input); result.IsError {
				t.Fatalf("control failed: %+v", result)
			}
			if err := store.FinishPlannerMessages(t.Context(), batch.Token, true); err != nil {
				t.Fatal(err)
			}
			if result := callWorkTool(t, ctx, tool, input); !result.IsError || calls != 1 {
				t.Fatal("stale request sent another worker control")
			}
			if err := store.SetIntentState(iid, "done"); err != nil {
				t.Fatal(err)
			}
			if err := store.MarkIntentRequestsForReview(t.Context(), iid); err != nil {
				t.Fatal(err)
			}
			w, err := store.GetWorkRequest(t.Context(), request.ID)
			if err != nil || w.State != "review" {
				t.Fatalf("corrected worker outcome lost: %+v %v", w, err)
			}
		})
	}
}
