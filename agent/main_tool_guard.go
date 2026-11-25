package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/Autumn-27/norma/llm"
)

const (
	mainGuardMaxBytes  = 2 << 20
	mainGuardMaxEvents = 32768
)

var errMainResponseLimit = errors.New("Main response exceeded its validation buffer limit")

// GuardMainToolCalls checks a Main response before the SDK can dispatch any of
// its tools. The SDK otherwise responds to an unavailable name with a generic
// unknown-tool error before running hooks, which can provoke endless model
// retries and an invented outage diagnosis. The guard never adds capabilities.
// It is applied after canonical role/policy setup. Schemas stay visible, while
// the current policy and request grants decide whether a call may execute.
func GuardMainToolCalls(provider llm.Provider) llm.Provider {
	return &mainToolGuard{inner: provider}
}

type mainToolGuard struct{ inner llm.Provider }

func mainGuardRequest(req llm.CompletionRequest) bool {
	for _, block := range req.System {
		if strings.Contains(block, "[Main agent role boundary]") {
			return true
		}
	}
	// The SDK reuses the provider for context compaction. Those requests have a
	// separate system prompt and must not be turned into operator-facing replies.
	return false
}

func mainGuardAllowed(req llm.CompletionRequest, name string) bool {
	for _, tool := range req.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func mainGuardDenied(ctx context.Context, req llm.CompletionRequest, name string, input json.RawMessage, checkInput bool) (string, bool) {
	if !mainGuardAllowed(req, name) {
		return "", true
	}
	if policy, tagged := ctx.Value(mainTurnPolicyKey{}).(MainTurnPolicy); tagged && !mainReadTool(name) {
		if reason := mainTurnWriteDenial(ctx, policy, name, input, checkInput); reason != "" {
			return reason, true
		}
	}
	return "", false
}

func mainGuardReply(ctx context.Context, name, reason string) string {
	if reason == "" {
		return mainUnavailableToolReply(ctx, name)
	}
	return mainToolStatusReply(ctx, "Main's call to "+mainToolLabel(name)+" was stopped. "+reason+" The blocked response did not dispatch any tools, and Main will not retry it. This is an authorization limit, not a temporary tool outage.")
}

func mainFenceStoppedReply(ctx context.Context, name, reason string) string {
	return mainToolStatusReply(ctx, "The server blocked Main's call to "+mainToolLabel(name)+": "+reason+" Main will not retry it or make further model calls this turn. Any earlier successful operations remain recorded; this refusal does not confirm worker execution.")
}

func (p *mainToolGuard) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if mainGuardRequest(req) {
		if err := ctx.Err(); err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		if name, reason := readMainFenceDenial(ctx); name != "" {
			reply := mainFenceStoppedReply(ctx, name, reason)
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(reply)}}, "end_turn", llm.Usage{}, ctx.Err()
		}
	}
	message, stop, usage, err := p.inner.Complete(ctx, req)
	if err != nil || !mainGuardRequest(req) {
		return message, stop, usage, err
	}
	if err := ctx.Err(); err != nil {
		return llm.Message{}, "", usage, err
	}
	for _, block := range message.Content {
		if block.Type != llm.BlockToolUse {
			continue
		}
		if reason, blocked := mainGuardDenied(ctx, req, block.Name, block.Input, true); blocked {
			reply := mainGuardReply(ctx, block.Name, reason)
			if err := ctx.Err(); err != nil {
				return llm.Message{}, "", usage, err
			}
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(reply)}}, "end_turn", usage, nil
		}
	}
	return message, stop, usage, nil
}

func (p *mainToolGuard) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	if !mainGuardRequest(req) {
		return p.inner.Stream(ctx, req)
	}
	return func(yield func(llm.StreamEvent, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		if name, reason := readMainFenceDenial(ctx); name != "" {
			reply := mainFenceStoppedReply(ctx, name, reason)
			if err := ctx.Err(); err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			yieldMainGuardReply(reply, llm.Usage{}, yield)
			return
		}
		// Buffer one bounded Main response. Nothing from an invalid response,
		// including its speculative preface or an earlier tool in the same batch,
		// reaches the SDK, the UI, or the conversation transcript.
		streamCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		events := make([]llm.StreamEvent, 0, 64)
		acc := llm.NewAccumulator()
		bytes := 0
		badName := ""
		badReason := ""
		blocked := false
		for event, err := range p.inner.Stream(streamCtx, req) {
			if err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			if err := ctx.Err(); err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			if event.Type == llm.SEToolUseStart {
				if reason, denied := mainGuardDenied(ctx, req, event.ToolName, nil, false); denied {
					badName, badReason, blocked = event.ToolName, reason, true
					cancel()
					break // closes the underlying iterator; no repair/retry model call
				}
			}
			bytes += len(event.Text) + len(event.ToolID) + len(event.ToolName) + len(event.StopReason) + 128
			if bytes > mainGuardMaxBytes || len(events) >= mainGuardMaxEvents {
				cancel()
				yield(llm.StreamEvent{}, errMainResponseLimit)
				return
			}
			acc.Add(event)
			events = append(events, event)
		}
		if err := ctx.Err(); err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		if !blocked {
			for _, block := range acc.Message().ToolUses() {
				if reason, denied := mainGuardDenied(ctx, req, block.Name, block.Input, true); denied {
					badName, badReason, blocked = block.Name, reason, true
					break
				}
			}
		}
		if blocked {
			reply := mainGuardReply(ctx, badName, badReason)
			if err := ctx.Err(); err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			yieldMainGuardReply(reply, acc.Usage, yield)
			return
		}
		for _, event := range events {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func yieldMainGuardReply(reply string, usage llm.Usage, yield func(llm.StreamEvent, error) bool) {
	for _, event := range []llm.StreamEvent{
		{Type: llm.SETextDelta, Text: reply},
		{Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: usage},
		{Type: llm.SEMessageStop},
	} {
		if !yield(event, nil) {
			return
		}
	}
}

func mainToolLabel(name string) string {
	label := "a tool outside its current role"
	valid := name != "" && len(name) <= 80
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-' || c == ':') {
			valid = false
			break
		}
	}
	if valid {
		label = "`" + name + "`"
	}
	return label
}

func mainUnavailableToolReply(ctx context.Context, name string) string {
	return mainToolStatusReply(ctx, "Main's call to "+mainToolLabel(name)+" was stopped. It is unavailable to Main for this turn by design; this is not a temporary tool outage. The blocked response did not dispatch any tools, and Main will not retry it.")
}

func mainToolStatusReply(ctx context.Context, text string) string {
	statusCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	page, err := ReadInstructionStatus(statusCtx, nil)
	if err != nil {
		return text + "\n\nCurrent instruction status could not be verified. Check the delivery receipt before resending; no worker action is confirmed."
	}
	for _, item := range page.Items {
		if page.CurrentMessageID <= 0 || item.MessageID != page.CurrentMessageID {
			continue
		}
		reason := strings.TrimSpace(item.Reason)
		if reason == "" {
			reason = "Delivery status: " + item.Delivery + "."
		}
		text += fmt.Sprintf("\n\nMessage #%d: %s", item.MessageID, reason)
		if item.Owner == "planner" {
			text += " Planner owns this instruction; Main will not create a duplicate worker request."
		}
		if item.Route == "action" || item.Route == "correction" || item.Route == "control" {
			text += " This receipt does not confirm that a worker applied it."
		}
		return text
	}
	return text + "\n\nNo current-message delivery receipt was returned. Check instruction status before resending; no worker action is confirmed."
}
