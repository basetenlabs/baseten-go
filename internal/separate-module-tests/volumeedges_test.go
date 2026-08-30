package separatemoduletests_test

// Edge cases at the ends of the range: a volume with nothing in it, and a
// transfer the caller abandons partway.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/transfer"
)

// TestEmptyVolumeRoundTrip covers a tree with no entries at all. It is a real
// state — a volume created before anything is put in it — and the degenerate
// case of nearly every loop in both engines.
func TestEmptyVolumeRoundTrip(t *testing.T) {
	source := t.TempDir()
	fake := newFakeService(t)
	ctx := context.Background()

	pushed, err := transfer.Push(ctx, fake.client(t), pushOptions(source, fake))
	if err != nil {
		t.Fatal(err)
	}
	if pushed.Files != 0 || pushed.Bytes != 0 {
		t.Errorf("pushed %d files and %d bytes from an empty tree", pushed.Files, pushed.Bytes)
	}
	// The manifest is still an object, and still the only one.
	if pushed.Chunks != 1 {
		t.Errorf("stored %d objects, want just the manifest", pushed.Chunks)
	}

	manifest, err := volume.DecodeManifest(fake.manifestBytes(t, pushed.ManifestDigest.String()))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.EntryCount() != 0 || manifest.TotalSize() != 0 {
		t.Errorf("manifest describes %d entries and %d bytes", manifest.EntryCount(), manifest.TotalSize())
	}

	dest := filepath.Join(t.TempDir(), "downloaded")
	downloaded, err := transfer.Pull(ctx, fake.client(t), pullOptions(dest, fake))
	if err != nil {
		t.Fatal(err)
	}
	if downloaded.Files != 0 || downloaded.ChunksFetched != 0 {
		t.Errorf("downloaded %d files and %d chunks", downloaded.Files, downloaded.ChunksFetched)
	}

	// The destination exists and is empty, rather than being absent.
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the published tree holds %d entries", len(entries))
	}
	if stages := stageDirs(t, dest); len(stages) != 0 {
		t.Errorf("stages left behind: %v", stages)
	}
}

// TestPushStopsWhenCancelled covers the caller giving up partway. The push
// must stop rather than run to completion, and must not commit — an abandoned
// push leaves unreferenced objects and no version.
func TestPushStopsWhenCancelled(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)

	ctx, cancel := context.WithCancel(context.Background())
	var uploads atomic.Int64
	fake.onUpload = func() {
		if uploads.Add(1) == 2 {
			cancel()
		}
	}

	_, err := transfer.Push(ctx, fake.client(t), pushOptions(root, fake))
	if err == nil {
		t.Fatal("a cancelled push should not have succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected a cancellation, got %v", err)
	}
	if len(fake.commits) != 0 {
		t.Error("a cancelled push committed a version")
	}
}

// TestPullStopsWhenCancelled covers the same for a download, which must also
// leave the destination unpublished.
func TestPullStopsWhenCancelled(t *testing.T) {
	_, fake := pushFixture(t)
	parent := t.TempDir()
	dest := filepath.Join(parent, "downloaded")

	ctx, cancel := context.WithCancel(context.Background())
	opts := pullOptions(dest, fake)
	store := fake.downloader()
	var fetched atomic.Int64
	opts.DownloadObject = func(ctx context.Context, req volume.ObjectDownload) (*volume.ObjectResult, error) {
		if fetched.Add(1) == 2 {
			cancel()
		}
		return store(ctx, req)
	}

	_, err := transfer.Pull(ctx, fake.client(t), opts)
	if err == nil {
		t.Fatal("a cancelled download should not have succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected a cancellation, got %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Error("a cancelled download published its destination")
	}
	// The partial work is left in the stage, which is what a later attempt
	// resumes from.
	if stages := stageDirs(t, dest); len(stages) != 1 {
		t.Errorf("expected the stage to survive for a retry, found %v", stages)
	}
}

// TestPullDoesNotPublishAfterCancellation pins the half of that contract the
// case above only samples. Cancelling partway usually leaves a download in
// flight to fail on, but a transfer small enough to dispatch in one wave can
// have every object in hand before the cancellation is noticed, and then
// nothing downstream has a context to fail on: the destination gets published
// for a caller who asked to stop. Placing the cancellation after the last
// object reproduces that state every run rather than a couple of times in
// three hundred.
func TestPullDoesNotPublishAfterCancellation(t *testing.T) {
	_, fake := pushFixture(t)

	// How many objects this plan fetches is a property of the fixture, not
	// something to hardcode, so it is measured on an uncancelled run first.
	var objects int64
	countDest := filepath.Join(t.TempDir(), "counted")
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(countDest, "assets"), 0o755) })
	countOpts := pullOptions(countDest, fake)
	countStore := fake.downloader()
	var counted atomic.Int64
	countOpts.DownloadObject = func(ctx context.Context, req volume.ObjectDownload) (*volume.ObjectResult, error) {
		counted.Add(1)
		return countStore(ctx, req)
	}
	if _, err := transfer.Pull(context.Background(), fake.client(t), countOpts); err != nil {
		t.Fatalf("the uncancelled reference run failed: %v", err)
	}
	objects = counted.Load()

	dest := filepath.Join(t.TempDir(), "downloaded")
	ctx, cancel := context.WithCancel(context.Background())
	opts := pullOptions(dest, fake)
	store := fake.downloader()
	var fetched atomic.Int64
	opts.DownloadObject = func(ctx context.Context, req volume.ObjectDownload) (*volume.ObjectResult, error) {
		res, err := store(ctx, req)
		// Cancelling after the object is in hand, on the last one, leaves the
		// download work complete and only the publish ahead.
		if fetched.Add(1) == objects {
			cancel()
		}
		return res, err
	}

	_, err := transfer.Pull(ctx, fake.client(t), opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected a cancellation, got %v", err)
	}
	if got := fetched.Load(); got != objects {
		t.Fatalf("the cancellation landed early: fetched %d of %d objects, which is not the state this test is for", got, objects)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Error("a cancelled download published its destination")
	}
	if stages := stageDirs(t, dest); len(stages) != 1 {
		t.Errorf("expected the stage to survive for a retry, found %v", stages)
	}
}
