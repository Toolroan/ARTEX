package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
)

// A scripted provider exercises the real Main session without an external LLM or
// database. The zero-value store is safe because only rejected or hook-blocked
// calls are submitted; any accidental graph-handler execution fails the test.
type mainRoleProvider struct {
	calls []llm.ContentBlock
	reqs  []llm.CompletionRequest
}

func (p *mainRoleProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.reqs = append(p.reqs, req)
	first := len(p.reqs) == 1
	return func(yield func(llm.StreamEvent, error) bool) {
		if first && len(p.calls) > 0 {
			for i, call := range p.calls {
				if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: fmt.Sprintf("call-%d", i), ToolName: call.Name}, nil) {
					return
				}
				if !yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(call.Input)}, nil) {
					return
				}
			}
			yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"}, nil)
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "done"}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
	}
}

func (p *mainRoleProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	a := llm.NewAccumulator()
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		a.Add(ev)
	}
	return a.Message(), a.StopReason, a.Usage, nil
}

func mainRoleProbe(name string, calls *atomic.Int64) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: "trusted " + name, Schema: obj(map[string]any{}),
		Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			calls.Add(1)
			return actool.Text("trusted result"), nil
		},
	})
}

func TestMainAgentRoleRejectsExecutionAndExtensions(t *testing.T) {
	oldAugment, oldResolve := ToolAugment(), ToolResolve()
	t.Cleanup(func() { SetToolAugment(oldAugment); SetToolResolve(oldResolve) })
	var augmentCalls, extensionCalls, networkHits atomic.Int64
	SetToolAugment(func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		augmentCalls.Add(1)
		return []actool.CoreTool{mainRoleProbe("mcp__fixture__exec", &extensionCalls)}, DeferredInfo{
			Deferred: []string{"mcp__fixture__exec"}, GlobalNames: []string{"mcp__fixture__exec"},
			Unlock: actool.NewUnlockSet("mcp__fixture__exec"),
		}, nil
	})
	SetToolResolve(func(_ context.Context, role string, in []actool.CoreTool) []actool.CoreTool {
		if role != "mainagent" {
			t.Fatalf("unexpected role %q", role)
		}
		// A resolver can mutate its input in place; this must not change the
		// separately held trusted handlers or their model-facing metadata.
		in[0] = mainRoleProbe("graph_overview", &extensionCalls)
		out := make([]actool.CoreTool, 0, len(in))
		for _, tool := range in {
			if tool.Name() != "list_facts" { // Honor an ordinary disabled binding.
				out = append(out, tool)
			}
		}
		out = append(out, actool.DefaultTools()...)
		out = append(out, actool.ShellSessionTools()...)
		for _, name := range []string{"custom_exec", "mcp__fixture__exec", "add_hint", "Compress"} {
			out = append(out, mainRoleProbe(name, &extensionCalls))
		}
		return out
	})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { networkHits.Add(1) }))
	t.Cleanup(server.Close)
	workDir := t.TempDir()
	marker := filepath.Join(workDir, "execution-must-not-run")
	input := map[string]any{"command": "touch " + marker, "file_path": marker, "content": "forbidden", "url": server.URL,
		"query": "fixture", "name": "mcp__fixture__exec", "arguments": map[string]any{}}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	p := &mainRoleProvider{}
	for _, name := range []string{"Bash", "Write", "shell_open", "Monitor", "WebFetch", "web_search", "custom_exec", "mcp__fixture__exec", "execute_extra_tool", "Skill", "Compress", "steer_work"} {
		p.calls = append(p.calls, llm.ContentBlock{Name: name, Input: raw})
	}
	m := NewMainAgent(p, "fixture", workDir, nil, 0, 3)
	m.SetWebSearch(WebSearchOpts{Enabled: true})
	m.SetNoaEnabled(func() bool { return true })
	if _, err := m.Chat(t.Context(), 0, 0, nil, &db.ExplorationStore{}, "fixture", "delegate this work", passHooks{}, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if augmentCalls.Load() != 0 || extensionCalls.Load() != 0 || networkHits.Load() != 0 {
		t.Fatalf("unexpected execution: augmentation=%d extension=%d network=%d", augmentCalls.Load(), extensionCalls.Load(), networkHits.Load())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("execution marker: %v", err)
	}
	if len(p.reqs) != 2 {
		t.Fatalf("provider requests=%d, want rejected calls followed by final reply", len(p.reqs))
	}
	var got []string
	for _, schema := range p.reqs[0].Tools {
		got = append(got, schema.Name)
		if schema.Name == "graph_overview" && schema.Description == "trusted graph_overview" {
			t.Fatal("in-place resolver replacement changed a trusted handler")
		}
	}
	want := []string{"graph_overview", "list_findings", "node_detail", "expand_digest", "get_worker_output", "get_worker_trace", "search_all_worker_traces", "list_assets", "list_untested_assets", "list_hints", "add_hint", "add_intent", "set_goals", "set_constraints", "add_task_scope", "add_company_scope"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("final provider tools=%v, want %v", got, want)
	}
	results := 0
	for _, message := range p.reqs[1].Messages {
		for _, block := range message.Content {
			if block.Type == llm.BlockToolResult {
				results++
				if !block.IsError {
					t.Errorf("forbidden call %s succeeded", block.ToolUseID)
				}
			}
		}
	}
	if results != len(p.calls) {
		t.Fatalf("tool results=%d, want %d rejections", results, len(p.calls))
	}
}

func TestMainAgentRoleRestoresTrustedHandlersAndDropsBindings(t *testing.T) {
	var trustedCalls, replacedCalls atomic.Int64
	trusted := []actool.CoreTool{mainRoleProbe("add_hint", &trustedCalls), mainRoleProbe("list_facts", &trustedCalls)}
	spoof := mainRoleProbe("add_hint", &replacedCalls)
	opts := agentcore.Options{Tools: []actool.CoreTool{spoof, spoof, mainRoleProbe("Bash", &replacedCalls)}}
	enforceMainAgentRole(&opts, trusted)
	if len(opts.Tools) != 1 || opts.Tools[0].Description() != "trusted add_hint" {
		t.Fatalf("canonical tools=%v, want only original add_hint", opts.Tools)
	}
	res, err := opts.Tools[0].Call(t.Context(), json.RawMessage(`{}`), nil)
	if err != nil || res.IsError || trustedCalls.Load() != 1 || replacedCalls.Load() != 0 {
		t.Fatalf("trusted call: result=%v err=%v trusted=%d replacement=%d", res, err, trustedCalls.Load(), replacedCalls.Load())
	}
	// Panic containment must survive replacing the resolver's wrappers.
	panics := actool.Build(actool.Spec{Name: "add_hint", Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) { panic("fixture") }})
	enforceMainAgentRole(&opts, []actool.CoreTool{panics})
	res, err = opts.Tools[0].Call(t.Context(), json.RawMessage(`{}`), nil)
	if err != nil || !res.IsError {
		t.Fatalf("panic was not converted to a tool error: %v %v", res, err)
	}
}

func TestMainAgentRoleEmptyBindingsCannotRestoreSDKTools(t *testing.T) {
	p := &captureReqProvider{}
	opts := agentcore.Options{
		Provider: p, PermissionMode: permission.ModeBypass, WorkingDir: t.TempDir(), MaxTurns: 1,
		EnableWebFetch: true, EnableWebSearch: true,
		DeferredTools: []string{"mcp__fixture__exec"}, UnlockSet: actool.NewUnlockSet("mcp__fixture__exec"),
		Plan: &agentcore.PlanOptions{}, Memory: &agentcore.MemoryOptions{}, Skills: skill.NewRegistry(), Todos: actool.NewTodoStore(),
		AskUser: func(context.Context, actool.Question) actool.Answer { return actool.Answer{} },
	}
	enforceMainAgentRole(&opts, mainAgentOrchestrationTools(NewToolSet(nil, "human")))
	if opts.Tools == nil {
		t.Fatal("nil Tools would restore SDK defaults")
	}
	s := agentcore.NewSession(opts)
	defer s.Close()
	for _, err := range s.Prompt(t.Context(), "fixture") {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := p.last().Tools; len(got) != 0 {
		t.Fatalf("SDK reintroduced tools after empty bindings: %v", got)
	}
}

func TestMainAgentRolePreservesIndependentHooks(t *testing.T) {
	oldResolve := ToolResolve()
	SetToolResolve(nil)
	t.Cleanup(func() { SetToolResolve(oldResolve) })
	hooks := &recordingHooks{}
	ctx := intercept.WithTargetScope(t.Context(), intercept.TargetScope{TaskID: "fixture", WaitBound: intercept.DefaultTargetWaitBound})
	m := NewMainAgent(toolCallThenStop("graph_overview", `{}`), "fixture", t.TempDir(), nil, 0, 3)
	if _, err := m.Chat(ctx, 0, 0, nil, &db.ExplorationStore{}, "fixture", "status", hooks, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	hooks.assertOneScopedCall(t, "graph_overview", "fixture")
}

func TestMainAgentRoleSurvivesPromptOverrideAndCoverageToggle(t *testing.T) {
	oldPrompt := PromptOverride()
	SetPromptOverride(func(string) (string, bool) { return "custom body", true })
	t.Cleanup(func() { SetPromptOverride(oldPrompt) })
	system := mainAgentSystem("fixture", "/data", "/work")
	if !strings.HasPrefix(system, "custom body") || !strings.Contains(system, mainAgentRoleInstructions) || !strings.Contains(system, untrustedContentRules) {
		t.Fatal("editable prompt displaced role or untrusted-output instructions")
	}
	tools := NewToolSet(nil, "human")
	tools.SetCoverageEnabled(false)
	for _, tool := range mainAgentOrchestrationTools(tools) {
		if tool.Name() == "list_untested_assets" {
			t.Fatal("disabled coverage tool remained available")
		}
	}
}

func TestMainAgentRoleModelHintCannotClaimHumanOrigin(t *testing.T) {
	store := guardTaskStore(t)
	tools := NewToolSet(store, "human")
	var notifiedOrigin string
	tools.SetNotifyHint(func(origin string, _ []string) { notifiedOrigin = origin })
	for _, tool := range mainAgentOrchestrationTools(tools) {
		if tool.Name() != "add_hint" {
			continue
		}
		result, err := tool.Call(t.Context(), json.RawMessage(`{"text":"Model-authored clarification fixture"}`), nil)
		if err != nil || result.IsError {
			t.Fatalf("add_hint: %v %v", result, err)
		}
		var id int64
		if _, err := fmt.Sscanf(result.Flatten(), "hint added: %d", &id); err != nil {
			t.Fatal(err)
		}
		node, err := store.GetNode(id)
		if err != nil || node == nil || node.Origin != "mainagent" || notifiedOrigin != "mainagent" {
			t.Fatalf("model hint provenance: node=%+v notify=%q err=%v", node, notifiedOrigin, err)
		}
		return
	}
	t.Fatal("Main clarification tool missing")
}
