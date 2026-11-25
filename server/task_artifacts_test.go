//go:build unix

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/artifacts"
)

func artifactServerFixture(t *testing.T) (*Server, string) {
	t.Helper()
	base := t.TempDir()
	run := filepath.Join(base, "tasks", "7", "i8")
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "fixture.conf"), []byte("synthetic=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{dir: base, tasks: map[string]*Task{"7": {ID: "7"}, "8": {ID: "8"}}}
	return &Server{m: m, engine: NewEngine(m)}, run
}

func artifactRequest(h http.HandlerFunc, method, path, body, taskID, artifactID string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.SetPathValue("id", taskID)
	r.SetPathValue("artifact", artifactID)
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func TestTaskArtifactRoutesRequireAuthentication(t *testing.T) {
	s, _ := newReadyServer(t, func(context.Context) error { return nil })
	h := s.Handler()
	for _, tc := range []struct{ method, path string }{{"GET", "/api/tasks/7/artifacts"}, {"POST", "/api/tasks/7/artifacts"}, {"GET", "/api/tasks/7/artifacts/id/download"}, {"GET", "/api/tasks/7/artifacts/id/preview"}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`)))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status=%d", tc.method, tc.path, w.Code)
		}
	}
}

func TestTaskArtifactHTTPPromotionPreviewAndDownload(t *testing.T) {
	s, run := artifactServerFixture(t)
	body := `{"path":"tasks/7/i8/fixture.conf","category":"config","source":"https://user:password@fixture.invalid/a?secret=1#f","description":"Synthetic fixture"}`
	w := artifactRequest(s.saveTaskArtifact, "POST", "/", body, "7", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("save=%d %s", w.Code, w.Body)
	}
	var entry artifacts.Entry
	if err := json.Unmarshal(w.Body.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Worker != "operator" || entry.IntentID != 0 || entry.Source != "https://fixture.invalid/a" {
		t.Fatalf("bad operator provenance: %+v", entry)
	}
	if err := os.WriteFile(filepath.Join(run, "fixture.conf"), []byte("later scratch"), 0o600); err != nil {
		t.Fatal(err)
	}
	w = artifactRequest(s.listTaskArtifacts, "GET", "/?category=config&q=fixture&limit=1", "", "7", "")
	var listing artifacts.Listing
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || listing.Total != 1 || len(listing.Items) != 1 || listing.Items[0] != entry {
		t.Fatalf("list=%d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "synthetic=true") {
		t.Fatal("list disclosed content")
	}
	w = artifactRequest(s.previewTaskArtifact, "GET", "/", "", "7", entry.ID)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"content":"synthetic=true\n"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("preview=%d %s", w.Code, w.Body)
	}
	w = artifactRequest(s.downloadTaskArtifact, "GET", "/", "", "7", entry.ID)
	if w.Code != 200 || w.Body.String() != "synthetic=true\n" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("download=%d %s", w.Code, w.Body)
	}
	for _, id := range []string{"8", "99"} {
		w = artifactRequest(s.downloadTaskArtifact, "GET", "/", "", id, entry.ID)
		if w.Code != 404 {
			t.Errorf("cross-task download %s=%d", id, w.Code)
		}
	}
	if s.engine.inflightCount("7") != 0 {
		t.Fatal("HTTP operation leaked inflight registration")
	}
}

func TestTaskArtifactHTTPRejectsForgedPathsMetadataAndLifecycle(t *testing.T) {
	s, _ := artifactServerFixture(t)
	for _, body := range []string{
		`{"path":"tasks/8/main/secret","category":"config"}`,
		`{"path":"tasks/7/i8/../i8/fixture.conf","category":"config"}`,
		`{"path":"/tasks/7/i8/fixture.conf","category":"config"}`,
		`{"path":"tasks/7/i8/fixture.conf","category":"config","worker":"forged"}`,
		`{"path":"tasks/7/i8/fixture.conf","category":"config","intent_id":8}`,
		`{"path":"tasks/7/i8/fixture.conf","category":"scripts"}`,
		`{"path":"tasks/7/i8/fixture.conf","category":"config"} {}`,
	} {
		w := artifactRequest(s.saveTaskArtifact, "POST", "/", body, "7", "")
		if w.Code != 400 {
			t.Errorf("invalid request=%d %s", w.Code, w.Body)
		}
	}
	for _, query := range []string{"?limit=501", "?offset=-1", "?limit=nope", "?category=scripts"} {
		w := artifactRequest(s.listTaskArtifacts, "GET", "/"+query, "", "7", "")
		if w.Code != 400 {
			t.Errorf("query %s=%d", query, w.Code)
		}
	}
	s.engine.deleting.Store("7", true)
	for _, h := range []http.HandlerFunc{s.listTaskArtifacts, s.saveTaskArtifact, s.previewTaskArtifact, s.downloadTaskArtifact} {
		w := artifactRequest(h, "GET", "/", "{}", "7", "abc")
		if w.Code != 409 {
			t.Errorf("deleting task accepted status=%d", w.Code)
		}
	}
	s.engine.deleting.Delete("7")
	s.engine.draining = true
	w := artifactRequest(s.listTaskArtifacts, "GET", "/", "", "7", "")
	if w.Code != 409 {
		t.Fatalf("draining task accepted status=%d", w.Code)
	}
	if s.engine.inflightCount("7") != 0 {
		t.Fatal("rejected HTTP operation leaked inflight registration")
	}
}

func TestTaskArtifactPreviewDoesNotInlineBinaryOrLargeFiles(t *testing.T) {
	s, run := artifactServerFixture(t)
	for _, tc := range []struct {
		name, content string
		large         bool
	}{{"fixture.bin", "one\x00two", false}, {"large.txt", strings.Repeat("x", maxWorkspaceRead+1), true}} {
		if err := os.WriteFile(filepath.Join(run, tc.name), []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		e, err := artifacts.Save(t.Context(), s.m.dir, 7, run, artifacts.SaveRequest{Path: tc.name, Category: "evidence"})
		if err != nil {
			t.Fatal(err)
		}
		w := artifactRequest(s.previewTaskArtifact, "GET", "/", "", "7", e.ID)
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || result["binary"] != true || result["too_large"] != tc.large || result["content"] != nil {
			t.Fatalf("unsafe preview=%d %s", w.Code, w.Body)
		}
	}
}

func TestCollectedArtifactsAreProtectedFromWorkspaceMutations(t *testing.T) {
	s, run := artifactServerFixture(t)
	e, err := artifacts.Save(t.Context(), s.m.dir, 7, run, artifacts.SaveRequest{Path: "fixture.conf", Category: "config"})
	if err != nil {
		t.Fatal(err)
	}
	root := "tasks/7/collected"
	path := root + "/" + e.ID + "/content"
	if err := os.Symlink(filepath.Join(s.m.dir, root), filepath.Join(s.m.dir, "alias")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{root, path, "alias/" + e.ID + "/content", "tasks/7/COLLECTED/" + e.ID + "/content", "tasks/7/collected./new"} {
		if _, ok := s.wsResolveConfined(rel); ok {
			t.Errorf("workspace accepted protected path %q", rel)
		}
	}
	for _, rel := range []string{root, path, "tasks/7", "tasks"} {
		w := httptest.NewRecorder()
		s.wsDelete(w, httptest.NewRequest("DELETE", "/api/workspace/delete?path="+url.QueryEscape(rel), nil))
		if w.Code != 400 {
			t.Errorf("workspace delete %s=%d", rel, w.Code)
		}
	}
	for _, tc := range []struct {
		h                  http.HandlerFunc
		method, path, body string
	}{
		{s.wsWrite, "POST", "/", `{"path":"` + path + `","content":"changed"}`},
		{s.wsMkdir, "POST", "/", `{"path":"` + root + `/planted"}`},
		{s.wsUpload, "POST", "/?path=" + url.QueryEscape(root), ""},
		{s.wsRead, "GET", "/?path=" + url.QueryEscape(path), ""},
		{s.wsDownload, "GET", "/?path=" + url.QueryEscape(path), ""},
	} {
		w := httptest.NewRecorder()
		tc.h(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != 400 {
			t.Errorf("workspace handler accepted collection=%d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.wsList(w, httptest.NewRequest("GET", "/?path=tasks/7", nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), `"name":"collected"`) {
		t.Fatalf("workspace shows private store: %d %s", w.Code, w.Body)
	}
	w = artifactRequest(s.downloadTaskArtifact, "GET", "/", "", "7", e.ID)
	if w.Code != 200 || w.Body.String() != "synthetic=true\n" {
		t.Fatal("workspace mutation damaged snapshot")
	}
}
