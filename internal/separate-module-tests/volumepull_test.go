package separatemoduletests_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
	"github.com/basetenlabs/baseten-go/internal/volume/transfer"
	"github.com/zeebo/blake3"
)

func pullOptions(dest string, f *fakeService) transfer.PullOptions {
	return transfer.PullOptions{
		Ref:            fakeNamespace + "/" + fakeVolume,
		DestDir:        dest,
		NewHasher:      newBlake3,
		Decompress:     newZstdReader,
		DownloadObject: f.downloader(),
	}
}

// pushFixture publishes the shared tree and returns the service holding it.
func pushFixture(t *testing.T) (string, *fakeService) {
	t.Helper()
	root := buildTree(t)
	fake := newFakeService(t)
	if _, err := transfer.Push(context.Background(), fake.client(t), pushOptions(root, fake)); err != nil {
		t.Fatal(err)
	}
	return root, fake
}

// treeDescription walks a directory into a stable, comparable description of
// every entry: its path, what it is, its mode, and its contents.
func treeDescription(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()

		switch {
		case d.IsDir():
			lines = append(lines, "dir "+rel+" "+modeString(mode))
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(abs)
			if err != nil {
				return err
			}
			lines = append(lines, "link "+rel+" -> "+filepath.ToSlash(target))
		default:
			content, err := os.ReadFile(abs)
			if err != nil {
				return err
			}
			digest := volume.Digest(blake3.Sum256(content))
			lines = append(lines, "file "+rel+" "+modeString(mode)+" "+digest.Hex()[:16])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func modeString(mode fs.FileMode) string {
	return fmt.Sprintf("%04o", mode.Perm())
}

func TestPullReproducesTheTree(t *testing.T) {
	root, fake := pushFixture(t)
	dest := filepath.Join(t.TempDir(), "downloaded")

	result, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "assets"), 0o755) })

	if got, want := treeDescription(t, dest), treeDescription(t, root); got != want {
		t.Errorf("the downloaded tree differs\n got:\n%s\nwant:\n%s", got, want)
	}
	if result.Files != 5 || result.SelectedFiles != 5 || result.TotalFiles != 5 {
		t.Errorf("files %d, selected %d, total %d", result.Files, result.SelectedFiles, result.TotalFiles)
	}
	if !strings.HasSuffix(result.VersionRef, "@"+result.ManifestDigest.String()) {
		t.Errorf("version ref %q is not pinned to the digest", result.VersionRef)
	}
	// Nothing was on disk beforehand, and the empty file's zero-length chunk
	// counts as neither fetched nor reused.
	if result.ChunksReused != 0 {
		t.Errorf("reused %d chunks on a virgin download", result.ChunksReused)
	}
	if result.ChunksFetched != 6 {
		t.Errorf("fetched %d chunks, want 6", result.ChunksFetched)
	}

	// Nothing is left beside the destination once the tree is in place.
	if stages := stageDirs(t, dest); len(stages) != 0 {
		t.Errorf("stages left behind: %v", stages)
	}
}

// TestPullReadsCompressedChunks covers the storage decision a reader cannot
// see from the key: the same chunk may be stored raw or compressed, and only
// the media type says which.
func TestPullReadsCompressedChunks(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)
	fake.compressChunks = true

	if _, err := transfer.Push(context.Background(), fake.client(t), pushOptions(root, fake)); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "downloaded")
	if _, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "assets"), 0o755) })

	if got, want := treeDescription(t, dest), treeDescription(t, root); got != want {
		t.Errorf("the downloaded tree differs\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestPullResumesFromAnInterruptedAttempt is the point of the staged tree: a
// download that failed partway leaves work that the next attempt keeps.
func TestPullResumesFromAnInterruptedAttempt(t *testing.T) {
	_, fake := pushFixture(t)
	dest := filepath.Join(t.TempDir(), "downloaded")

	// A first attempt that fails after some chunks have landed. The
	// downloader refuses a chunk partway through, which aborts the download
	// with the stage left on disk.
	failing := pullOptions(dest, fake)
	store := fake.downloader()
	seen := 0
	failing.Concurrency = volume.Concurrency{FileJobs: 1, ChunkOperations: 1}
	failing.DownloadObject = func(ctx context.Context, req volume.ObjectDownload) (*volume.ObjectResult, error) {
		seen++
		if seen > 4 {
			return nil, errors.New("connection reset")
		}
		return store(ctx, req)
	}
	if _, err := transfer.Pull(context.Background(), fake.client(t), failing); err == nil {
		t.Fatal("the interrupted download should have failed")
	}

	stages := stageDirs(t, dest)
	if len(stages) != 1 {
		t.Fatalf("expected one stage to resume from, found %v", stages)
	}

	result, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "assets"), 0o755) })

	if result.ChunksReused < 1 {
		t.Errorf("the second attempt reused %d chunks, expected the ones already written", result.ChunksReused)
	}
	if result.ChunksFetched+result.ChunksReused != 6 {
		t.Errorf("accounted for %d chunks, want 6", result.ChunksFetched+result.ChunksReused)
	}
}

// TestPullRefetchesCorruptedRanges covers a staged file that is the right
// length and the wrong bytes. Trusting the length alone would publish it.
func TestPullRefetchesCorruptedRanges(t *testing.T) {
	root, fake := pushFixture(t)
	parent := t.TempDir()
	dest := filepath.Join(parent, "downloaded")

	first, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake))
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(dest, "assets"), 0o755)

	// Move the finished tree back into a stage and damage one chunk of it, as
	// a half-written download would leave it.
	stage := dest + ".tmp-b3-" + first.ManifestDigest.Hex()[:12]
	if err := os.Rename(dest, stage); err != nil {
		t.Fatal(err)
	}
	damaged := filepath.Join(stage, "nested", "deep", "data.bin")
	handle, err := os.OpenFile(damaged, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.WriteAt([]byte("corrupt"), volume.ChunkSize+64); err != nil {
		t.Fatal(err)
	}
	handle.Close()

	second, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "assets"), 0o755) })

	if second.ChunksFetched != 1 {
		t.Errorf("fetched %d chunks, want only the damaged one", second.ChunksFetched)
	}
	if got, want := hashPrefix(t, filepath.Join(dest, "nested", "deep", "data.bin")),
		hashPrefix(t, filepath.Join(root, "nested", "deep", "data.bin")); got != want {
		t.Error("the repaired file does not match the source")
	}
}

func TestPullSubset(t *testing.T) {
	_, fake := pushFixture(t)
	dest := filepath.Join(t.TempDir(), "subset")

	opts := pullOptions(dest, fake)
	opts.Include = []string{"nested/deep", "small.txt"}
	result, err := transfer.Pull(context.Background(), fake.client(t), opts)
	if err != nil {
		t.Fatal(err)
	}

	want := strings.Join([]string{
		"dir nested 0755",
		"dir nested/deep 0755",
		"file nested/deep/data.bin 0644 " + hashPrefix(t, filepath.Join(dest, "nested/deep/data.bin")),
		"file small.txt 0600 " + hashPrefix(t, filepath.Join(dest, "small.txt")),
	}, "\n")
	if got := treeDescription(t, dest); got != want {
		t.Errorf("subset tree is\n%s\nwant\n%s", got, want)
	}
	if result.SelectedFiles != 2 || result.TotalFiles != 5 {
		t.Errorf("selected %d of %d files", result.SelectedFiles, result.TotalFiles)
	}
}

// TestPullSubsetRejectsAMissWithNothingWritten covers a selector naming
// something the volume does not have. Reporting success with an empty
// directory would look exactly like a volume that really was empty.
func TestPullSubsetRejectsAMiss(t *testing.T) {
	_, fake := pushFixture(t)
	dest := filepath.Join(t.TempDir(), "subset")

	opts := pullOptions(dest, fake)
	opts.Include = []string{"nested/deep", "does/not/exist"}
	_, err := transfer.Pull(context.Background(), fake.client(t), opts)
	if err == nil {
		t.Fatal("a selector matching nothing should fail the download")
	}
	if !strings.Contains(err.Error(), "does/not/exist") {
		t.Errorf("the error does not name the selector: %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Error("nothing should have been written")
	}
}

// TestPullSubsetDoesNotLeakAWiderStage covers a stage left by a download of
// the whole volume, followed by a download of one directory. Publishing the
// stage as it stood would deliver files that were never asked for.
func TestPullSubsetDoesNotLeakAWiderStage(t *testing.T) {
	_, fake := pushFixture(t)
	parent := t.TempDir()
	dest := filepath.Join(parent, "downloaded")

	full, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake))
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(dest, "assets"), 0o755)
	stage := dest + ".tmp-b3-" + full.ManifestDigest.Hex()[:12]
	if err := os.Rename(dest, stage); err != nil {
		t.Fatal(err)
	}

	opts := pullOptions(dest, fake)
	opts.Include = []string{"small.txt"}
	if _, err := transfer.Pull(context.Background(), fake.client(t), opts); err != nil {
		t.Fatal(err)
	}

	if got := treeDescription(t, dest); got != "file small.txt 0600 "+hashPrefix(t, filepath.Join(dest, "small.txt")) {
		t.Errorf("the narrowed download published\n%s", got)
	}
}

// TestPullRestartDiscardsTheStage covers the opt-out from resuming.
func TestPullRestartDiscardsTheStage(t *testing.T) {
	_, fake := pushFixture(t)
	parent := t.TempDir()
	dest := filepath.Join(parent, "downloaded")

	first, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake))
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(dest, "assets"), 0o755)
	stage := dest + ".tmp-b3-" + first.ManifestDigest.Hex()[:12]
	if err := os.Rename(dest, stage); err != nil {
		t.Fatal(err)
	}

	opts := pullOptions(dest, fake)
	opts.Restart = true
	second, err := transfer.Pull(context.Background(), fake.client(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "assets"), 0o755) })

	if second.ChunksReused != 0 {
		t.Errorf("restarting reused %d chunks; the discarded stage left nothing to reuse", second.ChunksReused)
	}
}

// TestPullRefusesANonEmptyDestination covers the guard on publishing over
// something already there.
func TestPullRefusesANonEmptyDestination(t *testing.T) {
	_, fake := pushFixture(t)
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "mine.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake))
	if err == nil {
		t.Fatal("expected a refusal to write into a non-empty directory")
	}
	if !strings.Contains(err.Error(), "Overwrite") {
		t.Errorf("the error does not say what to do about it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "mine.txt")); err != nil {
		t.Error("the existing file should have been left alone")
	}
}

// TestPullOverwriteWritesInPlace covers the mode that writes into an existing
// directory, leaving files the volume does not describe where they are.
func TestPullOverwriteWritesInPlace(t *testing.T) {
	root, fake := pushFixture(t)
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "mine.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := pullOptions(dest, fake)
	opts.Overwrite = true
	if _, err := transfer.Pull(context.Background(), fake.client(t), opts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "assets"), 0o755) })

	if stages := stageDirs(t, dest); len(stages) != 0 {
		t.Errorf("writing in place should stage nothing, found %v", stages)
	}
	kept, err := os.ReadFile(filepath.Join(dest, "mine.txt"))
	if err != nil || string(kept) != "keep me" {
		t.Errorf("the unrelated file was not left alone: %q, %v", kept, err)
	}

	// Everything the volume describes is present and correct beside it.
	published := treeDescription(t, root)
	for _, line := range strings.Split(published, "\n") {
		if !strings.Contains(treeDescription(t, dest), line) {
			t.Errorf("missing from the destination: %s", line)
		}
	}
}

// TestPullOverwriteReplacesASymlinkWithAFile covers a destination where an
// earlier version of the volume put a symlink at a path this version calls a
// regular file.
//
// The containment root permits a symlink that stays inside it, so opening the
// path for writing would follow the link: the bytes and the mode would land on
// whatever it points at, the link would survive, and the download would report
// success having published a tree that does not match the manifest — while
// silently rewriting a file the volume never described.
func TestPullOverwriteReplacesASymlinkWithAFile(t *testing.T) {
	root, fake := pushFixture(t)
	dest := t.TempDir()

	// The volume describes small.txt as a regular file. Here it is a link to
	// an unrelated file that the download has no business touching.
	bystander := filepath.Join(dest, "bystander.txt")
	if err := os.WriteFile(bystander, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bystander.txt", filepath.Join(dest, "small.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	opts := pullOptions(dest, fake)
	opts.Overwrite = true
	if _, err := transfer.Pull(context.Background(), fake.client(t), opts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "assets"), 0o755) })

	info, err := os.Lstat(filepath.Join(dest, "small.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		t.Error("small.txt is still a symlink; the download wrote through it")
	}
	if got := hashPrefix(t, filepath.Join(dest, "small.txt")); got != hashPrefix(t, filepath.Join(root, "small.txt")) {
		t.Error("small.txt does not match the published version")
	}
	if content, err := os.ReadFile(bystander); err != nil || string(content) != "do not touch" {
		t.Errorf("the unrelated file was rewritten through the symlink: %q, %v", content, err)
	}
}

// TestPullPinnedRef covers downloading a version by digest rather than by
// whatever head currently points at.
func TestPullPinnedRef(t *testing.T) {
	root, fake := pushFixture(t)

	// Publish a second version, moving head away from the first.
	writeFile(t, root, "small.txt", []byte("changed content"), 0o600)
	second, err := transfer.Push(context.Background(), fake.client(t), pushOptions(root, fake))
	if err != nil {
		t.Fatal(err)
	}
	first := fake.commits[0].manifestDigest
	if first == second.ManifestDigest.String() {
		t.Fatal("the two versions have the same digest")
	}

	dest := filepath.Join(t.TempDir(), "pinned")
	opts := pullOptions(dest, fake)
	opts.Ref = fakeNamespace + "/" + fakeVolume + "@" + first
	result, err := transfer.Pull(context.Background(), fake.client(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "assets"), 0o755) })

	if result.ManifestDigest.String() != first {
		t.Errorf("downloaded %s, want the pinned %s", result.ManifestDigest, first)
	}
	content, err := os.ReadFile(filepath.Join(dest, "small.txt"))
	if err != nil || string(content) != "hello volume" {
		t.Errorf("small.txt is %q, want the first version's content", content)
	}
}

// TestPullRejectsCorruptedObjects covers the store returning bytes that are
// not what the manifest says they are.
func TestPullRejectsCorruptedObjects(t *testing.T) {
	_, fake := pushFixture(t)
	dest := filepath.Join(t.TempDir(), "downloaded")

	opts := pullOptions(dest, fake)
	store := fake.downloader()
	opts.DownloadObject = func(ctx context.Context, req volume.ObjectDownload) (*volume.ObjectResult, error) {
		result, err := store(ctx, req)
		if err != nil || req.ExpectedSize == 0 {
			// Leave metadata alone. Corrupting a chunk is what the digest
			// check exists to catch, and it is the only thing that would
			// otherwise reach the file.
			return result, err
		}
		result.Body.Close()
		result.Body = io.NopCloser(bytes.NewReader(make([]byte, req.ExpectedSize)))
		result.ContentType = bdn.ContentTypeChunk
		return result, nil
	}
	_, err := transfer.Pull(context.Background(), fake.client(t), opts)
	if err == nil {
		t.Fatal("a corrupted chunk should fail the download")
	}
	if !strings.Contains(err.Error(), "hashes to") && !strings.Contains(err.Error(), "bytes, the manifest says") {
		t.Errorf("the error does not name the mismatch: %v", err)
	}
}

// TestPullSymlinkPermissions records what happens on a platform where
// creating a symlink is privileged.
func TestPullSymlinkPermissions(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("symlink creation is unprivileged here")
	}
	_, fake := pushFixture(t)
	dest := filepath.Join(t.TempDir(), "downloaded")

	if _, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake)); err != nil {
		// A refusal has to name the link, so the operator can see why the tree
		// could not be reproduced.
		if !strings.Contains(err.Error(), "symlink") {
			t.Errorf("a failed download should name the symlink: %v", err)
		}
	}
}

// stageDirs lists the leftover staging directories beside a destination.
func stageDirs(t *testing.T, dest string) []string {
	t.Helper()
	parent, base := filepath.Split(dest)
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var stages []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), base+".tmp-b3-") {
			stages = append(stages, entry.Name())
		}
	}
	return stages
}

func hashPrefix(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return volume.Digest(blake3.Sum256(content)).Hex()[:16]
}

// TestPullRenewsShortLivedCredentials covers the credential renewal that keeps
// a long download alive, and the guard that stops it becoming a storm.
//
// The credentials a resolve hands out expire, and a download longer than the
// lease has to resolve the pinned digest again to get more. If the service
// ever issues leases shorter than the renewal margin, every one of them looks
// like it is about to expire — so without a guard, every object would trigger
// its own resolve, serialized behind the lock that renewal holds.
func TestPullRenewsShortLivedCredentials(t *testing.T) {
	_, fake := pushFixture(t)
	// Well inside the renewal margin, so the very first object wants a fresher
	// lease and so does every replacement.
	fake.leaseTTL = 5 * time.Second
	before := fake.resolveCount()

	dest := filepath.Join(t.TempDir(), "downloaded")
	result, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "assets"), 0o755) })

	// One resolve to start the download, and one renewal that turns out not to
	// help. Anything more is a resolve per object.
	if got := fake.resolveCount() - before; got != 2 {
		t.Errorf("resolved %d times for a %d-chunk download; expected the renewal to give up after one try",
			got, result.ChunksFetched)
	}
	if result.ChunksFetched != 6 {
		t.Errorf("fetched %d chunks, want 6", result.ChunksFetched)
	}
}

// TestPullWithoutCredentialExpiryNeverRenews covers local development, which
// issues static credentials with no stated expiry. Nothing should re-resolve.
func TestPullWithoutCredentialExpiryNeverRenews(t *testing.T) {
	_, fake := pushFixture(t)
	before := fake.resolveCount()

	dest := filepath.Join(t.TempDir(), "downloaded")
	if _, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "assets"), 0o755) })

	if got := fake.resolveCount() - before; got != 1 {
		t.Errorf("resolved %d times; credentials with no expiry never need renewing", got)
	}
}

// TestPullSubsetOfAnEmptyDirectoryThroughADirtyStage is the composition case:
// the selector, the plan's directory set, and prune all have to agree, and
// they once did not.
//
// Selecting an empty directory below the top level puts "nested/cache" in the
// plan with nothing to pull "nested" in alongside it. prune then walks the
// stage, finds "nested" unplanned, skips descending into it — so it never
// learns the selected directory is underneath — and removes the lot. The pull
// reports success having published nothing, and against a stale wider stage,
// which is the case prune exists for, it deletes content that was selected.
//
// The unit tests pin the selector half. This is the only thing that would
// notice if prune's idea of what is planned ever drifted from it.
func TestPullSubsetOfAnEmptyDirectoryThroughADirtyStage(t *testing.T) {
	_, fake := pushFixture(t)
	parent := t.TempDir()
	dest := filepath.Join(parent, "downloaded")

	// A complete tree, moved into the stage the next pull of this version will
	// find and try to continue.
	full, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake))
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(dest, "assets"), 0o755)
	if err := os.Rename(dest, dest+".tmp-b3-"+full.ManifestDigest.Hex()[:12]); err != nil {
		t.Fatal(err)
	}

	opts := pullOptions(dest, fake)
	opts.Include = []string{"nested/cache"}
	result, err := transfer.Pull(context.Background(), fake.client(t), opts)
	if err != nil {
		t.Fatal(err)
	}

	// The selected directory survives, with the mode the manifest recorded,
	// and its ancestor came along to hold it.
	info, err := os.Lstat(filepath.Join(dest, "nested", "cache"))
	if err != nil {
		t.Fatalf("the selected directory did not survive: %v", err)
	}
	if !info.IsDir() {
		t.Error("nested/cache is not a directory")
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("nested/cache mode is %04o, want 0700", got)
	}
	if got := treeDescription(t, dest); got != "dir nested 0755\ndir nested/cache 0700" {
		t.Errorf("published tree is\n%s", got)
	}
	if result.SelectedFiles != 0 || result.TotalFiles != 5 {
		t.Errorf("selected %d of %d files", result.SelectedFiles, result.TotalFiles)
	}
}

// TestPullRestoresTheSpecialBits is the guard the pure helper test cannot be:
// it drives a real push and pull, so it fails if the download stops sending
// recorded modes through the translation. That is the gap the encode side had
// a test for and the decode side did not.
func TestPullRestoresTheSpecialBits(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "binary")
	if err := os.WriteFile(path, []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Whichever of the three this filesystem will keep is enough: they take
	// the same path through the translation, and the point is that the path
	// is taken at all.
	var special fs.FileMode
	for _, candidate := range []struct {
		mode uint32
		bit  fs.FileMode
	}{{0o4755, fs.ModeSetuid}, {0o2755, fs.ModeSetgid}, {0o1755, fs.ModeSticky}} {
		if os.Chmod(path, os.FileMode(candidate.mode)) != nil {
			continue
		}
		if info, err := os.Stat(path); err == nil && info.Mode()&candidate.bit != 0 {
			special = candidate.bit
			break
		}
	}
	if special == 0 {
		t.Skip("this filesystem keeps none of the setuid, setgid or sticky bits")
	}

	fake := newFakeService(t)
	if _, err := transfer.Push(context.Background(), fake.client(t), pushOptions(root, fake)); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "downloaded")
	if _, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake)); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dest, "binary"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&special == 0 {
		t.Errorf("the downloaded file lost its %v bit: mode %v", special, info.Mode())
	}
}
