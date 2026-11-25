package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func routingFixtureProvider(t *testing.T, s *Server, kind string, attemptMutation bool) {
	t.Helper()
	var mu sync.Mutex
	mainCalls := 0
	s.taskAgents = map[string]*taskAgentBundle{}
	s.provByProfile = map[int64]*provEntry{}
	s.llmOn = true
	s.llmCfg = agent.Config{Format: llm.FormatOpenAI, Model: "fixture", Stream: false}
	s.llmProv = retestProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(req.Tools) == 0 {
			text := `{"kind":"` + kind + `","summary":"fixture interpretation","context_ids":[],"proposal_id":0,"requires_context":false,"clarification":"","control_tools":[],"one_off":false}`
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(text)}}, "end_turn", llm.Usage{}, nil
		}
		for _, tool := range req.Tools {
			if tool.Name == "set_goals" || tool.Name == "add_intent" || tool.Name == "add_hint" {
				t.Errorf("read-only Main exposed %s", tool.Name)
			}
		}
		mainCalls++
		if mainCalls == 1 && attemptMutation {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "forbidden", Name: "add_intent", Input: json.RawMessage(`{"summary":"unauthorized fixture work"}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("Fixture answer from existing evidence.")}}, "end_turn", llm.Usage{}, nil
	}}
}

func waitMainFixture(t *testing.T, s *Server, task *Task) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.chatMu.Lock()
		busy := s.chatBusy[task.ID]
		s.chatMu.Unlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Main fixture did not finish")
}

func TestMainMessageStatusActuallyReadOnlyAndCorrectionQueued(t *testing.T) {
	for _, kind := range []string{"context", "correction"} {
		t.Run(kind, func(t *testing.T) {
			s, task := plannerBridgeFixture(t)
			routingFixtureProvider(t, s, kind, true)
			w := plannerBridgeRequest(s, task, false, "fixture operator message")
			if w.Code != http.StatusAccepted {
				t.Fatalf("status=%d %s", w.Code, w.Body)
			}
			waitMainFixture(t, s, task)
			messages, err := task.Store.ListMainMessages(t.Context(), 0, 0, 20)
			if err != nil || len(messages) != 1 {
				t.Fatalf("messages=%+v err=%v", messages, err)
			}
			m := messages[0]
			if m.Route != kind || !strings.Contains(m.AssistantText, "not a temporary tool outage") || !strings.Contains(m.AssistantText, "Main will not retry it") {
				t.Fatalf("message=%+v", m)
			}
			if kind == "correction" && !strings.Contains(m.AssistantText, "Instruction queued for Planner.") {
				t.Fatal("blocked Main response omitted the live Planner receipt")
			}
			if kind == "context" && !strings.Contains(m.AssistantText, "Conversation context only; Planner was not notified.") {
				t.Fatal("blocked Main response misrepresented context as an instruction")
			}
			hints, _ := task.Store.ListByKind(db.KindHint, 20)
			intents, _ := task.Store.ListByKind(db.KindIntent, 20)
			if len(hints) != 0 || len(intents) != 0 {
				t.Fatal("read-only conversation mutated graph")
			}
			pending, err := task.Store.HasQueuedPlannerMessages(t.Context())
			if err != nil || pending != (kind == "correction") {
				t.Fatalf("pending=%v err=%v", pending, err)
			}
			if kind == "context" {
				select {
				case <-task.notify:
					t.Fatal("status message woke Planner")
				default:
				}
			}
		})
	}
}

func TestMainMessageFailedRoutingRemainsVisibleAndRetrySafe(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	w := plannerBridgeRequest(s, task, false, "A request with unavailable model")
	if w.Code != 202 {
		t.Fatalf("status=%d %s", w.Code, w.Body)
	}
	waitMainFixture(t, s, task)
	rows, err := task.Store.ListMainMessages(t.Context(), 0, 0, 10)
	if err != nil || len(rows) != 1 || rows[0].RoutingStatus != "failed" || rows[0].DeliveryStatus != "none" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if s.messageReceipt(task, &rows[0]).Delivery != "routing_failed" || rows[0].AssistantText != "" {
		t.Fatal("failure hidden or saved as final conversation proposal")
	}
	if pending, _ := task.Store.HasQueuedPlannerMessages(t.Context()); pending {
		t.Fatal("failed router dispatched work")
	}
	r := httptest.NewRequest(http.MethodPost, "/retry", nil)
	r.SetPathValue("id", task.ID)
	r.SetPathValue("message", strconvMessageID(rows[0].ID))
	w = httptest.NewRecorder()
	s.retryMainMessageRoute(w, r)
	if w.Code != 202 {
		t.Fatalf("retry=%d %s", w.Code, w.Body)
	}
}

func strconvMessageID(id int64) string { raw, _ := json.Marshal(id); return string(raw) }

func makeMainOwnedFixture(t *testing.T, task *Task) *db.MainMessage {
	t.Helper()
	m, _, err := task.Store.CreateMainMessage(t.Context(), db.MainMessageInput{Segment: 0, RequestKey: "oneoff-fixture", Mode: "auto", InputSource: "main", RawText: "Count this fixture", OperatorText: "Count this fixture"})
	if err != nil {
		t.Fatal(err)
	}
	m, err = task.Store.ClaimMainRouting(t.Context(), m.ID)
	if err != nil || m == nil {
		t.Fatal(err)
	}
	m, err = task.Store.FinishMainRouting(t.Context(), m.ID, m.RoutingToken, db.MainRouteDecision{Route: "action", Owner: "main", DecisionData: json.RawMessage(`{"kind":"action","one_off":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMainOwnedRequestWaitsForResumeAndFencesOneOff(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	_, _ = s.m.pg.Exec(`UPDATE exploration_nodes SET state='met' WHERE exploration_id=$1 AND kind='goal'`, task.ExpID)
	m := makeMainOwnedFixture(t, task)
	task.updateLifecycle(func(state *taskLifecycleState) { state.Paused = true })
	s.runMainMessage(t.Context(), task, m, "")
	stored, _ := task.Store.GetMainMessage(t.Context(), m.ID)
	if stored.DeliveryStatus != "queued" || stored.AssistantText != "" {
		t.Fatal("paused action was consumed")
	}
	if s.messageReceipt(task, stored).Delivery != "waiting" {
		t.Fatal("paused receipt not waiting")
	}
	task.updateLifecycle(func(state *taskLifecycleState) { state.Paused = false })
	m, err := task.Store.ClaimMainDelivery(t.Context(), m.ID)
	if err != nil || m == nil {
		t.Fatal(err)
	}
	policy := s.mainMutationPolicy(task, m)
	calls := 0
	run := func() (actool.Result, error) { calls++; return actool.Text("created fixture"), nil }
	if result, err := policy.Execute(t.Context(), "add_intent", json.RawMessage(`{}`), run); err != nil || result.IsError {
		t.Fatalf("first dispatch %v %+v", err, result)
	}
	if result, _ := policy.Execute(t.Context(), "add_intent", json.RawMessage(`{}`), run); !result.IsError || calls != 1 {
		t.Fatal("one message dispatched twice")
	}
	_, _ = s.m.pg.Exec(`UPDATE main_message_routes SET delivery_until=now()-interval '1 second' WHERE id=$1`, m.ID)
	policy = s.mainMutationPolicy(task, m)
	if result, _ := policy.Execute(t.Context(), "add_intent", json.RawMessage(`{}`), run); !result.IsError || calls != 1 {
		t.Fatal("expired owner executed")
	}
}

func TestMainMessageContextAndIdempotentReplayWhileBusy(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	input := db.MainMessageInput{Segment: 2, RequestKey: "same-request", Mode: "context", InputSource: "main", RawText: "status?", OperatorText: "status?"}
	m, _, err := task.Store.CreateMainMessage(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	s.chatBusy[task.ID] = true
	r := httptest.NewRequest(http.MethodPost, "/api/chat?task="+task.ID, strings.NewReader(`{"message":"status?","seg":2,"request_id":"same-request","routing_mode":"context"}`))
	w := httptest.NewRecorder()
	s.chat(w, r)
	if w.Code != 202 {
		t.Fatalf("replay=%d %s", w.Code, w.Body)
	}
	var response struct {
		Receipt mainMessageReceipt `json:"planner_delivery"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	if response.Receipt.MessageID != m.ID {
		t.Fatal("retry lost original message")
	}
	s.chatBusy[task.ID] = false
}
