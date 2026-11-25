//go:build unix

package artifacts

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"golang.org/x/sys/unix"
)

// Every untrusted path component is opened relative to a held directory
// descriptor with O_NOFOLLOW. A worker cannot race a directory into a symlink
// and make the privileged service copy some other task or server secret.
func openTaskDirectory(path string) (*os.File, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/")
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range parts {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openChildDirectory(parent *os.File, name string, create bool) (*os.File, error) {
	if !validRelative(name) || strings.Contains(name, "/") {
		return nil, ErrInvalid
	}
	if create {
		if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, err
		}
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func openRelativeFile(parent *os.File, path string) (*os.File, error) {
	if !validRelative(path) {
		return nil, ErrInvalid
	}
	parts := strings.Split(path, "/")
	current := parent
	var held *os.File
	defer func() {
		if held != nil {
			_ = held.Close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		next, err := openChildDirectory(current, part, false)
		if err != nil {
			return nil, err
		}
		if held != nil {
			_ = held.Close()
		}
		held = next
		current = next
	}
	fd, err := unix.Openat(int(current.Fd()), parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func createChildFile(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func publishChildDirectory(parent *os.File, from, to string) error {
	// A published entry always contains two files, so rename cannot replace it.
	return unix.Renameat(int(parent.Fd()), from, int(parent.Fd()), to)
}

func removeChildDirectory(parent *os.File, name string) {
	_ = removeKnownChildDirectory(parent, name)
}

func removeKnownChildDirectory(parent *os.File, name string) error {
	dir, err := openChildDirectory(parent, name, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	for _, file := range []string{"metadata.json", "content"} {
		if err := unix.Unlinkat(int(dir.Fd()), file, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	return unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
}

func safeRegular(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}
	// os.FileInfo uses syscall.Stat_t, whose link field has the same exported
	// name across Unix platforms (x/sys has a distinct Go type).
	v := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if !v.IsValid() {
		return false
	}
	n := v.FieldByName("Nlink")
	if !n.IsValid() {
		return false
	}
	switch n.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return n.Uint() == 1
	case reflect.Int, reflect.Int64:
		return n.Int() == 1
	}
	return false
}

func metadataChanged(a, b os.FileInfo) bool {
	av, bv := reflect.Indirect(reflect.ValueOf(a.Sys())), reflect.Indirect(reflect.ValueOf(b.Sys()))
	for _, name := range []string{"Ctim", "Ctimespec"} {
		x, y := av.FieldByName(name), bv.FieldByName(name)
		if x.IsValid() && y.IsValid() {
			return !reflect.DeepEqual(x.Interface(), y.Interface())
		}
	}
	return false
}

func protectDirectory(dir *os.File) error {
	if os.Geteuid() == 0 {
		if err := dir.Chown(0, 0); err != nil {
			return err
		}
	}
	return dir.Chmod(0o700)
}

func protectFile(file *os.File) error {
	if os.Geteuid() == 0 {
		if err := file.Chown(0, 0); err != nil {
			return err
		}
	}
	return file.Chmod(0o600)
}
