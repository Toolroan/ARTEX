package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/artex/artifacts"
)

// Every route also passes through requireAuth. Task operations prevent archive,
// deletion or server drain from racing the source/snapshot filesystem work.
func (s *Server) artifactTask(w http.ResponseWriter, r *http.Request) (*Task, func(), bool) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "task not found")
		return nil, nil, false
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, http.StatusConflict, "Task is being deleted or server is draining")
		return nil, nil, false
	}
	return t, func() { s.engine.decInflight(t.ID) }, true
}

func artifactHTTPError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "Artifact operation failed"
	switch {
	case errors.Is(err, artifacts.ErrNotFound):
		status = http.StatusNotFound
		message = err.Error()
	case errors.Is(err, artifacts.ErrInvalid):
		status = http.StatusBadRequest
		message = err.Error()
	case errors.Is(err, artifacts.ErrTooLarge):
		status = http.StatusRequestEntityTooLarge
		message = err.Error()
	case errors.Is(err, artifacts.ErrChanged), errors.Is(err, artifacts.ErrCorrupt):
		status = http.StatusConflict
		message = err.Error()
	}
	writeErr(w, status, message)
}

func (s *Server) listTaskArtifacts(w http.ResponseWriter, r *http.Request) {
	t, done, ok := s.artifactTask(w, r)
	if !ok {
		return
	}
	defer done()
	q := artifacts.Query{Category: r.URL.Query().Get("category"), Search: r.URL.Query().Get("q")}
	for key, dest := range map[string]*int{"limit": &q.Limit, "offset": &q.Offset} {
		if raw := r.URL.Query().Get(key); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				writeErr(w, 400, "Invalid pagination")
				return
			}
			*dest = n
		}
	}
	listing, err := artifacts.List(s.m.dir, routingTaskID(t.ID), q)
	if err != nil {
		artifactHTTPError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, listing)
}

func (s *Server) saveTaskArtifact(w http.ResponseWriter, r *http.Request) {
	t, done, ok := s.artifactTask(w, r)
	if !ok {
		return
	}
	defer done()
	var body struct {
		Path        string `json:"path"`
		Category    string `json:"category"`
		Source      string `json:"source"`
		Description string `json:"description"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF {
		writeErr(w, 400, "Invalid or oversized artifact request")
		return
	}
	// Operator promotion is selected from the workspace browser. Resolve only a
	// workspace-relative task path; provenance is supplied by this handler.
	prefix := "tasks/" + t.ID + "/"
	if !strings.HasPrefix(body.Path, prefix) || strings.Contains(body.Path, "\\") || strings.ContainsRune(body.Path, 0) {
		writeErr(w, 400, "Source must be an existing file in this task")
		return
	}
	rel := strings.TrimPrefix(body.Path, prefix)
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			writeErr(w, 400, "Invalid source path")
			return
		}
	}
	taskDir := filepath.Join(s.m.dir, "tasks", t.ID)
	entry, err := artifacts.Save(r.Context(), s.m.dir, routingTaskID(t.ID), taskDir, artifacts.SaveRequest{Path: rel, Category: body.Category, Source: body.Source, Description: body.Description, Worker: "operator"})
	if err != nil {
		artifactHTTPError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, entry)
}

func (s *Server) downloadTaskArtifact(w http.ResponseWriter, r *http.Request) {
	t, done, ok := s.artifactTask(w, r)
	if !ok {
		return
	}
	defer done()
	entry, file, err := artifacts.Open(s.m.dir, routingTaskID(t.ID), r.PathValue("artifact"))
	if err != nil {
		artifactHTTPError(w, err)
		return
	}
	defer file.Close()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+sanitizeFilename(entry.Name)+"\"; filename*=UTF-8''"+url.PathEscape(entry.Name))
	http.ServeContent(w, r, entry.Name, time.UnixMilli(entry.CreatedAt), file)
}

func (s *Server) previewTaskArtifact(w http.ResponseWriter, r *http.Request) {
	t, done, ok := s.artifactTask(w, r)
	if !ok {
		return
	}
	defer done()
	entry, file, err := artifacts.Open(s.m.dir, routingTaskID(t.ID), r.PathValue("artifact"))
	if err != nil {
		artifactHTTPError(w, err)
		return
	}
	defer file.Close()
	result := map[string]any{"id": entry.ID, "name": entry.Name, "size": entry.Size, "binary": false, "too_large": false}
	if entry.Size > maxWorkspaceRead {
		result["binary"] = true
		result["too_large"] = true
	} else {
		body, err := io.ReadAll(io.LimitReader(file, maxWorkspaceRead+1))
		if err != nil {
			writeErr(w, 500, "Could not read artifact preview")
			return
		}
		if int64(len(body)) != entry.Size {
			artifactHTTPError(w, artifacts.ErrCorrupt)
			return
		}
		if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
			result["binary"] = true
		} else {
			result["content"] = string(body)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeJSON(w, http.StatusOK, result)
}
