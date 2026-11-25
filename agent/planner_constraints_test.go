package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestPlannerConstraintGrantIsTypedCanonicalAndBindingAware(t *testing.T) {
	for _, instructionOnly := range []bool{false, true} {
		for _, grant := range []bool{false, true} {
			for _, binding := range []bool{false, true} {
				t.Run(fmt.Sprintf("instruction_%t_grant_%t_binding_%t", instructionOnly, grant, binding), func(t *testing.T) {
					old := ToolResolve()
					t.Cleanup(func() { SetToolResolve(old) })
					var replacement atomic.Int64
					SetToolResolve(func(_ context.Context, _ string, in []actool.CoreTool) []actool.CoreTool {
						out := make([]actool.CoreTool, 0, len(in))
						for _, tool := range in {
							if tool.Name() == "set_constraints" {
								if !binding {
									continue
								}
								tool = mainRoleProbe("set_constraints", &replacement)
							}
							out = append(out, tool)
						}
						if !grant {
							out = append(out, mainRoleProbe("set_constraints", &replacement))
						}
						return out
					})
					store := guardTaskStore(t)
					p := &mainRoleProvider{calls: []llm.ContentBlock{{Name: "set_constraints", Input: json.RawMessage(`{"text":"Do not brute force","type":"deny"}`)}}}
					planner := NewPlanner(p, "fixture", t.TempDir(), nil, 0, 3)
					instruction := PlannerInstruction{ID: 9, Kind: "correction", Text: "Stop brute forcing and save a no-brute-force constraint"}
					if grant {
						instruction.ControlTools = []string{"set_constraints"}
					}
					ctx := WithPlannerInbox(t.Context(), []PlannerInstruction{instruction}, nil)
					if instructionOnly {
						ctx = WithPlannerInstructionOnly(ctx)
					}
					if _, _, err := planner.Plan(ctx, store.ID(), nil, store, "fixture", nil, passHooks{}, nil); err != nil {
						t.Fatal(err)
					}
					rows, err := store.ListConstraints()
					if err != nil {
						t.Fatal(err)
					}
					want := 0
					if grant && binding {
						want = 1
					}
					if len(rows) != want || replacement.Load() != 0 {
						t.Fatalf("constraints=%d want=%d replacement=%d", len(rows), want, replacement.Load())
					}
					if want == 1 && (rows[0].Text != "Do not brute force" || rows[0].Kind != "deny") {
						t.Fatal("constraint content changed")
					}
				})
			}
		}
	}
}
