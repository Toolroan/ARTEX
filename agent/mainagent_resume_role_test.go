package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func TestMainAgentResumedExecutionHistoryKeepsCurrentRoleAndDelegation(t *testing.T) {
	for _, delegationEnabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("delegation_enabled_%t", delegationEnabled), func(t *testing.T) {
			oldPrompt, oldResolve := PromptOverride(), ToolResolve()
			t.Cleanup(func() { SetPromptOverride(oldPrompt); SetToolResolve(oldResolve) })
			SetPromptOverride(func(string) (string, bool) {
				return "LEGACY BODY: use Bash directly to analyze files.", true
			})
			SetToolResolve(func(_ context.Context, _ string, in []actool.CoreTool) []actool.CoreTool {
				out := make([]actool.CoreTool, 0, len(in))
				for _, tool := range in {
					if delegationEnabled || tool.Name() != "add_intent" {
						out = append(out, tool)
					}
				}
				return out
			})

			store := guardTaskStore(t)
			workDir := t.TempDir()
			mainDir := filepath.Join(workDir, "tasks", fmt.Sprint(store.ID()), "main")
			filePath := filepath.Join(mainDir, "uploads", "saved-results.csv")
			if err := os.MkdirAll(filepath.Dir(filePath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filePath, []byte("fixture,value\none,7\n"), 0600); err != nil {
				t.Fatal(err)
			}
			tx := transcript.NewStore(t.TempDir())
			sessionID := fmt.Sprintf("exp%d-main", store.ID())
			writer := tx.NewWriter(sessionID, "")
			writer.RecordMessage(llm.UserText("Earlier: count the saved CSV."), llm.Usage{})
			writer.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
				Type: llm.BlockToolUse, ID: "legacy-bash", Name: "Bash", Input: json.RawMessage("{\"command\":\"old fixture command\"}"),
			}}}, llm.Usage{})
			writer.RecordMessage(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
				llm.ToolResultText("legacy-bash", "Historical fixture count: 12", false),
			}}, llm.Usage{})
			writer.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				llm.TextBlock("Earlier I ran Bash and reported 12."),
			}}, llm.Usage{})
			if err := writer.Err(); err != nil {
				t.Fatal(err)
			}

			marker := filepath.Join(workDir, "forbidden-command-ran")
			bash, _ := json.Marshal(map[string]string{"command": "touch " + marker})
			summary := "Analyze only the existing file " + filePath + "; recompute its row count and report the verified count, without new collection or network access."
			intent, _ := json.Marshal(map[string]any{"summary": summary, "priority": 8})
			provider := &mainRoleProvider{calls: []llm.ContentBlock{
				{Name: "Bash", Input: bash},
				{Name: "add_intent", Input: intent},
			}}
			main := NewMainAgent(provider, "fixture", workDir, tx, 100000, 3)
			message := "Please recompute the count from " + filePath +
				"\n[Server Planner handoff receipt for this user message]\n{\"hint_id\":42,\"delivery\":\"saved\",\"reason\":\"Task has no open goals; saved until an open goal is available.\"}"
			ctx := WithMainTurnPolicy(t.Context(), MainTurnPolicy{Mode: "main_oneoff", Execute: func(_ context.Context, name string, _ json.RawMessage, run func() (actool.Result, error)) (actool.Result, error) {
				if name != "add_intent" {
					t.Fatalf("unexpected canonical mutation: %s", name)
				}
				return run()
			}})
			ctx = WithInstructionStatusReader(ctx, func(context.Context, []int64) (InstructionStatusPage, error) {
				return InstructionStatusPage{CurrentMessageID: 42, Items: []InstructionStatus{{MessageID: 42, Route: "action", Owner: "main", Delivery: "main", Reason: "Main owns the bounded one-off request."}}}, nil
			})
			reply, err := main.Chat(ctx, store.ID(), 0, nil, store, "Analyze the existing fixture only", message, passHooks{}, nil, nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(provider.reqs) != 1 {
				t.Fatalf("requests=%d, want one rejected response with no model retry", len(provider.reqs))
			}
			if !strings.Contains(reply, "Main will not retry") || !strings.Contains(reply, "`Bash`") || !strings.Contains(reply, "Main owns the bounded one-off request") {
				t.Fatalf("missing authoritative refusal and fresh receipt: %s", reply)
			}
			first := provider.reqs[0]
			if !strings.Contains(strings.Join(first.System, "\n"), mainAgentRoleInstructions) {
				t.Fatal("resuming legacy history displaced current system rules")
			}
			if len(first.Messages) == 0 || !strings.Contains(first.Messages[0].Text(), mainAgentTurnReminder(mainDir)) {
				t.Fatal("actual resumed request lacks the current-turn role/path reminder")
			}
			raw, _ := json.Marshal(first.Messages)
			for _, retained := range []string{"legacy-bash", "Historical fixture count: 12", "Server Planner handoff receipt", filePath} {
				if !strings.Contains(string(raw), retained) {
					t.Fatalf("resumed request lost %q", retained)
				}
			}
			hasDelegation := false
			for _, schema := range first.Tools {
				switch schema.Name {
				case "Bash", "Read", "Write", "WebFetch", "Monitor", "steer_work":
					t.Fatalf("historical execution capability restored: %s", schema.Name)
				case "add_intent":
					hasDelegation = true
				}
			}
			if hasDelegation != delegationEnabled {
				t.Fatal("resumption bypassed current delegation binding")
			}
			intents, err := store.ListByKind(db.KindIntent, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(intents) != 0 {
				t.Fatalf("invalid response dispatched %d intents from its mixed batch", len(intents))
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("historical execution tool ran: %v", err)
			}

			// A separate valid current turn still delegates through the actual
			// canonical handler and ownership fence. Old execution history and the
			// refused batch must neither restore Bash nor poison valid delegation.
			delegation := &mainRoleProvider{calls: []llm.ContentBlock{{Name: "add_intent", Input: intent}}}
			main = NewMainAgent(delegation, "fixture", workDir, tx, 100000, 3)
			if _, err := main.Chat(ctx, store.ID(), 0, nil, store, "Analyze the existing fixture only", "Perform the assigned bounded check of "+filePath, passHooks{}, nil, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			intents, err = store.ListByKind(db.KindIntent, 10)
			if err != nil {
				t.Fatal(err)
			}
			wantIntents := 0
			wantRequests := 1
			if delegationEnabled {
				wantIntents = 1
				wantRequests = 2
			}
			if len(delegation.reqs) != wantRequests {
				t.Fatalf("delegation requests=%d, want %d", len(delegation.reqs), wantRequests)
			}
			if len(intents) != wantIntents {
				t.Fatalf("persisted worker intents=%d, want %d", len(intents), wantIntents)
			}
			if delegationEnabled && !strings.Contains(string(intents[0].Payload), filePath) {
				t.Fatal("worker delegation lost the full file context")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("historical execution tool ran: %v", err)
			}
		})
	}
}
