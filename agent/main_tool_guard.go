package agent

import (
	"context"
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
// It is applied after role/turn filtering, so req.Tools is the authoritative set.
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

func (p *mainToolGuard) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	message, stop, usage, err := p.inner.Complete(ctx, req)
	if err != nil || !mainGuardRequest(req) {
		return message, stop, usage, err
	}
	if err := ctx.Err(); err != nil {
		return llm.Message{}, "", usage, err
	}
	for _, block := range message.Content {
		if block.Type == llm.BlockToolUse && !mainGuardAllowed(req, block.Name) {
			reply := mainUnavailableToolReply(ctx, block.Name)
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
		// Buffer one bounded Main response. Nothing from an invalid response,
		// including its speculative preface or an earlier tool in the same batch,
		// reaches the SDK, the UI, or the conversation transcript.
		streamCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		events := make([]llm.StreamEvent, 0, 64)
		acc := llm.NewAccumulator()
		bytes := 0
		badName := ""
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
			if event.Type == llm.SEToolUseStart && !mainGuardAllowed(req, event.ToolName) {
				badName, blocked = event.ToolName, true
				cancel()
				break // closes the underlying iterator; no repair/retry model call
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
		if blocked {
			reply := mainUnavailableToolReply(ctx, badName)
			if err := ctx.Err(); err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			for _, event := range []llm.StreamEvent{
				{Type: llm.SETextDelta, Text: reply},
				{Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: acc.Usage},
				{Type: llm.SEMessageStop},
			} {
				if !yield(event, nil) {
					return
				}
			}
			return
		}
		for _, event := range events {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func mainUnavailableToolReply(ctx context.Context, name string) string {
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
	text := "Main's call to " + label + " was stopped. It is unavailable to Main for this turn by design; this is not a temporary tool outage. The blocked response did not dispatch any tools, and Main will not retry it."
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
