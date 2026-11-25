package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestPlannerDeliveryFenceChecksAtHandlerAdmission(t *testing.T) {
	var called, checked atomic.Int64
	tool := mainRoleProbe("steer_work", &called)
	opts := agentcore.Options{Tools: []actool.CoreTool{tool}, MaxTurns: 7}
	valid := true
	ctx := WithPlannerDeliveryFence(t.Context(), func(context.Context) error {
		checked.Add(1)
		if !valid {
			return errors.New("lease expired")
		}
		return nil
	})
	applyPlannerDeliveryFence(ctx, &opts)
	// Simulate a lease expiring after model/tool selection or an approval wait.
	valid = false
	res, err := opts.Tools[0].Call(ctx, json.RawMessage(`{}`), nil)
	if err != nil || !res.IsError || called.Load() != 0 || checked.Load() != 1 {
		t.Fatalf("expired lease admitted handler: %+v %v called=%d checks=%d", res, err, called.Load(), checked.Load())
	}
	valid = true
	res, err = opts.Tools[0].Call(ctx, json.RawMessage(`{}`), nil)
	if err != nil || res.IsError || called.Load() != 1 || opts.MaxTurns != 7 {
		t.Fatal("valid lease failed or budget changed")
	}
	untagged := agentcore.Options{Tools: []actool.CoreTool{tool}}
	applyPlannerDeliveryFence(t.Context(), &untagged)
	if untagged.Tools[0] != tool {
		t.Fatal("untagged planner handler changed")
	}
}

type deliveryTestHooks struct {
	pre  func() (bool, string, []byte)
	post bool
}

func (h *deliveryTestHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return h.pre()
}
func (h *deliveryTestHooks) PostToolUse(context.Context, string, []byte, []byte, bool) { h.post = true }
func (h *deliveryTestHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return true, []string{"budget stop"}, "stop preserved"
}

func TestPlannerDeliveryFenceRechecksAfterApprovalAndPreservesHooks(t *testing.T) {
	for _, innerBlock := range []bool{false, true} {
		t.Run(fmt.Sprintf("inner_block_%t", innerBlock), func(t *testing.T) {
			valid := true
			checks := 0
			inner := &deliveryTestHooks{pre: func() (bool, string, []byte) {
				valid = false
				return innerBlock, "guard refusal", []byte(`{"rewritten":true}`)
			}}
			opts := agentcore.Options{Hooks: inner}
			ctx := WithPlannerDeliveryFence(t.Context(), func(context.Context) error {
				checks++
				if !valid {
					return errors.New("expired during approval")
				}
				return nil
			})
			applyPlannerDeliveryFence(ctx, &opts)
			blocked, message, updated := opts.Hooks.PreToolUse(ctx, "Monitor", []byte(`{}`))
			if !blocked || string(updated) != `{"rewritten":true}` {
				t.Fatal("post-approval lease guard failed or lost rewrite")
			}
			if innerBlock {
				if message != "guard refusal" || checks != 0 {
					t.Fatal("inner safety block lost precedence")
				}
			} else if message != plannerDeliveryDenied || checks != 1 {
				t.Fatal("lease was not checked after approval")
			}
			opts.Hooks.PostToolUse(ctx, "Monitor", nil, nil, true)
			if !inner.post {
				t.Fatal("PostToolUse lost")
			}
			prevent, errs, msg := opts.Hooks.Stop(ctx, nil)
			if !prevent || len(errs) != 1 || msg != "stop preserved" {
				t.Fatal("Stop budget/safety outcome lost")
			}
			valid = true
			inner.pre = func() (bool, string, []byte) { return false, "", []byte(`{"safe":true}`) }
			blocked, _, updated = opts.Hooks.PreToolUse(ctx, "Monitor", nil)
			if blocked || string(updated) != `{"safe":true}` {
				t.Fatal("valid lease lost allowed rewrite")
			}
		})
	}
}
