package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

type guardScriptProvider struct {
	messages []llm.Message
	err      error
	calls    atomic.Int64
	closed   atomic.Int64
}

func (p *guardScriptProvider) next() llm.Message {
	n := int(p.calls.Add(1)) - 1
	if len(p.messages) == 0 {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}
	}
	if n >= len(p.messages) {
		n = len(p.messages) - 1
	}
	return p.messages[n]
}
func (p *guardScriptProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	m := p.next()
	stop := "end_turn"
	if len(m.ToolUses()) > 0 {
		stop = "tool_use"
	}
	return m, stop, llm.Usage{InputTokens: 101, OutputTokens: 12}, p.err
}
func (p *guardScriptProvider) Stream(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		defer p.closed.Add(1)
		m := p.next()
		if p.err != nil {
			yield(llm.StreamEvent{}, p.err)
			return
		}
		if err := ctx.Err(); err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 101}}, nil) {
			return
		}
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockText:
				if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text}, nil) {
					return
				}
			case llm.BlockThinking:
				if !yield(llm.StreamEvent{Type: llm.SEThinkingDelta, Text: b.Thinking}, nil) {
					return
				}
			case llm.BlockToolUse:
				if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name}, nil) {
					return
				}
				if !yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)}, nil) {
					return
				}
			}
		}
		stop := "end_turn"
		if len(m.ToolUses()) > 0 {
			stop = "tool_use"
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: llm.Usage{OutputTokens: 12}}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func guardToolCall(id, name string) llm.ContentBlock {
	return llm.ContentBlock{Type: llm.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(`{}`)}
}

func TestMainToolGuardStopsRepeatedUnavailableCallsBeforeDispatch(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "complete"}[nonStreaming], func(t *testing.T) {
			provider := &guardScriptProvider{messages: []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				llm.TextBlock("The tool disappeared transiently; I will reissue the request."),
				guardToolCall("read", "graph_overview"), guardToolCall("bad", "add_intent"),
			}}}}
			var mutations, statusReads atomic.Int64
			ctx := WithInstructionStatusReader(t.Context(), func(ctx context.Context, ids []int64) (InstructionStatusPage, error) {
				if ctx.Err() != nil || len(ids) != 0 {
					t.Fatal("status lookup used cancelled stream or fabricated IDs")
				}
				statusReads.Add(1)
				return InstructionStatusPage{CurrentMessageID: 17, Items: []InstructionStatus{{MessageID: 17, Route: "action", Owner: "planner", Delivery: "reviewing", Reason: "Planner is reviewing this instruction."}}}, nil
			})
			var emitted []db.Activity
			reply, _, err := captureRun(ctx, agentcore.Options{
				Provider: GuardMainToolCalls(provider), SystemPrompt: []string{mainAgentRoleInstructions},
				Tools: []actool.CoreTool{mainRoleProbe("graph_overview", &mutations)}, NonStreaming: nonStreaming, MaxTurns: 5, DisableBackgroundTasks: true,
			}, "Earlier receipt: queued for Planner", func(a db.Activity) { emitted = append(emitted, a) })
			if err != nil {
				t.Fatal(err)
			}
			if provider.calls.Load() != 1 || statusReads.Load() != 1 || mutations.Load() != 0 {
				t.Fatalf("calls=%d status=%d dispatches=%d", provider.calls.Load(), statusReads.Load(), mutations.Load())
			}
			if !nonStreaming && provider.closed.Load() != 1 {
				t.Fatal("underlying stream was not closed")
			}
			if !strings.Contains(reply, "Planner is reviewing this instruction") || !strings.Contains(reply, "Main will not retry") || !strings.Contains(reply, "not confirm that a worker applied") || strings.Contains(reply, "transiently") || strings.Contains(reply, "reissue") {
				t.Fatalf("bad authoritative reply: %s", reply)
			}
			for _, a := range emitted {
				if a.Kind == "tool_use" || a.Kind == "tool_result" || strings.Contains(a.Detail, "transiently") {
					t.Fatalf("blocked batch/preface escaped: %+v", a)
				}
			}
		})
	}
}

func TestMainToolGuardPreservesActualOneOffAndControlFences(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		for _, mode := range []string{"main_oneoff", "control"} {
			t.Run(mode+map[bool]string{false: "Stream", true: "Complete"}[nonStreaming], func(t *testing.T) {
				name := "add_intent"
				if mode == "control" {
					name = "set_constraints"
				}
				provider := &guardScriptProvider{messages: []llm.Message{
					{Role: llm.RoleAssistant, Content: []llm.ContentBlock{guardToolCall("allowed", name)}},
					{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}},
				}}
				var calls, fences atomic.Int64
				ctx := WithMainTurnPolicy(t.Context(), MainTurnPolicy{Mode: mode, ControlTools: []string{"set_constraints"}, Execute: func(_ context.Context, tool string, _ json.RawMessage, run func() (actool.Result, error)) (actool.Result, error) {
					if tool != name {
						t.Fatal("wrong canonical tool")
					}
					fences.Add(1)
					return run()
				}})
				ctx = WithWorkRequests(ctx, []db.WorkRequestGrant{{RequestID: 7, Revision: 1, Owner: "main", DeliveryToken: "fixture"}}, nil)
				opts := agentcore.Options{Provider: provider, SystemPrompt: []string{mainAgentRoleInstructions}, Tools: []actool.CoreTool{mainRoleProbe(name, &calls)}, NonStreaming: nonStreaming, MaxTurns: 3, DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass}
				enforceMainTurnPolicy(ctx, &opts)
				opts.Provider = GuardMainToolCalls(opts.Provider)
				reply, _, err := captureRun(ctx, opts, "Perform the explicitly assigned operation", nil)
				if err != nil || reply != "done" || calls.Load() != 1 || fences.Load() != 1 || provider.calls.Load() != 2 {
					t.Fatalf("allowed operation broken: reply=%q err=%v calls=%d fences=%d provider=%d", reply, err, calls.Load(), fences.Load(), provider.calls.Load())
				}
			})
		}
	}
}

func TestMainToolGuardDoesNotReinterpretHistoryOrInternalCompaction(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		p := &guardScriptProvider{messages: []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("unchanged answer")}}}}
		guard := GuardMainToolCalls(p)
		req := llm.CompletionRequest{System: []string{mainAgentRoleInstructions}, Messages: []llm.Message{llm.UserText(`historical Error: unknown tool "add_intent"`)}}
		if nonStreaming {
			m, _, u, err := guard.Complete(t.Context(), req)
			if err != nil || m.Text() != "unchanged answer" || u.InputTokens != 101 || u.OutputTokens != 12 {
				t.Fatalf("normal response altered: %+v %+v %v", m, u, err)
			}
		} else {
			acc := llm.NewAccumulator()
			for e, err := range guard.Stream(t.Context(), req) {
				if err != nil {
					t.Fatal(err)
				}
				acc.Add(e)
			}
			if acc.Message().Text() != "unchanged answer" || acc.Usage.InputTokens != 101 || acc.Usage.OutputTokens != 12 {
				t.Fatalf("normal stream altered: %+v %+v", acc.Message(), acc.Usage)
			}
		}
		p = &guardScriptProvider{messages: []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{guardToolCall("internal", "not_a_main_tool")}}}}
		guard = GuardMainToolCalls(p)
		req = llm.CompletionRequest{System: []string{"Summarize this conversation for internal compaction."}}
		if nonStreaming {
			m, stop, _, err := guard.Complete(t.Context(), req)
			if err != nil || stop != "tool_use" || len(m.ToolUses()) != 1 {
				t.Fatal("internal provider request hijacked")
			}
		} else {
			acc := llm.NewAccumulator()
			for e, err := range guard.Stream(t.Context(), req) {
				if err != nil {
					t.Fatal(err)
				}
				acc.Add(e)
			}
			if len(acc.Message().ToolUses()) != 1 {
				t.Fatal("internal stream request hijacked")
			}
		}
	}
}

func TestMainToolGuardFailsClosedWithoutStatusAndPreservesErrors(t *testing.T) {
	bad := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{guardToolCall("bad", "Bash")}}
	req := llm.CompletionRequest{System: []string{mainAgentRoleInstructions}}
	p := &guardScriptProvider{messages: []llm.Message{bad}}
	m, stop, usage, err := GuardMainToolCalls(p).Complete(t.Context(), req)
	if err != nil || stop != "end_turn" || usage.InputTokens != 101 || usage.OutputTokens != 12 || len(m.ToolUses()) != 0 || !strings.Contains(m.Text(), "status could not be verified") || strings.Contains(m.Text(), "queued for Planner") {
		t.Fatalf("status failure fabricated state: %+v %s %+v %v", m, stop, usage, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, _, err := GuardMainToolCalls(p).Complete(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation replaced with success: %v", err)
	}
	var streamErr error
	for _, err := range GuardMainToolCalls(p).Stream(ctx, req) {
		if err != nil {
			streamErr = err
		}
	}
	if !errors.Is(streamErr, context.Canceled) {
		t.Fatalf("stream cancellation replaced with success: %v", streamErr)
	}
	want := errors.New("provider fixture failure")
	p = &guardScriptProvider{err: want}
	if _, _, _, err := GuardMainToolCalls(p).Complete(t.Context(), req); !errors.Is(err, want) {
		t.Fatal("provider error hidden")
	}
	streamErr = nil
	for _, err := range GuardMainToolCalls(p).Stream(t.Context(), req) {
		if err != nil {
			streamErr = err
		}
	}
	if !errors.Is(streamErr, want) {
		t.Fatal("stream provider error hidden")
	}
}

func TestMainToolGuardBoundsItsStreamingBuffer(t *testing.T) {
	p := &guardScriptProvider{messages: []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(strings.Repeat("x", mainGuardMaxBytes+1))}}}}
	var got error
	var events int
	for _, err := range GuardMainToolCalls(p).Stream(t.Context(), llm.CompletionRequest{System: []string{mainAgentRoleInstructions}}) {
		if err != nil {
			got = err
		} else {
			events++
		}
	}
	if !errors.Is(got, errMainResponseLimit) || events != 0 || p.closed.Load() != 1 {
		t.Fatalf("unbounded or partial stream: err=%v events=%d closed=%d", got, events, p.closed.Load())
	}
}
