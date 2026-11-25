//go:build unix

package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func fixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	run := filepath.Join(base, "tasks", "7", "i8")
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	return base, run
}

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSaveSnapshotIdempotenceAndProvenance(t *testing.T) {
	base, run := fixture(t)
	source := filepath.Join(run, "config.env")
	put(t, source, "synthetic=true\n")
	req := SaveRequest{Path: "config.env", Category: "config", Source: "https://name:secret@fixture.invalid/a?token=secret#fragment", Worker: "worker-2", IntentID: 8, Description: "Synthetic configuration"}
	one, err := Save(t.Context(), base, 7, run, req)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Save(t.Context(), base, 7, run, req)
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatalf("retry not idempotent: %+v %+v", one, two)
	}
	if one.Source != "https://fixture.invalid/a" || one.OriginalPath != "i8/config.env" || one.Worker != "worker-2" || one.IntentID != 8 {
		t.Fatalf("bad provenance: %+v", one)
	}
	sum := sha256.Sum256([]byte("synthetic=true\n"))
	if one.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("incorrect checksum")
	}
	put(t, source, "later edits")
	entry, f, err := Open(base, 7, one.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || entry != one || string(body) != "synthetic=true\n" {
		t.Fatalf("snapshot changed: %q %v", body, err)
	}
	three, err := Save(t.Context(), base, 7, run, req)
	if err != nil || three.ID == one.ID {
		t.Fatalf("changed source not versioned: %+v %v", three, err)
	}
	listing, err := List(base, 7, Query{Category: "config", Search: "CONFIG.ENV", Limit: 1})
	if err != nil || listing.Total != 2 || len(listing.Items) != 1 {
		t.Fatalf("listing: %+v %v", listing, err)
	}
	page, err := List(base, 7, Query{Offset: 1, Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID == listing.Items[0].ID {
		t.Fatalf("pagination: %+v %v", page, err)
	}
	other, err := List(base, 99, Query{})
	if err != nil || other.Total != 0 || other.Items == nil {
		t.Fatalf("other task: %+v %v", other, err)
	}
}

func TestConcurrentIdenticalSnapshotsPublishOnce(t *testing.T) {
	base, run := fixture(t)
	put(t, filepath.Join(run, "db.sql"), strings.Repeat("fixture\n", 1024))
	var wg sync.WaitGroup
	entries := make(chan Entry, 12)
	errs := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			e, err := Save(t.Context(), base, 7, run, SaveRequest{Path: "db.sql", Category: "database", Worker: "worker-1", IntentID: 8})
			if err != nil {
				errs <- err
			} else {
				entries <- e
			}
		})
	}
	wg.Wait()
	close(entries)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var first Entry
	for e := range entries {
		if first.ID == "" {
			first = e
		} else if first != e {
			t.Fatal("duplicate publications disagree")
		}
	}
	list, err := List(base, 7, Query{})
	if err != nil || list.Total != 1 {
		t.Fatalf("duplicate entries: %+v %v", list, err)
	}
	names, err := os.ReadDir(filepath.Join(base, "tasks", "7", Directory))
	if err != nil || len(names) != 1 {
		t.Fatalf("staging leaked: %v %v", names, err)
	}
}

func TestSaveRejectsUnsafeSourcesAndLeavesNothingPublished(t *testing.T) {
	base, run := fixture(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	put(t, outside, "outside fixture")
	put(t, filepath.Join(run, "inside.txt"), "inside fixture")
	if err := os.Symlink(outside, filepath.Join(run, "out-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("inside.txt", filepath.Join(run, "in-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(run, "dir-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(run, "hard-link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(run, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, "../i8/inside.txt", "../../../../secret.txt", "out-link", "in-link", "dir-link/secret.txt", "hard-link", "pipe", ".", "missing", run, filepath.Join(base, "tasks", "8", "main", "file")} {
		if _, err := Save(t.Context(), base, 7, run, SaveRequest{Path: path, Category: "evidence"}); err == nil {
			t.Errorf("unsafe source accepted: %q", path)
		}
	}
	listing, err := List(base, 7, Query{})
	if err != nil || listing.Total != 0 {
		t.Fatalf("unsafe save published data: %+v %v", listing, err)
	}
	// A task/store symlink itself must never redirect privileged publication.
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(base, "tasks", "7", Directory)); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(t.Context(), base, 7, run, SaveRequest{Path: "inside.txt", Category: "evidence"}); err == nil {
		t.Fatal("symlink store accepted")
	}
	if got, _ := os.ReadFile(outside); string(got) != "outside fixture" {
		t.Fatal("outside file changed")
	}
}

func TestSaveValidationCancellationAndSizeLimits(t *testing.T) {
	base, run := fixture(t)
	put(t, filepath.Join(run, "fixture.txt"), "fixture")
	for _, req := range []SaveRequest{
		{Path: "fixture.txt", Category: "scripts"},
		{Path: "fixture.txt", Category: "other", Description: strings.Repeat("a", 2001)},
		{Path: "fixture.txt", Category: "other", Source: "hello\nworld"},
		{Path: "fixture.txt", Category: "other", Worker: "bad\nworker"},
		{Path: "fixture.txt", Category: "other", IntentID: -1},
	} {
		if _, err := Save(t.Context(), base, 7, run, req); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid metadata accepted: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Save(ctx, base, 7, run, SaveRequest{Path: "fixture.txt", Category: "other"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	large, err := os.Create(filepath.Join(run, "large.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate(MaxFileSize + 1); err != nil {
		t.Fatal(err)
	}
	_ = large.Close()
	if _, err := Save(t.Context(), base, 7, run, SaveRequest{Path: "large.sql", Category: "database"}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("size limit: %v", err)
	}
	listing, err := List(base, 7, Query{})
	if err != nil || listing.Total != 0 {
		t.Fatalf("failed save published: %+v %v", listing, err)
	}
	for _, q := range []Query{{Limit: 501}, {Offset: -1}, {Category: "scripts"}, {Search: strings.Repeat("a", 257)}} {
		if _, err := List(base, 7, q); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid query accepted: %v", err)
		}
	}
}

type cancelWriter struct {
	cancel context.CancelFunc
	n      int
}

func (w *cancelWriter) Write(p []byte) (int, error) { w.n += len(p); w.cancel(); return len(p), nil }
func TestSnapshotCopyChecksCancellationBetweenChunks(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := &cancelWriter{cancel: cancel}
	n, err := copySnapshot(ctx, w, strings.NewReader(strings.Repeat("x", 2*1024*1024)))
	if !errors.Is(err, context.Canceled) || n != 1024*1024 {
		t.Fatalf("copy ignored cancellation: %d %v", n, err)
	}
}

func TestDamagedOrReboundMetadataRejected(t *testing.T) {
	base, run := fixture(t)
	put(t, filepath.Join(run, "fixture.txt"), "fixture")
	e, err := Save(t.Context(), base, 7, run, SaveRequest{Path: "fixture.txt", Category: "evidence"})
	if err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(base, "tasks", "7", Directory, e.ID, "metadata.json")
	e.Description = "forged after publication"
	raw, _ := json.Marshal(e)
	if err := os.WriteFile(metadata, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(base, 7, e.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("metadata rebinding accepted: %v", err)
	}
	if _, err := List(base, 7, Query{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt listing accepted: %v", err)
	}
}

func TestStartupCleanupRemovesOnlyValidatedInterruptedSnapshots(t *testing.T) {
	base, run := fixture(t)
	put(t, filepath.Join(run, "fixture.txt"), "published fixture")
	req := SaveRequest{Path: "fixture.txt", Category: "evidence", Worker: "worker-2", IntentID: 8}
	saved, err := Save(t.Context(), base, 7, run, req)
	if err != nil {
		t.Fatal(err)
	}
	task := filepath.Join(base, "tasks", "7")
	store := filepath.Join(task, Directory)
	pending := filepath.Join(store, ".pending-12345678-1234-4234-8234-123456789abc")
	empty := filepath.Join(store, ".pending-87654321-4321-4321-8321-cba987654321")
	for _, dir := range []string{pending, empty} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	put(t, filepath.Join(pending, "content"), "unfinished bytes")
	put(t, filepath.Join(pending, "metadata.json"), "unfinished metadata")
	// Normal publication/listing cannot delete another in-flight staging copy.
	if _, err := Save(t.Context(), base, 7, run, req); err != nil {
		t.Fatal(err)
	}
	listing, err := List(base, 7, Query{})
	if err != nil || listing.Total != 1 {
		t.Fatalf("listing=%+v err=%v", listing, err)
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatal("live operations removed staging", err)
	}
	if err := ProtectTaskStore(task); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{pending, empty} {
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staging remains: %v", err)
		}
	}
	entry, file, err := Open(base, 7, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || entry != saved || string(data) != "published fixture" {
		t.Fatalf("published snapshot changed: %+v %q %v", entry, data, err)
	}
	if err := ProtectTaskStore(task); err != nil {
		t.Fatalf("repeat cleanup: %v", err)
	}
}

func TestStartupCleanupRefusesUnknownOrUnsafeStagingWithoutDeletingAnything(t *testing.T) {
	for _, kind := range []string{"unknown_name", "unknown_file", "symlink_file", "hardlink_file", "symlink_directory"} {
		t.Run(kind, func(t *testing.T) {
			base, run := fixture(t)
			put(t, filepath.Join(run, "fixture.txt"), "published fixture")
			saved, err := Save(t.Context(), base, 7, run, SaveRequest{Path: "fixture.txt", Category: "evidence"})
			if err != nil {
				t.Fatal(err)
			}
			task := filepath.Join(base, "tasks", "7")
			store := filepath.Join(task, Directory)
			good := filepath.Join(store, ".pending-12345678-1234-4234-8234-123456789abc")
			if err := os.Mkdir(good, 0o700); err != nil {
				t.Fatal(err)
			}
			put(t, filepath.Join(good, "content"), "leave on validation failure")
			bad := filepath.Join(store, ".pending-87654321-4321-4321-8321-cba987654321")
			outside := filepath.Join(t.TempDir(), "outside.txt")
			put(t, outside, "outside fixture")
			if kind == "unknown_name" {
				bad = filepath.Join(store, ".pending-not-a-uuid")
			}
			if kind == "symlink_directory" {
				if err := os.Symlink(filepath.Dir(outside), bad); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(bad, 0o700); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "unknown_file":
					put(t, filepath.Join(bad, "unexpected.txt"), "keep me")
				case "symlink_file":
					if err := os.Symlink(outside, filepath.Join(bad, "content")); err != nil {
						t.Fatal(err)
					}
				case "hardlink_file":
					if err := os.Link(outside, filepath.Join(bad, "content")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := ProtectTaskStore(task); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("unsafe cleanup did not fail closed: %v", err)
			}
			for _, path := range []string{good, bad, outside} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("validation failure removed %q: %v", path, err)
				}
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "outside fixture" {
				t.Fatalf("outside file changed: %q %v", data, err)
			}
			entry, file, err := Open(base, 7, saved.ID)
			if err != nil {
				t.Fatal(err)
			}
			_ = file.Close()
			if entry != saved {
				t.Fatal("published entry changed")
			}
		})
	}
}
