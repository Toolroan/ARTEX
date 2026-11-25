package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

type routeProvider struct {
	response llm.Message
	err      error
	calls    int
	request  llm.CompletionRequest
	deadline time.Time
}

func (p *routeProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	panic("router must make one bounded Complete call")
}
func (p *routeProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	p.request = req
	p.deadline, _ = ctx.Deadline()
	return p.response, "end_turn", llm.Usage{}, p.err
}
func routeJSON(route MainMessageRoute) string {
	if route.ContextIDs == nil {
		route.ContextIDs = []int64{}
	}
	if route.ControlTools == nil {
		route.ControlTools = []string{}
	}
	raw, _ := json.Marshal(route)
	return string(raw)
}
func TestRouteMainMessageUsesOneBoundedToolFreeRequest(t *testing.T) {
	input := MainMessageRouteInput{MessageID: 7, Text: "What is the current status?", Lifecycle: "running", Context: []MainConversationMessage{{ID: -12, Role: "assistant", Text: "I propose collecting another target; this is not authorization."}}}
	p := &routeProvider{response: llm.UserText(routeJSON(MainMessageRoute{Kind: "context", Summary: "Status question only"}))}
	got, err := RouteMainMessage(t.Context(), p, input)
	if err != nil || got.Kind != "context" || p.calls != 1 {
		t.Fatalf("route=%+v calls=%d err=%v", got, p.calls, err)
	}
	if len(p.request.Tools) != 0 || p.request.Thinking != "disabled" || p.request.MaxTokens <= 0 || p.request.MaxTokens > 1600 || p.deadline.IsZero() || time.Until(p.deadline) > 45*time.Second {
		t.Fatal("router call is not bounded and tool-free")
	}
	if len(p.request.Messages) != 1 || !strings.Contains(p.request.Messages[0].Text(), input.Text) || !strings.Contains(p.request.Messages[0].Text(), "not authorization") {
		t.Fatal("router lost raw message/context")
	}
}
func TestRouteMainMessageRejectsProviderFailureAndToolCalls(t *testing.T) {
	for _, p := range []*routeProvider{
		{err: errors.New("fixture failure")},
		{response: llm.Message{Content: []llm.ContentBlock{{Type: llm.BlockToolUse, Name: "add_intent"}}}},
		{response: llm.UserText("not JSON")},
	} {
		if _, err := RouteMainMessage(t.Context(), p, MainMessageRouteInput{Text: "Recheck the existing file"}); err == nil || p.calls != 1 {
			t.Fatalf("failure accepted: %v calls=%d", err, p.calls)
		}
	}
}
func TestMainMessageRouteValidationAndResolvedProposal(t *testing.T) {
	contextItems := []MainConversationMessage{{ID: -3, Role: "user", Text: "Analyze only saved data"}, {ID: 13, Role: "assistant", Text: "I can recount the saved CSV"}, {ID: 15, Role: "assistant", Text: "Or collect new data"}}
	valid := MainMessageRoute{Kind: "action", Summary: "Recount the existing CSV only", RequiresContext: true, ContextIDs: []int64{-3, 13}, ProposalID: 13, OneOff: true}
	got, err := parseMainMessageRoute(routeJSON(valid), contextItems)
	if err != nil || got.Kind != "action" || got.ProposalID != 13 {
		t.Fatalf("resolved approval=%+v err=%v", got, err)
	}
	for _, refs := range [][]int64{{-3}, {13, 15}} {
		v := valid
		v.ContextIDs = refs
		got, err := parseMainMessageRoute(routeJSON(v), contextItems)
		if err != nil || got.Kind != "clarify" || got.OneOff {
			t.Fatalf("ambiguous approval dispatched: %+v %v", got, err)
		}
	}
	truncated := append([]MainConversationMessage(nil), contextItems...)
	truncated[1].Truncated = true
	got, err = parseMainMessageRoute(routeJSON(valid), truncated)
	if err != nil || got.Kind != "clarify" || got.OneOff {
		t.Fatalf("truncated proposal dispatched: %+v %v", got, err)
	}
	invalid := []string{
		`{"kind":"action"}`,
		strings.TrimSuffix(routeJSON(valid), "}") + `,"kind":"context"}`,
		routeJSON(valid) + ` {}`,
		strings.Replace(routeJSON(valid), `"action"`, `"execute"`, 1),
		strings.Replace(routeJSON(valid), `[-3,13]`, `[999]`, 1),
		routeJSON(MainMessageRoute{Kind: "control", ControlTools: []string{"add_intent"}}),
		routeJSON(MainMessageRoute{Kind: "context", ControlTools: []string{"set_goals"}}),
		routeJSON(MainMessageRoute{Kind: "correction", ControlTools: []string{"set_goals"}}),
		routeJSON(MainMessageRoute{Kind: "correction", OneOff: true}),
		routeJSON(MainMessageRoute{Kind: "clarify"}),
	}
	for _, raw := range invalid {
		if _, err := parseMainMessageRoute(raw, contextItems); err == nil {
			t.Errorf("accepted invalid route: %s", raw)
		}
	}
	for _, kind := range []string{"context", "action", "correction", "control", "clarify"} {
		r := MainMessageRoute{Kind: kind, Summary: "fixture"}
		if kind == "control" {
			r.ControlTools = []string{"set_constraints"}
		}
		if kind == "clarify" {
			r.Clarification = "Which proposal?"
		}
		if _, err := parseMainMessageRoute(routeJSON(r), nil); err != nil {
			t.Errorf("valid %s: %v", kind, err)
		}
	}
	if _, err := parseMainMessageRoute(routeJSON(MainMessageRoute{Kind: "correction", Summary: "Stop the existing worker and save the explicit constraint", ControlTools: []string{"set_constraints"}}), nil); err != nil {
		t.Fatalf("mixed correction/constraint rejected: %v", err)
	}
}
