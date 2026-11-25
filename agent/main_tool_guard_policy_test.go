package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

func TestMainToolGuardStopsRepeatOnlyUnauthorizedVisibleCalls(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		for _, tc := range []struct {
			name, mode, binding, tool, input string
			controls                         []string
		}{
			{name: "readonly", mode: "readonly", binding: "main", tool: "set_goals"},
			{name: "lifecycle only", mode: "control", controls: []string{"continue_task"}, binding: "main", tool: "set_goals"},
			{name: "absent lease", mode: "work_request", tool: "set_goals"},
			{name: "empty lease", mode: "work_request", binding: "empty", tool: "set_goals"},
			{name: "Planner lease", mode: "work_request", binding: "planner", tool: "set_goals"},
			{name: "wrong request ID", mode: "work_request", binding: "main", tool: "set_goals", input: `{"request_id":999}`},
			{name: "forged grant", mode: "work_request", binding: "main", tool: "set_goals", input: `{"delivery_token":"forged"}`},
			{name: "unassigned sibling dispatch", mode: "control", controls: []string{"continue_task", "set_goals"}, binding: "main", tool: "add_intent"},
		} {
			t.Run(tc.name+map[bool]string{false: "/stream", true: "/complete"}[nonStreaming], func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				if tc.binding != "" {
					var grants []db.WorkRequestGrant
					if tc.binding != "empty" {
						grants = []db.WorkRequestGrant{{RequestID: 7, Revision: 1, Owner: tc.binding, DeliveryToken: "fixture"}}
					}
					ctx = WithWorkRequests(ctx, grants, nil)
				}
				ctx = WithMainTurnPolicy(ctx, MainTurnPolicy{Mode: tc.mode, ControlTools: tc.controls, Execute: func(context.Context, string, json.RawMessage, func() (actool.Result, error)) (actool.Result, error) {
					t.Fatal("unauthorized response reached a server fence")
					return actool.Text(""), nil
				}})
				input := tc.input
				if input == "" {
					input = `{}`
				}
				blocks := []llm.ContentBlock{llm.TextBlock("I will retry until this works."), guardToolCall("read", "list_goals")}
				if tc.tool == "add_intent" {
					// An authorized sibling must not execute from a rejected batch.
					blocks = append(blocks, guardToolCall("goal", "set_goals"))
				}
				blocks = append(blocks, llm.ContentBlock{Type: llm.BlockToolUse, ID: "denied", Name: tc.tool, Input: json.RawMessage(input)})
				provider := &guardScriptProvider{messages: []llm.Message{{Role: llm.RoleAssistant, Content: blocks}}}
				var toolCalls atomic.Int64
				opts := agentcore.Options{Provider: GuardMainToolCalls(provider), SystemPrompt: []string{mainAgentRoleInstructions}, NonStreaming: nonStreaming, MaxTurns: 0, DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass,
					Tools: []actool.CoreTool{mainRoleProbe("list_goals", &toolCalls), mainRoleProbe("set_goals", &toolCalls), mainRoleProbe("add_intent", &toolCalls)}}
				enforceMainTurnPolicy(ctx, &opts)
				if len(opts.Tools) != 3 {
					t.Fatal("stable advertised schemas disappeared")
				}
				reply, _, err := captureRun(ctx, opts, "Current operator request", nil)
				if err != nil || provider.calls.Load() != 1 || toolCalls.Load() != 0 || !strings.Contains(reply, "Main will not retry") || strings.Contains(reply, "I will retry until") || strings.Contains(reply, "unavailable to Main") {
					t.Fatalf("unauthorized repeat escaped: reply=%q err=%v provider=%d tools=%d", reply, err, provider.calls.Load(), toolCalls.Load())
				}
				if !nonStreaming && provider.closed.Load() != 1 {
					t.Fatal("provider stream was not closed")
				}
			})
		}
	}
}

func TestMainToolGuardStopsBeforeNextProviderAfterServerFenceDenial(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "complete"}[nonStreaming], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			ctx = WithWorkRequests(ctx, []db.WorkRequestGrant{{RequestID: 7, Revision: 1, Owner: "main", DeliveryToken: "fixture"}}, nil)
			var fences, canonical atomic.Int64
			ctx = WithMainTurnPolicy(ctx, MainTurnPolicy{Mode: "work_request", Execute: func(context.Context, string, json.RawMessage, func() (actool.Result, error)) (actool.Result, error) {
				fences.Add(1)
				return actool.Errorf("Task is now paused; the request remains saved."), nil
			}})
			provider := &guardScriptProvider{messages: []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{guardToolCall("goal", "set_goals")}}}}
			opts := agentcore.Options{Provider: GuardMainToolCalls(provider), SystemPrompt: []string{mainAgentRoleInstructions}, NonStreaming: nonStreaming, MaxTurns: 0, DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, Tools: []actool.CoreTool{mainRoleProbe("set_goals", &canonical)}}
			enforceMainTurnPolicy(ctx, &opts)
			reply, _, err := captureRun(ctx, opts, "Current assigned work", nil)
			if err != nil || provider.calls.Load() != 1 || fences.Load() != 1 || canonical.Load() != 0 || !strings.Contains(reply, "Task is now paused") || !strings.Contains(reply, "Main will not retry") {
				t.Fatalf("server denial retried: reply=%q err=%v provider=%d fences=%d canonical=%d", reply, err, provider.calls.Load(), fences.Load(), canonical.Load())
			}
		})
	}
}

func TestMainToolGuardPreservesCanonicalValidationRetry(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "complete"}[nonStreaming], func(t *testing.T) {
			ctx := WithWorkRequests(t.Context(), []db.WorkRequestGrant{{RequestID: 7, Revision: 1, Owner: "main", DeliveryToken: "fixture"}}, nil)
			ctx = WithMainTurnPolicy(ctx, MainTurnPolicy{Mode: "work_request", Execute: func(_ context.Context, _ string, _ json.RawMessage, run func() (actool.Result, error)) (actool.Result, error) {
				return run()
			}})
			var calls atomic.Int64
			tool := writeTool("set_goals", "fixture", obj(map[string]any{}), func(context.Context, json.RawMessage) (actool.Result, error) {
				if calls.Add(1) == 1 {
					return actool.Errorf("Provide a nonempty goal text."), nil
				}
				return actool.Text("saved"), nil
			})
			provider := &guardScriptProvider{messages: []llm.Message{
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{guardToolCall("first", "set_goals")}},
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{guardToolCall("corrected", "set_goals")}},
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}},
			}}
			opts := agentcore.Options{Provider: GuardMainToolCalls(provider), SystemPrompt: []string{mainAgentRoleInstructions}, NonStreaming: nonStreaming, MaxTurns: 4, DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, Tools: []actool.CoreTool{tool}}
			enforceMainTurnPolicy(ctx, &opts)
			reply, _, err := captureRun(ctx, opts, "Current assigned work", nil)
			if err != nil || reply != "done" || provider.calls.Load() != 3 || calls.Load() != 2 {
				t.Fatalf("canonical correction blocked: %q %v %d %d", reply, err, provider.calls.Load(), calls.Load())
			}
		})
	}
}

func TestMainFenceDenialStopsLaterWritesInSameResponse(t *testing.T) {
	ctx := WithWorkRequests(t.Context(), []db.WorkRequestGrant{{RequestID: 7, Revision: 1, Owner: "main", DeliveryToken: "fixture"}}, nil)
	var fences, canonical atomic.Int64
	ctx = WithMainTurnPolicy(ctx, MainTurnPolicy{Mode: "control", ControlTools: []string{"continue_task", "set_goals"}, Execute: func(_ context.Context, _ string, _ json.RawMessage, run func() (actool.Result, error)) (actool.Result, error) {
		if fences.Add(1) == 1 {
			return actool.Errorf("Continuation must succeed before goal registration."), nil
		}
		return run()
	}})
	opts := agentcore.Options{Tools: []actool.CoreTool{mainRoleProbe("set_goals", &canonical), mainRoleProbe("continue_task", &canonical)}}
	enforceMainTurnPolicy(ctx, &opts)
	for _, name := range []string{"set_goals", "continue_task", "set_goals"} {
		result, err := workToolByName(t, opts.Tools, name).Call(ctx, json.RawMessage(`{}`), nil)
		if err != nil || !result.IsError {
			t.Fatalf("same-response write escaped denial: %s %+v %v", name, result, err)
		}
	}
	if fences.Load() != 1 || canonical.Load() != 0 {
		t.Fatalf("same-response writes retried: fences=%d canonical=%d", fences.Load(), canonical.Load())
	}
}

func TestMainToolGuardPreservesIdempotentRequestRetry(t *testing.T) {
	store := guardTaskStore(t)
	grant, request := agentWorkRequestFixture(t, store, "guard-idempotent-request")
	ctx := WithWorkRequests(t.Context(), []db.WorkRequestGrant{grant}, []db.WorkRequest{request})
	ctx = WithMainTurnPolicy(ctx, MainTurnPolicy{Mode: "work_request", Execute: func(_ context.Context, _ string, _ json.RawMessage, run func() (actool.Result, error)) (actool.Result, error) {
		return run()
	}})
	first, retry := guardToolCall("first", "set_goals"), guardToolCall("retry", "set_goals")
	first.Input, retry.Input = json.RawMessage(`{"text":"Verify the synthetic saved row count"}`), json.RawMessage(`{"text":"Verify the synthetic saved row count"}`)
	provider := &guardScriptProvider{messages: []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{first}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{retry}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}},
	}}
	tools := NewToolSet(store, "human")
	opts := agentcore.Options{Provider: GuardMainToolCalls(provider), SystemPrompt: []string{mainAgentRoleInstructions}, MaxTurns: 4, DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, Tools: []actool.CoreTool{tools.setGoals()}}
	enforceMainTurnPolicy(ctx, &opts)
	reply, _, err := captureRun(ctx, opts, "Current assigned work", nil)
	if err != nil || reply != "done" || provider.calls.Load() != 3 {
		t.Fatalf("idempotent retry blocked: %q %v %d", reply, err, provider.calls.Load())
	}
	goals, err := store.ListByKind(db.KindGoal, 10)
	if err != nil || len(goals) != 1 {
		t.Fatalf("retry duplicated goals: %+v %v", goals, err)
	}
	latest, err := store.GetWorkRequest(t.Context(), request.ID)
	if err != nil || latest == nil || len(latest.Nodes) != 1 || latest.Nodes[0].NodeID != goals[0].ID {
		t.Fatalf("retry lost request identity: %+v %v", latest, err)
	}
}
