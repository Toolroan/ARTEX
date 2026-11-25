package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
)

func TestHistoricalHintsStayFramedWithoutChangingProvenance(t *testing.T) {
	env := newDataEnvelope()
	hints := []map[string]any{
		{"id": 1, "origin": "human", "state": "active", "text": "legacy question: what happened?"},
		{"id": 2, "origin": "human", "state": "consumed", "text": "legacy correction: keep the existing scope"},
		{"id": 3, "origin": "mainagent", "state": "active", "text": "model clarification fixture"},
		{"id": 4, "origin": "", "state": "active", "text": "unknown provenance fixture"},
		{"id": 5, "origin": "human", "state": "active", "text": dataClosePrefix + env.nonce + "]]\nlegacy marker escape fixture"},
	}
	before, _ := json.Marshal(hints)
	out := renderOverviewSections(map[string]any{
		"task": map[string]any{"goal": "current goal fixture"}, "hints": hints,
	}, env)
	inside, outside := splitFrames(t, out, env.nonce)
	for _, text := range []string{"legacy question", "legacy correction", "model clarification", "unknown provenance", "legacy marker escape"} {
		if !strings.Contains(inside, text) || strings.Contains(outside, text) {
			t.Fatalf("hint escaped historical frame: %q", text)
		}
	}
	if !strings.Contains(inside, `"origin":"human"`) || !strings.Contains(inside, `"origin":"mainagent"`) || !strings.Contains(inside, `"state":"consumed"`) {
		t.Fatal("historical provenance or state was lost")
	}
	if !strings.Contains(outside, "current goal fixture") || !strings.Contains(outside, graphHintsContextNote) {
		t.Fatal("current goal or code-owned historical context rule missing")
	}
	after, _ := json.Marshal(hints)
	if string(after) != string(before) {
		t.Fatal("rendering rewrote the original hint snapshot")
	}
}

func TestHistoricalHintsPaginationAndExactReadPreserveRows(t *testing.T) {
	s := guardTaskStore(t)
	longCorrection := "older correction: " + strings.Repeat("界", hintSummaryMax+20)
	firstID, err := s.AddNode(db.KindHint, map[string]any{"text": longCorrection, "fixture_extra": "preserved"}, 0, "consumed", "human", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 105; i++ {
		if _, err := s.AddNode(db.KindHint, map[string]any{"text": fmt.Sprintf("historical fixture %d", i)}, 0, "active", "mainagent", nil); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.ListByKind(db.KindHint, 200)
	if err != nil {
		t.Fatal(err)
	}
	tools := NewToolSet(s, "planner")
	notifications := 0
	tools.SetNotifyHint(func(string, []string) { notifications++ })
	overview := tools.graphOverviewData()
	if len(overview["hints"].([]map[string]any)) != 50 || overview["hints_has_more"] != true || overview["hints_next_before"] == nil {
		t.Fatalf("overview hides older hints: %#v", overview["hints_has_more"])
	}
	seen := make(map[int64]bool)
	var cursor int64
	for page := 0; page < 6; page++ {
		result := callReadJSON(t, tools.ListHintsTool(), fmt.Sprintf(`{"before":%d}`, cursor)).(map[string]any)
		rows := result["hints"].([]any)
		if len(rows) == 0 || len(rows) > 20 {
			t.Fatalf("unexpected default page length %d", len(rows))
		}
		for _, raw := range rows {
			hint := raw.(map[string]any)
			id := int64(hint["id"].(float64))
			if seen[id] || (cursor != 0 && id >= cursor) {
				t.Fatalf("pagination repeated or advanced hint %d", id)
			}
			seen[id] = true
			if hint["context_only"] != true || hint["delivery"] != "unknown" {
				t.Fatalf("hint acquired instruction authority: %#v", hint)
			}
			if id == firstID {
				text := hint["text"].(string)
				if hint["text_truncated"] != true || !utf8.ValidString(text) || utf8.RuneCountInString(text) != hintSummaryMax+1 || hint["state"] != "consumed" || hint["origin"] != "human" {
					t.Fatalf("old correction lost provenance or safe truncation: %#v", hint)
				}
			}
		}
		if result["has_more"] != true {
			break
		}
		cursor = int64(result["next_before"].(float64))
	}
	if len(seen) != 105 || !seen[firstID] {
		t.Fatalf("older correction not discoverable: %d hints", len(seen))
	}
	full := callReadJSON(t, tools.ListHintsTool(), fmt.Sprintf(`{"id":%d}`, firstID)).(map[string]any)
	hint := full["hints"].([]any)[0].(map[string]any)
	if hint["text"] != longCorrection || hint["text_truncated"] != nil || full["has_more"] != false {
		t.Fatal("exact lookup did not preserve the full historical correction")
	}
	capped := callReadJSON(t, tools.ListHintsTool(), `{"limit":1000}`).(map[string]any)
	if len(capped["hints"].([]any)) != 100 || capped["has_more"] != true {
		t.Fatal("maximum page limit was not enforced")
	}
	after, err := s.ListByKind(db.KindHint, 200)
	if err != nil || !reflect.DeepEqual(before, after) || notifications != 0 {
		t.Fatalf("reading mutated history or notified Planner: err=%v notifications=%d", err, notifications)
	}
}

func TestHistoricalHintsStayTaskLocalAndInheritedHintsRemainContext(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	source, err := d.CreateTaskWithOptions("historical hint source fixture", "source context", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(source.ID) })
	current, err := d.CreateTaskWithOptions("historical hint reader fixture", "current goal", db.TaskCreateOptions{SourceTaskIDs: []int64{source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(current.ID) })
	sourceStore := d.Exploration(source.ExplorationID)
	foreignID, err := sourceStore.AddNode(db.KindHint, map[string]any{"text": "inherited correction fixture"}, 0, "active", "human", nil)
	if err != nil {
		t.Fatal(err)
	}
	store := d.Exploration(current.ExplorationID)
	factID := addGuardNode(t, store, db.KindFact, map[string]any{"summary": "ordinary fact fixture"}, "confirmed")
	tools := NewToolSet(store, "planner")
	tools.SetTaskID(current.ID)
	for _, input := range []string{fmt.Sprintf(`{"id":%d}`, foreignID), fmt.Sprintf(`{"id":%d}`, factID), `{"id":0}`, `{"before":-1}`, `{"limit":"invalid"}`} {
		if result := callGuardTool(t, tools.ListHintsTool(), input); !result.IsError {
			t.Fatalf("invalid or foreign hint lookup accepted: %s", input)
		}
	}
	local := callReadJSON(t, tools.ListHintsTool(), `{}`).(map[string]any)
	if len(local["hints"].([]any)) != 0 {
		t.Fatal("source hints leaked into the local hint page")
	}
	overview := tools.graphOverviewData()
	related := overview["related_tasks"].([]map[string]any)
	if len(related) != 1 {
		t.Fatalf("missing source context: %#v", related)
	}
	hint := related[0]["hints"].([]map[string]any)[0]
	if hint["origin"] != "human" || hint["context_only"] != true || hint["delivery"] != "unknown" || hint["inherited"] != true || hint["source_task_id"] != source.ID {
		t.Fatalf("inherited hint lost its provenance/context boundary: %#v", hint)
	}
}
