package separatemoduletests_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
	"github.com/basetenlabs/baseten-go/internal/volume/transfer"
	"github.com/zeebo/blake3"
)

// buildTree writes a tree exercising every shape a manifest can describe: an
// empty file, a file inside one chunk, a file spanning several, a symlink, and
// a directory whose mode would stop its own children being written if it were
// applied too early.
func buildTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	mkdir(t, root, "nested/deep")
	mkdir(t, root, "assets")
	// An empty directory below the top level. Nothing inside it can pull its
	// parent into a subset plan, which is the shape that once let prune delete
	// the very directory that was selected.
	mkdir(t, root, "nested/cache")
	writeFile(t, root, "empty.txt", nil, 0o644)
	writeFile(t, root, "small.txt", []byte("hello volume"), 0o600)
	writeFile(t, root, "nested/deep/data.bin", patternBytes(volume.ChunkSize*2+1024), 0o644)
	writeFile(t, root, "nested/dup.txt", []byte("hello volume"), 0o644)
	writeFile(t, root, "assets/read-only.txt", []byte("frozen"), 0o444)

	if err := os.Symlink("../small.txt", filepath.Join(root, "nested", "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Chmod(filepath.Join(root, "assets"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "nested", "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "assets"), 0o755) })
	return root
}

func mkdir(t *testing.T, root, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(path)), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, root, path string, content []byte, mode os.FileMode) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(path))
	if err := os.WriteFile(abs, content, mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is masked by the umask, so the mode the manifest should
	// record is set explicitly.
	if err := os.Chmod(abs, mode); err != nil {
		t.Fatal(err)
	}
}

// patternBytes builds compressible but non-uniform content, so chunk digests
// differ from each other.
func patternBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i%251) ^ byte(i>>13)
	}
	return b
}

func pushOptions(root string, f *fakeService) transfer.PushOptions {
	return transfer.PushOptions{
		Namespace:      fakeNamespace,
		Volume:         fakeVolume,
		SourceDir:      root,
		SourceURI:      "file:///fixture",
		NewHasher:      newBlake3,
		DownloadObject: f.downloader(),
		Decompress:     newZstdReader,
	}
}

func TestPushPublishesTheWholeTree(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)

	result, err := transfer.Push(context.Background(), fake.client(t), pushOptions(root, fake))
	if err != nil {
		t.Fatal(err)
	}

	// Five files, and every one of them accounted for.
	if result.Files != 5 {
		t.Errorf("pushed %d files, want 5", result.Files)
	}
	wantBytes := int64(len("hello volume")*2 + len("frozen") + volume.ChunkSize*2 + 1024)
	if result.Bytes != wantBytes {
		t.Errorf("pushed %d bytes, want %d", result.Bytes, wantBytes)
	}

	// The manifest names every entry, grouped and sorted, with the modes the
	// tree carried.
	manifest, err := volume.DecodeManifest(fake.manifestBytes(t, result.ManifestDigest.String()))
	if err != nil {
		t.Fatal(err)
	}
	if got := manifestPaths(manifest); got != "assets,nested,nested/cache,nested/deep|assets/read-only.txt,empty.txt,nested/deep/data.bin,nested/dup.txt,small.txt|nested/link" {
		t.Errorf("manifest entries are %s", got)
	}
	if got := fileEntry(t, manifest, "small.txt").Mode; got != 0o600 {
		t.Errorf("small.txt mode is %04o, want 0600", got)
	}
	if got := dirEntry(t, manifest, "assets").Mode; got != 0o555 {
		t.Errorf("assets mode is %04o, want 0555", got)
	}
	if got := manifest.Symlinks[0].Target; got != "../small.txt" {
		t.Errorf("symlink target is %q", got)
	}
	if manifest.Provenance.SourceURI != "file:///fixture" {
		t.Errorf("provenance uri is %q", manifest.Provenance.SourceURI)
	}

	// An empty file is a real entry naming a real object: the digest of no
	// bytes at all.
	empty := fileEntry(t, manifest, "empty.txt")
	if empty.Kind != volume.FileKindChunk || empty.Size != 0 {
		t.Errorf("empty.txt is %s of %d bytes", empty.Kind, empty.Size)
	}
	if want := (volume.Digest(blake3.Sum256(nil))); empty.Chunk.Digest != want {
		t.Errorf("empty.txt chunk digest is %s, want %s", empty.Chunk.Digest, want)
	}

	// One chunk is named inline; more than one needs a chunkmap.
	if got := fileEntry(t, manifest, "small.txt").Kind; got != volume.FileKindChunk {
		t.Errorf("small.txt is %s, want an inline chunk", got)
	}
	big := fileEntry(t, manifest, "nested/deep/data.bin")
	if big.Kind != volume.FileKindChunkmap {
		t.Fatalf("data.bin is %s, want a chunkmap", big.Kind)
	}
	if big.Size != volume.ChunkSize*2+1024 {
		t.Errorf("data.bin size is %d", big.Size)
	}

	// The chunkmap tiles the file in fixed chunks, with the remainder as a
	// short final chunk rather than folded into the one before it.
	chunkmap := readChunkmap(t, fake, big.Digest)
	if len(chunkmap.Chunks) != 3 {
		t.Fatalf("data.bin has %d chunks, want 3", len(chunkmap.Chunks))
	}
	for i, want := range []uint64{volume.ChunkSize, volume.ChunkSize, 1024} {
		if chunkmap.Chunks[i].Length != want {
			t.Errorf("chunk %d is %d bytes, want %d", i, chunkmap.Chunks[i].Length, want)
		}
	}

	if len(fake.commits) != 1 {
		t.Fatalf("%d commits, want 1", len(fake.commits))
	}
	commit := fake.commits[0]
	if commit.manifestDigest != result.ManifestDigest.String() || !commit.updateHead {
		t.Errorf("committed %s, update_head %v", commit.manifestDigest, commit.updateHead)
	}
	if commit.idempotencyKey == "" {
		t.Error("commit carried no idempotency key")
	}
	if !result.HeadUpdated || result.HeadMoveDenied {
		t.Errorf("head updated %v, denied %v", result.HeadUpdated, result.HeadMoveDenied)
	}
}

// TestPushSendsIdenticalBytesOnce covers in-session deduplication: two files
// with the same content are one object on the wire.
//
// The files are pushed one at a time here, because the deduplication is
// deliberately racy — two goroutines can miss at the same moment and both
// upload, which costs a duplicate request and nothing else. Serializing every
// lookup behind the upload it guards would cost more than the duplicates do.
func TestPushSendsIdenticalBytesOnce(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)

	opts := pushOptions(root, fake)
	opts.Concurrency.FileJobs = 1
	result, err := transfer.Push(context.Background(), fake.client(t), opts)
	if err != nil {
		t.Fatal(err)
	}

	// small.txt and nested/dup.txt hold the same twelve bytes, so only one of
	// them crosses the wire.
	const duplicate = "hello volume"
	sent := 0
	for _, upload := range fake.uploads {
		if upload.contentType == bdn.ContentTypeChunk && upload.size == len(duplicate) {
			sent++
		}
	}
	if sent != 1 {
		t.Errorf("sent the duplicated content %d times, want 1", sent)
	}
	if result.Reused < 1 {
		t.Errorf("reused %d objects, want at least the deduplicated one", result.Reused)
	}

	// The uploaded bytes are the tree's distinct content, not its total size.
	wantBytes := len(duplicate) + len("frozen") + volume.ChunkSize*2 + 1024
	if got := fake.uploadedBytes(); got != wantBytes {
		t.Errorf("uploaded %d bytes, want %d", got, wantBytes)
	}
}

// TestPushReusesUnchangedFiles is the point of reading the previous version: a
// second push of the same tree should send no chunk bytes at all.
func TestPushReusesUnchangedFiles(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)
	ctx := context.Background()

	first, err := transfer.Push(ctx, fake.client(t), pushOptions(root, fake))
	if err != nil {
		t.Fatal(err)
	}
	fake.reset()

	second, err := transfer.Push(ctx, fake.client(t), pushOptions(root, fake))
	if err != nil {
		t.Fatal(err)
	}

	if second.ManifestDigest != first.ManifestDigest {
		t.Errorf("the same tree produced %s then %s", first.ManifestDigest, second.ManifestDigest)
	}
	if got := fake.uploadCount(bdn.ContentTypeChunk); got != 0 {
		t.Errorf("re-uploaded %d chunks for an unchanged tree", got)
	}
	// The chunkmap is not rebuilt either: every chunk matched, so the previous
	// file entry still describes the file.
	if got := fake.uploadCount(bdn.ContentTypeChunkmap); got != 0 {
		t.Errorf("re-uploaded %d chunkmaps for an unchanged tree", got)
	}
	// The manifest is re-sent, and the service reports it as one it already
	// has rather than as a conflict.
	if got := fake.uploadCount(bdn.ContentTypeManifest); got != 1 {
		t.Errorf("sent %d manifests, want 1", got)
	}
	if second.Existing != 1 {
		t.Errorf("%d objects reported as already stored, want 1", second.Existing)
	}
}

// TestPushRefusesAnUncontainedTree covers the client half of the containment
// rule: a tree whose links leave the volume or dangle fails before the first
// byte uploads, with the entry named — not at the commit gate with the whole
// upload already spent. The absolute case pins the reinterpretation too: an
// absolute target means the volume's own path, so without such an entry it
// dangles rather than pointing at the pushing machine's file.
func TestPushRefusesAnUncontainedTree(t *testing.T) {
	cases := []struct{ name, target, want string }{
		{"escaping link", "../../etc/passwd", "steps outside the volume root"},
		{"dangling link", "no-such-file", "dangles"},
		{"absolute link with no matching entry", "/etc/passwd", "dangles"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "kept.txt", []byte("x"), 0o644)
			if err := os.Symlink(c.target, filepath.Join(root, "esc")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			fake := newFakeService(t)

			_, err := transfer.Push(context.Background(), fake.client(t), pushOptions(root, fake))
			if err == nil {
				t.Fatal("an uncontained tree was pushed")
			}
			if !errors.Is(err, volume.ErrNotContained) ||
				!strings.Contains(err.Error(), "esc") || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want ErrNotContained naming esc and %q, got: %v", c.want, err)
			}
			if got := len(fake.uploads); got != 0 {
				t.Errorf("%d uploads were attempted for a tree that must fail before the first", got)
			}
			if got := len(fake.commits); got != 0 {
				t.Errorf("%d commits recorded for a refused push", got)
			}
		})
	}
}

// TestPushCountsAKeptChunkmap pins the documented meaning of the object
// counts across a wholesale reuse. The count promises every object the push
// accounted for — chunks, chunkmaps and the manifest — and a multi-chunk file
// reused from the previous version keeps its chunkmap without making a
// request, which is exactly what the reused partition means. The single-chunk
// file is here for the other direction: its entry names the chunk inline, so
// its reuse must add nothing beyond the chunk itself.
func TestPushCountsAKeptChunkmap(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "one.txt", []byte("hello"), 0o644)
	writeFile(t, root, "big.bin", patternBytes(volume.ChunkSize+1024), 0o644)
	fake := newFakeService(t)
	ctx := context.Background()

	if _, err := transfer.Push(ctx, fake.client(t), pushOptions(root, fake)); err != nil {
		t.Fatal(err)
	}
	fake.reset()

	second, err := transfer.Push(ctx, fake.client(t), pushOptions(root, fake))
	if err != nil {
		t.Fatal(err)
	}

	if second.Reused != 4 {
		t.Errorf("reused %d objects, want 4: three chunks and the kept chunkmap", second.Reused)
	}
	if second.Existing != 1 {
		t.Errorf("%d objects reported as already stored, want the re-sent manifest alone", second.Existing)
	}
	if second.Unique != 0 {
		t.Errorf("%d objects reported as new on an unchanged push, want 0", second.Unique)
	}
	if second.Chunks != 5 {
		t.Errorf("accounted for %d objects, want 5: three chunks, one chunkmap, one manifest", second.Chunks)
	}
}

// TestPushSendsOnlyChangedChunks covers a large file edited in one place: the
// chunks around the edit are reused and only the changed one is sent.
func TestPushSendsOnlyChangedChunks(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)
	ctx := context.Background()

	if _, err := transfer.Push(ctx, fake.client(t), pushOptions(root, fake)); err != nil {
		t.Fatal(err)
	}
	fake.reset()

	// Change one byte in the middle chunk, keeping the file's size the same so
	// the chunk boundaries do not move.
	data := patternBytes(volume.ChunkSize*2 + 1024)
	data[volume.ChunkSize+10] ^= 0xff
	writeFile(t, root, "nested/deep/data.bin", data, 0o644)

	result, err := transfer.Push(ctx, fake.client(t), pushOptions(root, fake))
	if err != nil {
		t.Fatal(err)
	}

	if got := fake.uploadedBytes(); got != volume.ChunkSize {
		t.Errorf("uploaded %d bytes, want one chunk of %d", got, volume.ChunkSize)
	}
	// The file's chunk list changed, so its chunkmap is rebuilt even though
	// two of its three chunks were reused.
	if got := fake.uploadCount(bdn.ContentTypeChunkmap); got != 1 {
		t.Errorf("sent %d chunkmaps, want 1", got)
	}
	if result.Reused < 2 {
		t.Errorf("reused %d objects, want at least the two unchanged chunks", result.Reused)
	}
}

// TestPushRepublishesAfterAModeChange covers a file whose bytes are identical
// and whose permissions are not. Reusing the previous entry wholesale would
// publish the old mode and silently lose the change.
func TestPushRepublishesAfterAModeChange(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)
	ctx := context.Background()

	first, err := transfer.Push(ctx, fake.client(t), pushOptions(root, fake))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "small.txt"), 0o640); err != nil {
		t.Fatal(err)
	}
	fake.reset()

	second, err := transfer.Push(ctx, fake.client(t), pushOptions(root, fake))
	if err != nil {
		t.Fatal(err)
	}

	if second.ManifestDigest == first.ManifestDigest {
		t.Fatal("a mode change produced the same manifest")
	}
	if got := fake.uploadedBytes(); got != 0 {
		t.Errorf("uploaded %d bytes for a mode-only change", got)
	}
	manifest, err := volume.DecodeManifest(fake.manifestBytes(t, second.ManifestDigest.String()))
	if err != nil {
		t.Fatal(err)
	}
	if got := fileEntry(t, manifest, "small.txt").Mode; got != 0o640 {
		t.Errorf("small.txt mode is %04o, want 0640", got)
	}
}

// TestPushWithoutTheReuseSeams covers the configuration that has no way to
// read object storage: it uploads everything, and publishes the same manifest.
func TestPushWithoutTheReuseSeams(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)
	ctx := context.Background()

	withSeams, err := transfer.Push(ctx, fake.client(t), pushOptions(root, fake))
	if err != nil {
		t.Fatal(err)
	}
	fake.reset()

	bare := pushOptions(root, fake)
	bare.DownloadObject, bare.Decompress = nil, nil
	withoutSeams, err := transfer.Push(ctx, fake.client(t), bare)
	if err != nil {
		t.Fatal(err)
	}

	if withoutSeams.ManifestDigest != withSeams.ManifestDigest {
		t.Errorf("reuse changed the published manifest: %s then %s",
			withSeams.ManifestDigest, withoutSeams.ManifestDigest)
	}
	if got := fake.uploadCount(bdn.ContentTypeChunk); got == 0 {
		t.Error("expected the whole tree to be re-sent without the reuse seams")
	}
}

// TestPushSurvivesATransientFailure covers the retry path with a real client
// and a real session: a shedding service costs attempts, not correctness.
func TestPushSurvivesATransientFailure(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)
	fake.failNextUploads = 3

	result, err := transfer.Push(context.Background(), fake.client(t), pushOptions(root, fake))
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 5 {
		t.Errorf("pushed %d files, want 5", result.Files)
	}
}

// TestPushAppliesTags covers tags travelling with the commit rather than as a
// separate call that could half-succeed.
func TestPushAppliesTags(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)

	opts := pushOptions(root, fake)
	opts.Tags = []string{"prod", "v1"}
	result, err := transfer.Push(context.Background(), fake.client(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.TagsApplied, ",") != "prod,v1" {
		t.Errorf("applied tags %v", result.TagsApplied)
	}
	if strings.Join(fake.commits[0].tags, ",") != "prod,v1" {
		t.Errorf("commit carried tags %v", fake.commits[0].tags)
	}
}

// TestPushWithoutHeadPermission covers a token whose grants do not reach the
// reserved head tag. The version is still published; what changes is that refs
// without a tag keep resolving to the old one, and the result says so.
func TestPushWithoutHeadPermission(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)

	client, err := bdn.New(bdn.Options{
		HTTPClient: fake.server.Client(),
		Tokens: func(context.Context, string) (string, string, error) {
			// Push permission, but only for one named tag.
			return makeGrantToken(fakeOrg, fakeNamespace, fakeVolume, "prod", "push"), fake.server.URL, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := transfer.Push(context.Background(), client, pushOptions(root, fake))
	if err != nil {
		t.Fatal(err)
	}
	if !result.HeadMoveDenied || result.HeadUpdated {
		t.Errorf("head denied %v, updated %v", result.HeadMoveDenied, result.HeadUpdated)
	}
	if fake.commits[0].updateHead {
		t.Error("the commit asked to move head anyway")
	}

	// The same options with RequireHeadMove fail before anything is uploaded.
	opts := pushOptions(root, fake)
	opts.RequireHeadMove = true
	if _, err := transfer.Push(context.Background(), client, opts); err == nil {
		t.Fatal("RequireHeadMove should have failed the push")
	}
}

func manifestPaths(m *volume.Manifest) string {
	var dirs, files, links []string
	for _, d := range m.Directories {
		dirs = append(dirs, d.Path)
	}
	for _, f := range m.Files {
		files = append(files, f.Path)
	}
	for _, l := range m.Symlinks {
		links = append(links, l.Path)
	}
	return strings.Join(dirs, ",") + "|" + strings.Join(files, ",") + "|" + strings.Join(links, ",")
}

func fileEntry(t *testing.T, m *volume.Manifest, path string) volume.FileEntry {
	t.Helper()
	for _, f := range m.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no file %q in the manifest", path)
	return volume.FileEntry{}
}

func dirEntry(t *testing.T, m *volume.Manifest, path string) volume.DirectoryEntry {
	t.Helper()
	for _, d := range m.Directories {
		if d.Path == path {
			return d
		}
	}
	t.Fatalf("no directory %q in the manifest", path)
	return volume.DirectoryEntry{}
}

func readChunkmap(t *testing.T, f *fakeService, digest volume.Digest) *volume.Chunkmap {
	t.Helper()
	body, err := volume.FetchObject(context.Background(), f.downloader(), newZstdReader,
		volume.ObjectDownload{
			Bucket: fakeBucket,
			Key:    volume.ObjectKey(fakeOrg, fakeNamespace, volume.TargetForDigest(digest)),
		}, volume.MaxChunkmapBytes)
	if err != nil {
		t.Fatal(err)
	}
	chunkmap, err := volume.DecodeChunkmap(body)
	if err != nil {
		t.Fatal(err)
	}
	return chunkmap
}
