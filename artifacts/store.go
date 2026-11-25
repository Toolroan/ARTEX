// Package artifacts publishes local task files as immutable, curated snapshots.
// It performs no network activity and never discovers or collects remote files.
package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	Directory              = "collected"
	MaxFileSize      int64 = 16 << 30
	maxMetadataBytes       = 16 << 10
	maxItems               = 50000
)

var (
	ErrInvalid  = errors.New("invalid artifact")
	ErrNotFound = errors.New("artifact file not found")
	ErrTooLarge = errors.New("artifact exceeds the 16 GiB snapshot limit")
	ErrChanged  = errors.New("source changed while saving; wait for the file to finish writing and retry")
	ErrCorrupt  = errors.New("artifact metadata or snapshot is damaged")
)

type Entry struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Category     string `json:"category"`
	Source       string `json:"source"`
	Description  string `json:"description,omitempty"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	CreatedAt    int64  `json:"created_at"`
	Worker       string `json:"worker"`
	IntentID     int64  `json:"intent_id"`
	OriginalPath string `json:"original_path"`
}

// Worker and IntentID are supplied by the caller's trusted runtime, not its tool
// input. Source and Description are reported labels, not verified remote facts.
type SaveRequest struct {
	Path        string `json:"path"`
	Category    string `json:"category"`
	Source      string `json:"source"`
	Description string `json:"description,omitempty"`
	Worker      string `json:"-"`
	IntentID    int64  `json:"-"`
}

type Query struct {
	Category string
	Search   string
	Limit    int
	Offset   int
}

type Listing struct {
	Items  []Entry `json:"items"`
	Total  int     `json:"total"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
}

func ValidCategory(s string) bool {
	switch s {
	case "database", "source", "config", "document", "evidence", "other":
		return true
	}
	return false
}

func boundedText(s string, n int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > n {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

func cleanSource(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !boundedText(s, 512) {
		return "", fmt.Errorf("%w: source must be a short host or source label", ErrInvalid)
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("%w: invalid source URL", ErrInvalid)
		}
		u.User, u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = nil, "", "", "", false
		s = u.String()
	} else {
		// Query strings and fragments are never useful provenance labels.
		s = strings.SplitN(strings.SplitN(s, "?", 2)[0], "#", 2)[0]
	}
	return s, nil
}

func validRelative(s string) bool {
	if s == "" || !boundedText(s, 4096) || strings.Contains(s, "\\") || filepath.IsAbs(s) {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(s), "/") {
		if seg == "" || seg == "." || seg == ".." || strings.Contains(seg, ":") {
			return false
		}
	}
	return true
}

func validHex(s string) bool {
	if len(s) != 64 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func taskRoot(base string, taskID int64) (string, error) {
	if taskID <= 0 || base == "" {
		return "", fmt.Errorf("%w: invalid task", ErrInvalid)
	}
	// The configured base is trusted. Resolving it once permits ordinary platform
	// aliases (/var on macOS); every task-controlled descendant is opened no-follow.
	root, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", pathError(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", ErrInvalid
	}
	return filepath.Join(root, "tasks", strconv.FormatInt(taskID, 10)), nil
}

func pathError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return fmt.Errorf("%w: file is unavailable or is not a safe regular file", ErrInvalid)
}

func sourceRelative(base, taskPath, runDir, path string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.Contains(path, "\\") || !boundedText(path, 8192) {
		return "", ErrInvalid
	}
	runAbs, err := filepath.Abs(runDir)
	if err != nil {
		return "", ErrInvalid
	}
	// Rebase the trusted configured path when its base uses a platform alias.
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", ErrInvalid
	}
	relRun, err := filepath.Rel(baseAbs, runAbs)
	if err != nil || relRun == ".." || strings.HasPrefix(relRun, ".."+string(filepath.Separator)) {
		return "", ErrInvalid
	}
	realBase := filepath.Dir(filepath.Dir(taskPath))
	runAbs = filepath.Join(realBase, relRun)
	runRel, err := filepath.Rel(taskPath, runAbs)
	if err != nil || (runRel != "." && !validRelative(filepath.ToSlash(runRel))) {
		return "", ErrInvalid
	}
	var candidate string
	if filepath.IsAbs(path) {
		candidate = filepath.Clean(path)
		if rel, e := filepath.Rel(baseAbs, candidate); e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			candidate = filepath.Join(realBase, rel)
		}
	} else {
		if !validRelative(filepath.ToSlash(path)) {
			return "", ErrInvalid
		}
		candidate = filepath.Join(runAbs, path)
	}
	rel, err := filepath.Rel(taskPath, candidate)
	if err != nil || !validRelative(filepath.ToSlash(rel)) {
		return "", fmt.Errorf("%w: source must be inside this task", ErrInvalid)
	}
	rel = filepath.ToSlash(rel)
	if rel == Directory || strings.HasPrefix(rel, Directory+"/") {
		return "", fmt.Errorf("%w: use the existing collected entry", ErrInvalid)
	}
	return rel, nil
}

// Save copies a completed local regular file into a private snapshot. It never
// moves or edits the source. Repeating the same bytes and metadata returns the
// existing entry; changed bytes or provenance produce a distinct entry.
func Save(ctx context.Context, base string, taskID int64, runDir string, req SaveRequest) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	if !ValidCategory(req.Category) || !boundedText(req.Description, 2000) || !boundedText(req.Worker, 128) || req.IntentID < 0 {
		return Entry{}, ErrInvalid
	}
	source, err := cleanSource(req.Source)
	if err != nil {
		return Entry{}, err
	}
	taskPath, err := taskRoot(base, taskID)
	if err != nil {
		return Entry{}, err
	}
	rel, err := sourceRelative(base, taskPath, runDir, req.Path)
	if err != nil {
		return Entry{}, err
	}
	task, err := openTaskDirectory(taskPath)
	if err != nil {
		return Entry{}, pathError(err)
	}
	defer task.Close()
	input, err := openRelativeFile(task, rel)
	if err != nil {
		return Entry{}, pathError(err)
	}
	defer input.Close()
	before, err := input.Stat()
	if err != nil || !safeRegular(before) {
		return Entry{}, fmt.Errorf("%w: source must be a regular file without hard links", ErrInvalid)
	}
	if before.Size() > MaxFileSize {
		return Entry{}, ErrTooLarge
	}
	name := filepath.Base(rel)
	if !boundedText(name, 255) {
		return Entry{}, fmt.Errorf("%w: filename is too long or contains control characters", ErrInvalid)
	}
	store, err := openChildDirectory(task, Directory, true)
	if err != nil {
		return Entry{}, pathError(err)
	}
	defer store.Close()
	if err := protectDirectory(store); err != nil {
		return Entry{}, pathError(err)
	}
	pending := ".pending-" + uuid.NewString()
	staging, err := openChildDirectory(store, pending, true)
	if err != nil {
		return Entry{}, pathError(err)
	}
	defer staging.Close()
	defer removeChildDirectory(store, pending)
	out, err := createChildFile(staging, "content")
	if err != nil {
		return Entry{}, pathError(err)
	}
	hash := sha256.New()
	n, copyErr := copySnapshot(ctx, io.MultiWriter(out, hash), input)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		return Entry{}, copyErr
	}
	if closeErr != nil {
		return Entry{}, pathError(closeErr)
	}
	after, err := input.Stat()
	if err != nil || !safeRegular(after) || n != before.Size() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || metadataChanged(before, after) {
		return Entry{}, ErrChanged
	}
	entry := Entry{Name: name, Category: req.Category, Source: source, Description: strings.TrimSpace(req.Description), Size: n, SHA256: hex.EncodeToString(hash.Sum(nil)), Worker: req.Worker, IntentID: req.IntentID, OriginalPath: rel}
	identity, _ := json.Marshal(entry) // excludes created time and id, stable for retries
	id := sha256.Sum256(identity)
	entry.ID = hex.EncodeToString(id[:])
	entry.CreatedAt = time.Now().UnixMilli()
	raw, _ := json.Marshal(entry)
	if len(raw) > maxMetadataBytes {
		return Entry{}, fmt.Errorf("%w: combined artifact metadata is too long; shorten the source or description", ErrInvalid)
	}
	meta, err := createChildFile(staging, "metadata.json")
	if err != nil {
		return Entry{}, pathError(err)
	}
	_, err = meta.Write(raw)
	if err == nil {
		err = meta.Sync()
	}
	metaClose := meta.Close()
	if err != nil || metaClose != nil {
		return Entry{}, fmt.Errorf("could not save artifact metadata")
	}
	if err = staging.Sync(); err != nil {
		return Entry{}, fmt.Errorf("could not sync artifact snapshot")
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	if err = publishChildDirectory(store, pending, entry.ID); err != nil {
		// The final directory is never replaced. Concurrent identical publications
		// converge on the first complete snapshot, including its timestamp.
		existing, file, openErr := openEntry(store, entry.ID)
		if openErr != nil {
			return Entry{}, fmt.Errorf("could not publish artifact snapshot")
		}
		_ = file.Close()
		return existing, nil
	}
	if err = store.Sync(); err != nil {
		return Entry{}, fmt.Errorf("snapshot saved but directory sync failed; retry the same request")
	}
	return entry, nil
}

func copySnapshot(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 1024*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if total+int64(n) > MaxFileSize {
				return total, ErrTooLarge
			}
			written, err := dst.Write(buf[:n])
			total += int64(written)
			if err != nil {
				return total, fmt.Errorf("could not write artifact snapshot")
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, fmt.Errorf("could not read artifact source")
		}
	}
}

func validateEntry(e Entry, id string) error {
	if e.ID != id || !validHex(id) || !validHex(e.SHA256) || !ValidCategory(e.Category) || e.Size < 0 || e.Size > MaxFileSize || e.CreatedAt <= 0 || e.IntentID < 0 || !validRelative(e.OriginalPath) || e.Name != filepath.Base(e.OriginalPath) || !boundedText(e.Name, 255) || !boundedText(e.Source, 512) || !boundedText(e.Description, 2000) || !boundedText(e.Worker, 128) {
		return ErrCorrupt
	}
	if e.OriginalPath == Directory || strings.HasPrefix(e.OriginalPath, Directory+"/") {
		return ErrCorrupt
	}
	source, err := cleanSource(e.Source)
	if err != nil || source != e.Source {
		return ErrCorrupt
	}
	identity := e
	identity.ID = ""
	identity.CreatedAt = 0
	raw, _ := json.Marshal(identity)
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != id {
		return ErrCorrupt
	}
	return nil
}

func readEntry(dir *os.File, id string) (Entry, error) {
	f, err := openRelativeFile(dir, "metadata.json")
	if err != nil {
		return Entry{}, ErrCorrupt
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !safeRegular(fi) || fi.Size() > maxMetadataBytes {
		return Entry{}, ErrCorrupt
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxMetadataBytes+1))
	if err != nil || len(raw) > maxMetadataBytes {
		return Entry{}, ErrCorrupt
	}
	var e Entry
	if json.Unmarshal(raw, &e) != nil || validateEntry(e, id) != nil {
		return Entry{}, ErrCorrupt
	}
	return e, nil
}

func openEntry(store *os.File, id string) (Entry, *os.File, error) {
	if !validHex(id) {
		return Entry{}, nil, ErrInvalid
	}
	dir, err := openChildDirectory(store, id, false)
	if err != nil {
		return Entry{}, nil, pathError(err)
	}
	defer dir.Close()
	e, err := readEntry(dir, id)
	if err != nil {
		return Entry{}, nil, err
	}
	f, err := openRelativeFile(dir, "content")
	if err != nil {
		return Entry{}, nil, ErrCorrupt
	}
	fi, err := f.Stat()
	if err != nil || !safeRegular(fi) || fi.Size() != e.Size {
		_ = f.Close()
		return Entry{}, nil, ErrCorrupt
	}
	return e, f, nil
}

// Open returns only a server-owned snapshot. The caller must close the file.
func Open(base string, taskID int64, id string) (Entry, *os.File, error) {
	if !validHex(id) {
		return Entry{}, nil, ErrInvalid
	}
	taskPath, err := taskRoot(base, taskID)
	if err != nil {
		return Entry{}, nil, err
	}
	task, err := openTaskDirectory(taskPath)
	if err != nil {
		return Entry{}, nil, pathError(err)
	}
	defer task.Close()
	store, err := openChildDirectory(task, Directory, false)
	if err != nil {
		return Entry{}, nil, pathError(err)
	}
	defer store.Close()
	return openEntry(store, id)
}

func List(base string, taskID int64, q Query) (Listing, error) {
	if q.Category != "" && !ValidCategory(q.Category) || !boundedText(q.Search, 256) || q.Offset < 0 || q.Limit < 0 || q.Limit > 500 {
		return Listing{}, ErrInvalid
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	result := Listing{Items: []Entry{}, Limit: q.Limit, Offset: q.Offset}
	taskPath, err := taskRoot(base, taskID)
	if errors.Is(err, ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	task, err := openTaskDirectory(taskPath)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, pathError(err)
	}
	defer task.Close()
	store, err := openChildDirectory(task, Directory, false)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, pathError(err)
	}
	defer store.Close()
	names, err := store.Readdirnames(maxItems + 1)
	if err != nil && err != io.EOF {
		return result, pathError(err)
	}
	if len(names) > maxItems {
		return result, fmt.Errorf("artifact catalogue exceeds %d entries", maxItems)
	}
	items := []Entry{}
	search := strings.ToLower(strings.TrimSpace(q.Search))
	for _, id := range names {
		if !validHex(id) {
			continue
		} // unpublished staging directories are never visible
		dir, err := openChildDirectory(store, id, false)
		if err != nil {
			return result, ErrCorrupt
		}
		e, err := readEntry(dir, id)
		_ = dir.Close()
		if err != nil {
			return result, err
		}
		if q.Category != "" && e.Category != q.Category {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(strings.Join([]string{e.Name, e.Source, e.Description, e.Worker, e.OriginalPath, e.SHA256}, " ")), search) {
			continue
		}
		items = append(items, e)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt == items[j].CreatedAt {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt > items[j].CreatedAt
	})
	result.Total = len(items)
	if q.Offset >= len(items) {
		return result, nil
	}
	end := q.Offset + q.Limit
	if end > len(items) {
		end = len(items)
	}
	result.Items = items[q.Offset:end]
	return result, nil
}

// ProtectTaskStore is called during privilege setup and archive restore, before
// task operations/workers are admitted. It must not run against a live task:
// validated staging directories here are leftovers from an interrupted process.
// A collected subtree is platform-owned, never legacy executor working data.
func ProtectTaskStore(taskDir string) error {
	// Resolve only the configured data-dir prefix, never the task/store entries.
	taskDir, err := filepath.Abs(taskDir)
	if err != nil {
		return err
	}
	base, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(taskDir)))
	if err != nil {
		return err
	}
	taskDir = filepath.Join(base, filepath.Base(filepath.Dir(taskDir)), filepath.Base(taskDir))
	storePath := filepath.Join(taskDir, Directory)
	if _, err := os.Lstat(storePath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	task, err := openTaskDirectory(taskDir)
	if err != nil {
		return err
	}
	defer task.Close()
	store, err := openChildDirectory(task, Directory, false)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := protectDirectory(store); err != nil {
		return err
	}
	if err := cleanupCrashStaging(store); err != nil {
		return err
	}
	// Use a fresh descriptor so permission repair starts at the beginning of the
	// directory after cleanup consumed its directory stream.
	private, err := openChildDirectory(task, Directory, false)
	if err != nil {
		return err
	}
	defer private.Close()
	return protectTree(private, 0)
}

func validPendingDirectory(name string) bool {
	text, ok := strings.CutPrefix(name, ".pending-")
	if !ok {
		return false
	}
	id, err := uuid.Parse(text)
	return err == nil && id.String() == text && id.Version() == 4 && id.Variant() == uuid.RFC4122
}

func cleanupCrashStaging(store *os.File) error {
	names, err := store.Readdirnames(maxItems + 1)
	if err != nil && err != io.EOF {
		return err
	}
	if len(names) > maxItems {
		return ErrCorrupt
	}
	pending := make([]string, 0)
	// Validate the entire cleanup set before deleting anything. Unknown names,
	// symlinks, hardlinks and unexpected contents require operator review.
	for _, name := range names {
		if !validHex(name) && !validPendingDirectory(name) {
			return fmt.Errorf("%w: unexpected entry in collected storage; staging cleanup was not attempted", ErrCorrupt)
		}
		dir, err := openChildDirectory(store, name, false)
		if err != nil {
			return fmt.Errorf("%w: unsafe entry in collected storage; staging cleanup was not attempted", ErrCorrupt)
		}
		if validPendingDirectory(name) {
			err = validateCrashStaging(dir)
			if err == nil {
				pending = append(pending, name)
			}
		}
		_ = dir.Close()
		if err != nil {
			return err
		}
	}
	for _, name := range pending {
		if err := removeKnownChildDirectory(store, name); err != nil {
			return fmt.Errorf("could not remove interrupted artifact staging: %w", err)
		}
	}
	if len(pending) > 0 {
		return store.Sync()
	}
	return nil
}

func validateCrashStaging(dir *os.File) error {
	names, err := dir.Readdirnames(3)
	if err != nil && err != io.EOF {
		return err
	}
	if len(names) > 2 {
		return fmt.Errorf("%w: unexpected staging contents; staging cleanup was not attempted", ErrCorrupt)
	}
	for _, name := range names {
		if name != "content" && name != "metadata.json" {
			return fmt.Errorf("%w: unexpected staging contents; staging cleanup was not attempted", ErrCorrupt)
		}
		file, err := openRelativeFile(dir, name)
		if err != nil {
			return fmt.Errorf("%w: unsafe staging contents; staging cleanup was not attempted", ErrCorrupt)
		}
		info, statErr := file.Stat()
		_ = file.Close()
		if statErr != nil || !safeRegular(info) {
			return fmt.Errorf("%w: unsafe staging contents; staging cleanup was not attempted", ErrCorrupt)
		}
	}
	return nil
}

func protectTree(dir *os.File, depth int) error {
	if depth > 2 {
		return ErrCorrupt
	}
	if err := protectDirectory(dir); err != nil {
		return err
	}
	names, err := dir.Readdirnames(maxItems + 1)
	if err != nil && err != io.EOF {
		return err
	}
	if len(names) > maxItems {
		return ErrCorrupt
	}
	for _, name := range names {
		child, dirErr := openChildDirectory(dir, name, false)
		if dirErr == nil {
			err = protectTree(child, depth+1)
			_ = child.Close()
			if err != nil {
				return err
			}
			continue
		}
		file, fileErr := openRelativeFile(dir, name)
		if fileErr != nil {
			return fileErr
		}
		fi, statErr := file.Stat()
		if statErr != nil || !safeRegular(fi) {
			_ = file.Close()
			return ErrCorrupt
		}
		err = protectFile(file)
		_ = file.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
