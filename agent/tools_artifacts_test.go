package agent

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/artifacts"
	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

func TestWorkerPublishesCompletedArtifactAndMainCanListIt(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "tasks", "42", "i7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.sql"), []byte("synthetic database fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := NewToolSet(nil, "worker-3")
	worker.SetWorkBase(base)
	worker.SetTaskID(42)
	worker.SetOwnerNode(7)
	input := json.RawMessage(`{"path":"fixture.sql","category":"database","source":"fixture.example"}`)
	result, err := worker.saveArtifact().Call(t.Context(), input, &actool.ToolContext{WorkingDir: dir})
	if err != nil || result.IsError {
		t.Fatalf("save: %v %s", err, result.Flatten())
	}
	var saved artifacts.Entry
	if err := json.Unmarshal([]byte(result.Flatten()), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Worker != "worker-3" || saved.IntentID != 7 || saved.Category != "database" || saved.ID == "" || saved.SHA256 == "" {
		t.Fatalf("missing host provenance: %+v", saved)
	}
	if worker.Writes().Artifacts != 1 || worker.Writes().Total() != 1 {
		t.Fatal("artifact-only work was counted as no persistence")
	}
	// Editing the original workspace file must not alter the published snapshot.
	if err := os.WriteFile(filepath.Join(dir, "fixture.sql"), []byte("later scratch edits"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, file, err := artifacts.Open(base, 42, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	file.Close()
	if err != nil || string(data) != "synthetic database fixture\n" {
		t.Fatalf("snapshot changed: %q %v", data, err)
	}
	main := NewToolSet(nil, "mainagent")
	main.SetWorkBase(base)
	main.SetTaskID(42)
	result, err = main.listArtifacts().Call(t.Context(), json.RawMessage(`{"category":"database"}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("list: %v %s", err, result.Flatten())
	}
	if !strings.Contains(result.Flatten(), saved.ID) || strings.Contains(result.Flatten(), "synthetic database fixture") {
		t.Fatal("metadata list omitted saved record or exposed content")
	}
	main.SetTaskID(43)
	result, err = main.listArtifacts().Call(t.Context(), json.RawMessage(`{}`), nil)
	if err != nil || result.IsError || strings.Contains(result.Flatten(), saved.ID) {
		t.Fatal("cross-task metadata leak")
	}
}

func TestArtifactToolRefusesForeignRunAndForgedProvenance(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "tasks", "42", "i7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.txt"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := NewToolSet(nil, "worker-3")
	worker.SetWorkBase(base)
	worker.SetTaskID(42)
	worker.SetOwnerNode(7)
	foreignFile := filepath.Join(base, "tasks", "42", "main", "foreign.txt")
	if err := os.MkdirAll(filepath.Dir(foreignFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreignFile, []byte("other run fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreignInput, _ := json.Marshal(map[string]string{"path": foreignFile, "category": "evidence"})
	for _, tc := range []struct{ input, dir string }{
		{string(foreignInput), dir},
		{`{"path":"../main/foreign.txt","category":"evidence"}`, dir},
		{`{"path":"fixture.txt","category":"evidence","worker":"forged","intent_id":99}`, dir},
		{`{"path":"fixture.txt","category":"evidence","task_id":43}`, dir},
		{`{"path":"fixture.txt","category":"evidence"}`, filepath.Join(base, "tasks", "43", "i7")},
		{`{"path":"fixture.txt","category":"evidence"}`, filepath.Join(base, "tasks", "42", "main")},
	} {
		res, err := worker.saveArtifact().Call(t.Context(), json.RawMessage(tc.input), &actool.ToolContext{WorkingDir: tc.dir})
		if err != nil || !res.IsError {
			t.Fatalf("forged context admitted: %v %s", err, res.Flatten())
		}
	}
	if worker.Writes().Total() != 0 {
		t.Fatal("rejected calls counted as persisted artifacts")
	}
	for _, tool := range []actool.CoreTool{NewToolSet(nil, "").saveArtifact(), NewToolSet(nil, "").listArtifacts()} {
		res, err := tool.Call(t.Context(), json.RawMessage(`{"path":"fixture.txt","category":"evidence"}`), nil)
		if err != nil || !res.IsError {
			t.Fatalf("unbound tool %s did not refuse", tool.Name())
		}
	}
}

func TestArtifactToolsStayBoundToCorrectRoles(t *testing.T) {
	seed := map[string]ToolSeed{}
	for _, s := range BuiltinToolSeeds() {
		seed[s.Key] = s
	}
	if !slices.Equal(seed["save_artifact"].Agents, []string{"worker"}) {
		t.Fatalf("save bindings: %v", seed["save_artifact"].Agents)
	}
	for _, role := range []string{"mainagent", "planner", "worker"} {
		if !slices.Contains(seed["list_artifacts"].Agents, role) {
			t.Fatalf("list not bound to %s", role)
		}
	}
	tools := NewToolSet(nil, "mainagent")
	trusted := mainAgentOrchestrationTools(tools)
	opts := agentcore.Options{Tools: append(append([]actool.CoreTool{}, trusted...), tools.saveArtifact())}
	enforceMainAgentRole(&opts, trusted)
	enforceMainTurnPolicy(WithMainTurnPolicy(t.Context(), MainTurnPolicy{Mode: "readonly"}), &opts)
	found := false
	for _, tool := range opts.Tools {
		if tool.Name() == "save_artifact" {
			t.Fatal("Main received artifact mutation")
		}
		if tool.Name() == "list_artifacts" {
			found = true
		}
	}
	if !found {
		t.Fatal("Main cannot inspect collection metadata")
	}
	if BudgetWrapupBlocks("save_artifact") || !toolRAGKeep(nil)["save_artifact"] || !toolRAGKeep(nil)["list_artifacts"] {
		t.Fatal("retention tools lost during wrap-up or tool projection")
	}
}

func TestWorkerCollectedArtifactGuidanceSurvivesPromptOverride(t *testing.T) {
	old := PromptOverride()
	defer SetPromptOverride(old)
	SetPromptOverride(func(string) (string, bool) { return "custom task instructions", true })
	prompt := workerSystem("", "", "/fixture", "/fixture/tasks/42/i7")
	for _, text := range []string{"save_artifact", "list_artifacts", "Helper scripts", "already-collected", "do not authorize any additional collection"} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("code-owned retention guidance missing %q", text)
		}
	}
}
