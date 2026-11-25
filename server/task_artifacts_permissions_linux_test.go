//go:build linux

package server

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Autumn-27/artex/artifacts"
)

func TestCollectedArtifactsStayPrivateAcrossStartupAndRestoreAsRoot(t *testing.T) {
	f := newExecRootFixture(t)
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is needed for the executor fixture: %v", err)
	}
	source := filepath.Join(f.work, "fixture.conf")
	if err := os.WriteFile(source, []byte("synthetic=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := artifacts.Save(t.Context(), f.data, 1, f.work, artifacts.SaveRequest{
		Path: "fixture.conf", Category: "config", Source: "fixture.invalid", Worker: "planner",
	})
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(f.data, "tasks", "1")
	collected := filepath.Join(taskDir, "collected")
	for _, mode := range []string{"startup", "restore"} {
		t.Run(mode, func(t *testing.T) {
			// Reproduce permissions written by the previous generic task sharing
			// pass (or by extracted archive entries) before applying the new pass.
			if err := shareWithExecGroup(collected, int(f.priv.GID)); err != nil {
				t.Fatal(err)
			}
			if mode == "startup" {
				err = shareTaskDir(taskDir, int(f.priv.GID))
			} else {
				err = shareRestoredTaskDir(f.priv, taskDir)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := filepath.WalkDir(collected, func(path string, item os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				info, err := item.Info()
				if err != nil {
					return err
				}
				wantMode := os.FileMode(0o600)
				if item.IsDir() {
					wantMode = 0o700
				}
				st := info.Sys().(*syscall.Stat_t)
				actualMode := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
				if st.Uid != 0 || st.Gid != 0 || actualMode != wantMode {
					t.Errorf("%s permissions=%v uid=%d gid=%d, want %v root:root", path, actualMode, st.Uid, st.Gid, wantMode)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			paths, _ := json.Marshal(map[string]string{
				"collection": collected,
				"content":    filepath.Join(collected, entry.ID, "content"),
				"metadata":   filepath.Join(collected, entry.ID, "metadata.json"),
			})
			code := "paths = " + string(paths) + `
import errno, os
def attempt(name, fn):
    try:
        fn()
        print(name + "=allowed")
    except OSError as err:
        print(name + "=" + errno.errorcode.get(err.errno, str(err.errno)))
attempt("workspace_write", lambda: open("still-writable.txt", "w").close())
attempt("collection_list", lambda: os.listdir(paths["collection"]))
attempt("collection_create", lambda: open(os.path.join(paths["collection"], "planted"), "w").close())
attempt("snapshot_read", lambda: open(paths["content"], "rb").close())
attempt("snapshot_write", lambda: open(paths["content"], "wb").close())
attempt("metadata_write", lambda: open(paths["metadata"], "wb").close())
attempt("collection_rename", lambda: os.rename(paths["collection"], paths["collection"] + ".moved"))
`
			out, err := execPython(t.Context(), py, "collected-permission-fixture", code, nil, f.work, nil, 30*time.Second, f.priv)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "workspace_write=allowed\n") || strings.Contains(out, "[exit:") {
				t.Fatalf("executor workspace fixture failed: %s", out)
			}
			for _, name := range []string{"collection_list", "collection_create", "snapshot_read", "snapshot_write", "metadata_write", "collection_rename"} {
				if !strings.Contains(out, name+"=EACCES\n") && !strings.Contains(out, name+"=EPERM\n") {
					t.Errorf("%s was not denied: %s", name, out)
				}
			}
			_, file, err := artifacts.Open(f.data, 1, entry.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, readErr := io.ReadAll(file)
			closeErr := file.Close()
			if readErr != nil || closeErr != nil || string(got) != "synthetic=true\n" {
				t.Fatalf("snapshot changed after executor attempts: %q read=%v close=%v", got, readErr, closeErr)
			}
		})
	}
}
