//go:build unix

package server

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Autumn-27/artex/artifacts"
	pgdb "github.com/Autumn-27/artex/db"
)

// Curated metadata refers to task-relative source paths. The bytes and catalogue
// must survive a whole-task archive, even when the restored directory gets a new
// task ID, and must not become a reference to a mutable worker scratch file.
func TestCollectedArtifactsTaskArchiveRoundTripAndPortablePaths(t *testing.T) {
	dataDir := t.TempDir()
	runDir := filepath.Join(dataDir, "tasks", "42", "i7")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(runDir, "export.sql")
	collectedBytes := []byte("-- synthetic archive fixture\nSELECT 1;\n")
	if err := os.WriteFile(sourcePath, collectedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := artifacts.Save(t.Context(), dataDir, 42, runDir, artifacts.SaveRequest{
		Path: "export.sql", Category: "database", Source: "fixture.invalid",
		Description: "Synthetic retained export", Worker: "worker-7", IntentID: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want.OriginalPath != "i7/export.sql" {
		t.Fatalf("source path must be task-relative: %q", want.OriginalPath)
	}
	// A later worker edit cannot silently change the collected snapshot.
	if err := os.WriteFile(sourcePath, []byte("updated scratch contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	stage, err := stageTaskArchiveFiles(dataDir, 101, "42", 73)
	if err != nil {
		t.Fatal(err)
	}
	archivePath := taskArchivePath(dataDir, 101, "42")
	snapshot := &pgdb.TaskArchiveSnapshot{FormatVersion: pgdb.TaskArchiveFormatVersion, TaskID: 42, ExplorationID: 73}
	_, _, checksum, err := writeTaskArchivePackage(archivePath, stage.payload, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := stage.commit(); err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(dataDir, "restore-collected")
	if err := extractTaskArchivePackage(archivePath, checksum, extracted); err != nil {
		t.Fatal(err)
	}
	// The installer uses the task-directory name assigned by its caller. Renaming
	// that extracted tree models restoring to another task without rewriting any
	// collected metadata or inventing a second provenance record.
	if err := os.Rename(filepath.Join(extracted, "files", "tasks", "42"), filepath.Join(extracted, "files", "tasks", "84")); err != nil {
		t.Fatal(err)
	}
	installed, err := installTaskArchiveFiles(dataDir, extracted, "84", 101, ExecPrivilege{})
	if err != nil {
		t.Fatal(err)
	}
	if err := installed.commit(); err != nil {
		t.Fatal(err)
	}
	listing, err := artifacts.List(dataDir, 84, artifacts.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if listing.Total != 1 || len(listing.Items) != 1 || !reflect.DeepEqual(listing.Items[0], want) {
		t.Fatalf("restored catalogue=%+v, want one unchanged entry %+v", listing, want)
	}
	got, file, err := artifacts.Open(dataDir, 84, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	body, err := io.ReadAll(file)
	if err != nil || !bytes.Equal(body, collectedBytes) || !reflect.DeepEqual(got, want) {
		t.Fatalf("restored snapshot entry=%+v body=%q err=%v", got, body, err)
	}
	if raw, err := os.ReadFile(filepath.Join(dataDir, "tasks", "84", "i7", "export.sql")); err != nil || string(raw) != "updated scratch contents" {
		t.Fatalf("ordinary workspace was not preserved separately: %q, %v", raw, err)
	}
}
