//go:build linux

package volume

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func forceLinuxFchmodat2Unavailable(t *testing.T) {
	t.Helper()
	original := linuxFchmodat2NoFollow
	linuxFchmodat2NoFollow = func(*os.File, string, os.FileMode) error {
		return syscall.ENOSYS
	}
	t.Cleanup(func() {
		linuxFchmodat2NoFollow = original
	})
}

func TestLinuxChmodNoFollowFallsBackOnENOSYS(t *testing.T) {
	forceLinuxFchmodat2Unavailable(t)
	rootPath := t.TempDir()
	if err := os.Chmod(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := openRootedDirectory(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	for path, mode := range map[string]os.FileMode{
		"file":      0o400,
		"directory": 0o500,
	} {
		before, err := root.lstat(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		after, err := root.chmodNoFollow(path, mode, nil, before)
		if err != nil {
			t.Fatalf("chmod fallback for %s: %v", path, err)
		}
		if !sameRootedObject(before, after) || after.mode.Perm() != mode {
			t.Fatalf("chmod fallback for %s returned %+v", path, after)
		}
	}
}

func TestLinuxENOSYSFallbackSupportsStaleTreeCleanup(t *testing.T) {
	forceLinuxFchmodat2Unavailable(t)
	parentPath := t.TempDir()
	stalePath := filepath.Join(parentPath, "stale")
	if err := os.Mkdir(stalePath, 0o700); err != nil {
		t.Fatal(err)
	}
	restrictedPath := filepath.Join(stalePath, "restricted")
	if err := os.Mkdir(restrictedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(restrictedPath, "file"),
		nil,
		0o400,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(restrictedPath, 0o500); err != nil {
		t.Fatal(err)
	}
	parent, err := openRootedDirectory(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.close()
	stale, err := parent.lstat("stale", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.removeTree("stale", stale, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stalePath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale tree remains: %v", err)
	}
}
