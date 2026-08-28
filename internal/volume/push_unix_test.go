//go:build linux || darwin

package volume

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestUnixPushPreservesSymlinksWithoutFollowingThem(t *testing.T) {
	if platformSourceSymlinkPolicy != sourceSymlinksPreserved {
		t.Fatalf("source symlink policy = %d, want preserve", platformSourceSymlinkPolicy)
	}
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDirectory, "content"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}

	inputs, err := collectPushInputs(t.Context(), root, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer inputs.close()
	if len(inputs.symlinks) != 1 ||
		inputs.symlinks[0].Path != "alias" ||
		inputs.symlinks[0].Target != "real" {
		t.Fatalf("source symlinks = %+v, want alias -> real", inputs.symlinks)
	}
	if len(inputs.files) != 1 || inputs.files[0].relativePath != "real/content" {
		t.Fatalf("source files = %+v, want only real/content", inputs.files)
	}
}

func TestPushTraversalRejectsSpecialFilesAndHonorsCancellation(t *testing.T) {
	t.Run("named pipe", func(t *testing.T) {
		root := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := collectPushInputs(t.Context(), root, 10)
		if err == nil || !IsCode(err, ErrorUnsupported) {
			t.Fatalf("named-pipe error = %v, want %s", err, ErrorUnsupported)
		}
	})

	t.Run("canceled traversal", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := collectPushInputs(ctx, t.TempDir(), 10)
		if err == nil || !IsCode(err, ErrorCanceled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("traversal cancellation = %v", err)
		}
	})
}

func TestPushTraversalRejectsDirectoryToSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "content"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "content"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := newTestVolumeClient(t)
	var once sync.Once
	client.filesystemHooks = &filesystemTestHooks{
		afterPushLstat: func(path string) {
			if path != "directory" {
				return
			}
			once.Do(func() {
				if err := os.Rename(directory, directory+".moved"); err != nil {
					t.Error(err)
					return
				}
				if err := os.Symlink(outside, directory); err != nil {
					t.Error(err)
				}
			})
		},
	}
	session := newMemoryUploadSession()
	_, err := client.Push(t.Context(), PushOptions{
		Path: root, Session: session, Uploader: session,
	})
	if err == nil || !IsCode(err, ErrorPreconditionFailed) {
		t.Fatalf("directory swap error = %v, want %s", err, ErrorPreconditionFailed)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.uploadAttempts != 0 || session.publishAttempts != 0 {
		t.Fatalf(
			"directory swap reached transfer boundaries: upload %d publish %d",
			session.uploadAttempts,
			session.publishAttempts,
		)
	}
}

func TestPushRegularFileToFIFOTransitionCannotBlock(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := newTestVolumeClient(t)
	var once sync.Once
	client.filesystemHooks = &filesystemTestHooks{
		beforePushRead: func(relative string) {
			if relative != "source" {
				return
			}
			once.Do(func() {
				if err := os.Remove(path); err != nil {
					t.Error(err)
					return
				}
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Error(err)
				}
			})
		},
	}
	session := newMemoryUploadSession()
	done := make(chan error, 1)
	go func() {
		_, err := client.Push(t.Context(), PushOptions{
			Path: root, Session: session, Uploader: session,
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !IsCode(err, ErrorFilesystem) {
			t.Fatalf("FIFO transition error = %v, want %s", err, ErrorFilesystem)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("push blocked while opening a source replaced by a FIFO")
	}
}
