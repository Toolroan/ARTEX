package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

func TestInstructionStatusToolRereadsLiveDelivery(t *testing.T) {
	states := []string{"queued", "reviewing", "presented"}
	calls := 0
	ctx := WithInstructionStatusReader(t.Context(), func(_ context.Context, ids []int64) (InstructionStatusPage, error) {
		if len(ids) != 1 || ids[0] != 42 {
			t.Fatalf("ids=%v", ids)
		}
		page := InstructionStatusPage{ObservedAt: time.Unix(int64(100+calls), 0).UTC(), CurrentMessageID: 43, ApplicationUnverified: true,
			Items: []InstructionStatus{{MessageID: 42, Owner: "planner", Route: "action", Delivery: states[calls]}}}
		calls++
		return page, nil
	})
	tool := instructionStatusTool(ctx)
	for i, state := range states {
		result, err := tool.Call(ctx, json.RawMessage(`{"message_ids":[42]}`), nil)
		if err != nil || result.IsError {
			t.Fatalf("call=%d result=%v err=%v", i, result, err)
		}
		var page InstructionStatusPage
		if err := json.Unmarshal([]byte(result.Flatten()), &page); err != nil {
			t.Fatal(err)
		}
		if page.Items[0].Delivery != state || !page.ApplicationUnverified || page.ObservedAt.Unix() != int64(100+i) {
			t.Fatalf("stale or overstated receipt: %+v", page)
		}
	}
	if calls != 3 {
		t.Fatalf("reader calls=%d", calls)
	}
}

func TestInstructionStatusToolRejectsInvalidIDsAndUnavailableReads(t *testing.T) {
	if instructionStatusTool(t.Context()) != nil {
		t.Fatal("status tool exposed without a server reader")
	}
	if _, err := ReadInstructionStatus(t.Context(), nil); err == nil {
		t.Fatal("missing reader reported success")
	}
	calls := 0
	ctx := WithInstructionStatusReader(t.Context(), func(context.Context, []int64) (InstructionStatusPage, error) {
		calls++
		return InstructionStatusPage{}, errors.New("private database error MUST NOT LEAK")
	})
	tool := instructionStatusTool(ctx)
	var tooMany []int64
	for i := range 21 {
		tooMany = append(tooMany, int64(i+1))
	}
	rawMany, _ := json.Marshal(map[string]any{"message_ids": tooMany})
	for _, raw := range []string{`null`, `[]`, `{`, `{"message_ids":[0]}`, `{"message_ids":[-1]}`, `{"message_ids":[1,1]}`, `{"message_ids":[1.5]}`, `{"message_id":1}`, `{} {}`, string(rawMany)} {
		result, err := tool.Call(ctx, json.RawMessage(raw), nil)
		if err != nil || !result.IsError || calls != 0 {
			t.Fatalf("invalid input %s reached reader: %v %v calls=%d", raw, result, err, calls)
		}
	}
	result, err := tool.Call(ctx, json.RawMessage(`{}`), nil)
	if err != nil || !result.IsError || calls != 1 || strings.Contains(result.Flatten(), "MUST NOT LEAK") {
		t.Fatalf("unavailable reader misreported or leaked: %v %v", result, err)
	}
}

func TestInstructionStatusReaderDoesNotExposeCallerSlice(t *testing.T) {
	ids := []int64{9}
	ctx := WithInstructionStatusReader(t.Context(), func(_ context.Context, input []int64) (InstructionStatusPage, error) {
		input[0] = 10
		return InstructionStatusPage{}, nil
	})
	if _, err := ReadInstructionStatus(ctx, ids); err != nil || ids[0] != 9 {
		t.Fatal(fmt.Sprintf("caller IDs mutated: %v err=%v", ids, err))
	}
}

func TestInstructionStatusToolRemainsAvailableInReadOnlyMain(t *testing.T) {
	calls := 0
	ctx := WithInstructionStatusReader(t.Context(), func(context.Context, []int64) (InstructionStatusPage, error) {
		calls++
		return InstructionStatusPage{ApplicationUnverified: true}, nil
	})
	ctx = WithMainTurnPolicy(ctx, MainTurnPolicy{Mode: "readonly"})
	mutate := writeTool("add_intent", "must remain unavailable", obj(map[string]any{}), func(context.Context, json.RawMessage) (actool.Result, error) {
		t.Fatal("read-only status access dispatched work")
		return actool.Text(""), nil
	})
	trusted := []actool.CoreTool{instructionStatusTool(ctx), mutate}
	opts := agentcore.Options{Tools: trusted}
	enforceMainAgentRole(&opts, trusted)
	enforceMainTurnPolicy(ctx, &opts)
	if len(opts.Tools) != 1 || opts.Tools[0].Name() != "get_instruction_status" {
		t.Fatalf("wrong readonly capabilities: %v", opts.Tools)
	}
	result, err := opts.Tools[0].Call(ctx, json.RawMessage(`{}`), nil)
	if err != nil || result.IsError || calls != 1 {
		t.Fatalf("readonly Main could not read status: %v %v calls=%d", result, err, calls)
	}
}
