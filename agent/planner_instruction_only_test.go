package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestPlannerInstructionOnlyCanCorrectWithoutCompletingGoallessTask(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("steering_bound_%t", enabled), func(t *testing.T) {
			oldAugment, oldResolve := ToolAugment(), ToolResolve()
			t.Cleanup(func() { SetToolAugment(oldAugment); SetToolResolve(oldResolve) })
			var augmented, replaced, steered atomic.Int64
			SetToolAugment(func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
				augmented.Add(1)
				return []actool.CoreTool{mainRoleProbe("mcp_exec", &replaced)}, DeferredInfo{}, nil
			})
			SetToolResolve(func(_ context.Context, _ string, in []actool.CoreTool) []actool.CoreTool {
				out := make([]actool.CoreTool, 0, len(in))
				for i, tool := range in {
					if tool.Name() == "steer_work" {
						in[i] = mainRoleProbe("steer_work", &replaced)
						tool = in[i]
						if !enabled {
							continue
						}
					}
					out = append(out, tool)
				}
				return append(out, mainRoleProbe("Bash", &replaced), mainRoleProbe("prove_goal", &replaced))
			})
			store := guardTaskStore(t)
			intentID := addGuardNode(t, store, db.KindIntent, map[string]any{"summary": "existing bounded saved-file check"}, "running")
			workDir := t.TempDir()
			marker := filepath.Join(workDir, "must-not-execute")
			bash, _ := json.Marshal(map[string]string{"command": "touch " + marker})
			steer, _ := json.Marshal(map[string]any{"intent_id": intentID, "message": "Use the existing saved CSV only"})
			p := &mainRoleProvider{calls: []llm.ContentBlock{{Name: "Bash", Input: bash}, {Name: "Monitor", Input: bash}, {Name: "goal_met", Input: json.RawMessage(`{}`)}, {Name: "prove_goal", Input: json.RawMessage(`{}`)}, {Name: "steer_work", Input: steer}}}
			planner := NewPlanner(p, "fixture", workDir, nil, 0, 3)
			planner.SetNoaEnabled(func() bool { return true })
			planner.SetWebSearch(WebSearchOpts{Enabled: true})
			planner.SetSteerWork(func(id int64, message string) error {
				if id != intentID || message != "Use the existing saved CSV only" {
					t.Error("correction changed")
				}
				steered.Add(1)
				return nil
			})
			ctx := WithPlannerInstructionOnly(WithPlannerInbox(t.Context(), []PlannerInstruction{{ID: 8, Kind: "correction", Text: "Use the existing saved CSV only"}}, nil))
			met, _, err := planner.Plan(ctx, store.ID(), nil, store, "Existing saved-file check", nil, passHooks{}, nil)
			if err != nil || met {
				t.Fatalf("instruction review completed task: met=%v err=%v", met, err)
			}
			if len(p.reqs) != 2 {
				t.Fatalf("provider calls=%d", len(p.reqs))
			}
			if augmented.Load() != 0 || replaced.Load() != 0 {
				t.Fatal("instruction-only mode invoked augmentation or resolver replacement")
			}
			want := int64(0)
			if enabled {
				want = 1
			}
			if steered.Load() != want {
				t.Fatalf("steered=%d want=%d", steered.Load(), want)
			}
			for _, tool := range p.reqs[0].Tools {
				if !mainReadTool(tool.Name) && tool.Name != "list_goals" && tool.Name != "steer_work" && tool.Name != "kill_work" && tool.Name != "add_intent" {
					t.Fatalf("unsafe instruction-mode tool %s", tool.Name)
				}
			}
			input := p.reqs[0].Messages[len(p.reqs[0].Messages)-1].Text()
			if strings.Contains(input, "Hard floor") || strings.Contains(input, "must produce") || !strings.Contains(strings.Join(p.reqs[0].System, "\n"), "instruction-only coordination round") {
				t.Fatal("autonomous planning instruction leaked into correction round")
			}
			if nodeState(t, store, intentID) != "running" {
				t.Fatal("correction review changed worker lifecycle")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("execution occurred: %v", err)
			}
		})
	}
}
