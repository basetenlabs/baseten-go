//go:build unix

package separatemoduletests_test

// The identity pin's unix arm, in a unix-tagged file so it RUNS wherever the
// pin is live rather than skipping everywhere — a guard that never runs is a
// non-measurement wearing green.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/transfer"
)

// TestPushRefusesAReplacedFile pins the inode check: a multi-chunk file
// swapped for a same-sized one between scan and open passes the size
// re-check, and only the identity recorded by the scan's own Lstat tells the
// two files apart.
func TestPushRefusesAReplacedFile(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)
	ctx := context.Background()
	if _, err := transfer.Push(ctx, fake.client(t), pushOptions(root, fake)); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(root, "nested", "deep", "data.bin")
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := volume.FileIdentity(info); !ok {
		t.Fatal("this platform is unix-tagged yet exposes no identity — the arm is not running where it claims to")
	}

	opts := pushOptions(root, fake)
	opts.DownloadObject = mutateOnPriorFetch(fake, func() {
		// A different file of exactly the scanned size takes the path over:
		// same bytes count, new inode.
		replacement := filepath.Join(root, "replacement.bin")
		if err := os.WriteFile(replacement, patternBytes(int(info.Size())), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, target); err != nil {
			t.Fatal(err)
		}
	})
	_, err = transfer.Push(ctx, fake.client(t), opts)
	if err == nil {
		t.Fatal("the push read a replaced file as if it were the scanned one")
	}
	if !strings.Contains(err.Error(), "was replaced during push") {
		t.Errorf("the failure does not name the replacement: %v", err)
	}
	if !strings.Contains(err.Error(), "nested/deep/data.bin") {
		t.Errorf("the failure does not name the file: %v", err)
	}
}
