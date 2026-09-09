package volume_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/volume/transfer"
)

// stripOptions narrows to include and lays the result out relative to prefix.
func stripOptions(dest string, f *fakeService, prefix string, include ...string) transfer.PullOptions {
	opts := pullOptions(dest, f)
	opts.Include = include
	opts.StripPrefix = prefix
	return opts
}

// TestPullStripPrefixLandsAtTheDestinationRoot is the whole point: a small
// directory five levels down should not arrive five levels down.
func TestPullStripPrefixLandsAtTheDestinationRoot(t *testing.T) {
	_, fake := pushFixture(t)
	dest := filepath.Join(t.TempDir(), "out")

	if _, err := transfer.Pull(context.Background(), fake.client(t),
		stripOptions(dest, fake, "nested/deep", "nested/deep")); err != nil {
		t.Fatal(err)
	}

	// The file is at the root, and none of the directories that led to it
	// were created.
	if _, err := os.Stat(filepath.Join(dest, "data.bin")); err != nil {
		t.Errorf("data.bin should be at the destination root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "nested")); !os.IsNotExist(err) {
		t.Errorf("nested should not exist under the destination, got %v", err)
	}
	// The stripped subtree is the whole of what was written: one entry, and
	// none of the directories above it.
	got := treeDescription(t, dest)
	if !strings.Contains(got, "file data.bin ") || strings.Contains(got, "\n") {
		t.Errorf("destination should hold data.bin alone, got:\n%s", got)
	}
}

// TestPullStripPrefixKeepsRecordedModes covers what the prefix does and does
// not carry: entries under it keep their recorded modes, while the prefix's
// own directory is the destination, whose mode belongs to whoever created it
// rather than to the volume.
func TestPullStripPrefixKeepsRecordedModes(t *testing.T) {
	_, fake := pushFixture(t)
	dest := filepath.Join(t.TempDir(), "out")

	if _, err := transfer.Pull(context.Background(), fake.client(t),
		stripOptions(dest, fake, "assets", "assets")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dest, 0o755) })

	info, err := os.Lstat(filepath.Join(dest, "read-only.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o444 {
		t.Errorf("read-only.txt mode %o, want 444", got)
	}

	// "assets" is recorded 0555 in the volume. The destination did not take
	// that mode, because the destination is not that directory.
	destInfo, err := os.Lstat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := destInfo.Mode().Perm(); got == 0o555 {
		t.Errorf("the destination took the prefix directory's recorded mode %o", got)
	}
}

// TestPullStripPrefixRefusesAnEntryOutside pins the check that keeps a write
// inside the destination. An entry outside the prefix has no place under it,
// and placing it anyway would mean climbing out of the destination.
func TestPullStripPrefixRefusesAnEntryOutside(t *testing.T) {
	_, fake := pushFixture(t)
	dest := filepath.Join(t.TempDir(), "out")

	_, err := transfer.Pull(context.Background(), fake.client(t),
		stripOptions(dest, fake, "nested/deep", "nested/deep", "small.txt"))
	if err == nil {
		t.Fatal("an entry outside the prefix should be refused")
	}
	if !strings.Contains(err.Error(), `"small.txt" is not under "nested/deep"`) {
		t.Errorf("got %v, want the entry named against the prefix", err)
	}
	// Refused before anything was created.
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("the destination should not exist, got %v", err)
	}
}

// TestPullStripPrefixRefusesALinkPointingOutside is the case the manifest's
// own containment check cannot catch: "nested/link" targets "../small.txt",
// which is inside the volume but outside "nested". Stripping the prefix makes
// the destination that subtree, so the link would reach out of it.
func TestPullStripPrefixRefusesALinkPointingOutside(t *testing.T) {
	_, fake := pushFixture(t)
	dest := filepath.Join(t.TempDir(), "out")

	_, err := transfer.Pull(context.Background(), fake.client(t),
		stripOptions(dest, fake, "nested", "nested"))
	if err == nil {
		t.Fatal("a symlink reaching outside the prefix should be refused")
	}
	if !strings.Contains(err.Error(), "outside") || !strings.Contains(err.Error(), "nested/link") {
		t.Errorf("got %v, want nested/link named as pointing outside the prefix", err)
	}
}

// TestPullStripPrefixKeepsALinkPointingInside is the other half: a link whose
// target stays within the prefix is written, with its target re-rendered for
// the shallower depth it now sits at.
func TestPullStripPrefixKeepsALinkPointingInside(t *testing.T) {
	root := buildTree(t)
	// A link beside its target, both under the prefix.
	writeFile(t, root, "nested/deep/target.txt", []byte("inside"), 0o644)
	if err := os.Symlink("target.txt", filepath.Join(root, "nested", "deep", "beside")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	fake := newFakeService(t)
	if _, err := transfer.Push(context.Background(), fake.client(t), pushOptions(root, fake)); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "out")
	if _, err := transfer.Pull(context.Background(), fake.client(t),
		stripOptions(dest, fake, "nested/deep", "nested/deep")); err != nil {
		t.Fatal(err)
	}

	target, err := os.Readlink(filepath.Join(dest, "beside"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "target.txt" {
		t.Errorf("link target %q, want %q", target, "target.txt")
	}
	body, err := os.ReadFile(filepath.Join(dest, "beside"))
	if err != nil {
		t.Fatalf("the link should resolve inside the destination: %v", err)
	}
	if string(body) != "inside" {
		t.Errorf("read %q through the link, want %q", body, "inside")
	}
}

// TestPullStripPrefixKeepsALinkToTheStrippedDirectory is the boundary of the
// case above: the link resolves to the prefix itself, which is the destination
// root once stripped. Trimming the prefix off cannot express that, since
// nothing follows it, so getting this wrong points the link at a directory of
// the prefix's own name that the destination does not have.
func TestPullStripPrefixKeepsALinkToTheStrippedDirectory(t *testing.T) {
	root := buildTree(t)
	writeFile(t, root, "nested/deep/target.txt", []byte("inside"), 0o644)
	// "nested/deep/here" targets ".", which resolves to "nested/deep".
	if err := os.Symlink(".", filepath.Join(root, "nested", "deep", "here")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	fake := newFakeService(t)
	if _, err := transfer.Push(context.Background(), fake.client(t), pushOptions(root, fake)); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "out")
	if _, err := transfer.Pull(context.Background(), fake.client(t),
		stripOptions(dest, fake, "nested/deep", "nested/deep")); err != nil {
		t.Fatal(err)
	}

	target, err := os.Readlink(filepath.Join(dest, "here"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "." {
		t.Errorf("link target %q, want %q", target, ".")
	}
	// Read through the link, which is what proves it landed somewhere real.
	body, err := os.ReadFile(filepath.Join(dest, "here", "target.txt"))
	if err != nil {
		t.Fatalf("the link should resolve to the destination root: %v", err)
	}
	if string(body) != "inside" {
		t.Errorf("read %q through the link, want %q", body, "inside")
	}
}

// TestPullStripPrefixPrunesByDestinationName covers the one place the two path
// spaces could disagree: prune walks the destination, so what it compares
// against has to be destination names rather than volume paths, or it would
// delete everything it just wrote.
func TestPullStripPrefixPrunesByDestinationName(t *testing.T) {
	_, fake := pushFixture(t)
	dest := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(dest, "stray.txt")
	if err := os.WriteFile(stray, []byte("gone"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := stripOptions(dest, fake, "nested/deep", "nested/deep")
	opts.Overwrite = true
	if _, err := transfer.Pull(context.Background(), fake.client(t), opts); err != nil {
		t.Fatal(err)
	}

	// Overwrite leaves what the volume does not describe alone, so the stray
	// file survives and the written file is there.
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("overwrite should leave an undescribed file alone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "data.bin")); err != nil {
		t.Errorf("data.bin should have been written: %v", err)
	}
}
