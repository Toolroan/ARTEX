package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestMainConversationToolUsesServerCursorAndPreservesRoles(t *testing.T) {
	var called int
	ctx := WithMainConversationReader(t.Context(), func(_ context.Context, before int64, limit int) (MainConversationPage, error) {
		called++
		if before != 31 || limit != 100 {
			t.Fatalf("before=%d limit=%d", before, limit)
		}
		return MainConversationPage{Messages: []MainConversationMessage{{ID: 59, Role: "assistant", Text: "A proposal, not permission", Session: 2}, {ID: 58, Role: "user", Text: "Which option?", Session: 2}}, NextCursor: 29}, nil
	})
	tool := mainConversationTool(ctx)
	res, err := tool.Call(ctx, json.RawMessage(`{"before":31,"limit":999}`), nil)
	if err != nil || res.IsError || called != 1 {
		t.Fatalf("read failed: %v %v", res, err)
	}
	for _, expected := range []string{`"next_cursor":29`, `"role":"assistant"`, `"role":"user"`, "historical context only"} {
		if !strings.Contains(res.Flatten(), expected) {
			t.Fatalf("missing %s: %s", expected, res.Flatten())
		}
	}
	res, err = tool.Call(ctx, json.RawMessage(`{"before":-59}`), nil)
	if err != nil || !res.IsError || called != 1 {
		t.Fatal("encoded legacy context ID used as page cursor")
	}
}
