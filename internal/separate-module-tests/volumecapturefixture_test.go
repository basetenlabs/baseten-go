package separatemoduletests_test

// The cross-client compatibility anchor.
//
// Every other wire test in this repo checks the encoder against expectations
// derived from the same reading of the format that produced the encoder, so a
// misreading would agree with itself. These bytes did not come from here: they
// were captured from a live volume service: a synthetic tree was pushed into
// it, and what the service stored was read back through its own manifest and
// object endpoints.
//
// testdata/capture holds the captured manifest and chunkmap, the source URI
// that push recorded, and a description of the tree. The tree is described
// rather than checked in because one of its files is 16 MiB; the description
// carries everything the manifest depends on, which is each entry's path,
// mode, and contents.
//
// To regenerate, if the format changes: bring up a volume service and its
// object store, build the tree described by fixture.json at the path its
// source_uri names, push it, then save the manifest
// from the service's manifest endpoint and the chunkmap from its object
// endpoint, decompressing the latter.
//
// The source path matters and is part of the fixture: it goes into the
// provenance record, which is inside the bytes the digest covers. Pushing the
// same tree from anywhere else produces a different version.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/zeebo/blake3"
)

type captureFixture struct {
	SourceURI      string         `json:"source_uri"`
	ManifestDigest string         `json:"manifest_digest"`
	ChunkmapPath   string         `json:"chunkmap_path"`
	ChunkmapDigest string         `json:"chunkmap_digest"`
	Entries        []fixtureEntry `json:"entries"`
}

// fixtureEntry describes one entry of the pushed tree. Content is either
// "text:<literal>" or "pattern:<length>", the latter generating the bytes
// i%251 xor (i>>13) so a large file costs one line of testdata.
type fixtureEntry struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Content string `json:"content"`
	Target  string `json:"target"`
}

func loadCaptureFixture(t *testing.T) captureFixture {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "capture", "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture captureFixture
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "capture", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// buildFixtureTree recreates the pushed tree in a temporary directory.
func buildFixtureTree(t *testing.T, fixture captureFixture) string {
	t.Helper()
	root := t.TempDir()

	// Directories are created permissive and given their recorded modes last,
	// for the same reason a download does it: one of them is read-only, and
	// its own contents have to be written first.
	for _, entry := range fixture.Entries {
		abs := filepath.Join(root, filepath.FromSlash(entry.Path))
		switch entry.Kind {
		case "dir":
			if err := os.MkdirAll(abs, 0o755); err != nil {
				t.Fatal(err)
			}
		case "file":
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(abs, fixtureContent(t, entry.Content), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(abs, fixtureMode(t, entry.Mode)); err != nil {
				t.Fatal(err)
			}
		case "symlink":
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(entry.Target, abs); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		default:
			t.Fatalf("unknown fixture entry kind %q", entry.Kind)
		}
	}

	dirs := []fixtureEntry{}
	for _, entry := range fixture.Entries {
		if entry.Kind == "dir" {
			dirs = append(dirs, entry)
		}
	}
	slices.SortFunc(dirs, func(a, b fixtureEntry) int { return strings.Compare(b.Path, a.Path) })
	for _, entry := range dirs {
		if err := os.Chmod(filepath.Join(root, filepath.FromSlash(entry.Path)), fixtureMode(t, entry.Mode)); err != nil {
			t.Fatal(err)
		}
	}
	// A read-only directory would otherwise defeat the temporary directory's
	// own cleanup.
	t.Cleanup(func() {
		for _, entry := range dirs {
			_ = os.Chmod(filepath.Join(root, filepath.FromSlash(entry.Path)), 0o755)
		}
	})
	return root
}

func fixtureContent(t *testing.T, spec string) []byte {
	t.Helper()
	if literal, ok := strings.CutPrefix(spec, "text:"); ok {
		return []byte(literal)
	}
	size, ok := strings.CutPrefix(spec, "pattern:")
	if !ok {
		t.Fatalf("unknown fixture content %q", spec)
	}
	n, err := strconv.Atoi(size)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, n)
	for i := range data {
		data[i] = byte((i % 251) ^ ((i >> 13) & 0xFF))
	}
	return data
}

func fixtureMode(t *testing.T, mode string) os.FileMode {
	t.Helper()
	parsed, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		t.Fatal(err)
	}
	return os.FileMode(parsed)
}

// TestManifestMatchesServiceCapture rebuilds the manifest for the pushed tree
// and asserts it is byte-for-byte what the service stored.
//
// Byte equality is the assertion that matters. Divergence would not corrupt
// anything — content addressing sees to that — but it would give the same tree
// two different digests depending on which client pushed it, so neither could
// reuse the other's objects.
func TestManifestMatchesServiceCapture(t *testing.T) {
	// Byte identity embeds the recorded modes, so it holds only where the
	// filesystem can express them; the probe measures that instead of
	// guessing by platform name. The chunkmap capture test stays unguarded —
	// chunk bytes carry no modes — and the decode-only capture tests run
	// everywhere.
	requireExpressibleModes(t)
	fixture := loadCaptureFixture(t)
	root := buildFixtureTree(t, fixture)

	source, err := volume.ScanSource(root)
	if err != nil {
		t.Fatal(err)
	}

	files := make([]volume.FileEntry, 0, len(source.Files))
	for _, file := range source.Files {
		files = append(files, buildFileEntry(t, root, file))
	}
	manifest := volume.NewManifest(source, fixture.SourceURI, files)
	encoded := volume.EncodeManifest(manifest)

	want := fixtureBytes(t, "manifest.jsonl")
	if string(encoded) != string(want) {
		t.Fatalf("manifest bytes differ from the capture\n got:\n%s\nwant:\n%s", encoded, want)
	}

	digest := volume.Digest(blake3.Sum256(encoded))
	if digest.String() != fixture.ManifestDigest {
		t.Errorf("manifest digest is %s, the capture records %s", digest, fixture.ManifestDigest)
	}
}

// TestChunkmapMatchesServiceCapture does the same for the one file large
// enough to need a chunkmap. It is also what pins the chunk boundaries: the
// file is two full chunks and a 1 KiB remainder, and the remainder stays its
// own chunk rather than being folded into the one before it. A client that
// coalesced it would share no chunks with this one for any large file.
func TestChunkmapMatchesServiceCapture(t *testing.T) {
	fixture := loadCaptureFixture(t)
	root := buildFixtureTree(t, fixture)

	source, err := volume.ScanSource(root)
	if err != nil {
		t.Fatal(err)
	}
	var target volume.SourceFile
	for _, file := range source.Files {
		if file.Path == fixture.ChunkmapPath {
			target = file
		}
	}
	if target.Path == "" {
		t.Fatalf("no file %q in the scanned tree", fixture.ChunkmapPath)
	}

	chunkmap := buildChunkmap(t, root, target)
	if err := volume.ValidateChunkmap(chunkmap); err != nil {
		t.Fatal(err)
	}
	encoded := volume.EncodeChunkmap(chunkmap)

	want := fixtureBytes(t, "chunkmap.jsonl")
	if string(encoded) != string(want) {
		t.Fatalf("chunkmap bytes differ from the capture\n got:\n%s\nwant:\n%s", encoded, want)
	}

	digest := volume.Digest(blake3.Sum256(encoded))
	if digest.String() != fixture.ChunkmapDigest {
		t.Errorf("chunkmap digest is %s, the capture records %s", digest, fixture.ChunkmapDigest)
	}

	if len(chunkmap.Chunks) != 3 || chunkmap.Chunks[2].Length != 1024 {
		t.Errorf("expected two full chunks and a 1 KiB tail, got %d chunks", len(chunkmap.Chunks))
	}
}

// TestCapturedTargetsAreDerivable checks that every target the server chose
// is the one TargetForDigest builds. The push path uses the
// target the server echoes rather than deriving it, so this is not something
// the client depends on — but it does mean a reader holding only a digest can
// find the object, which the resume and verification paths rely on.
func TestCapturedTargetsAreDerivable(t *testing.T) {
	manifest, err := volume.DecodeManifest(fixtureBytes(t, "manifest.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	checked := 0
	for _, file := range manifest.Files {
		switch file.Kind {
		case volume.FileKindChunk:
			if got := volume.TargetForDigest(file.Chunk.Digest); got != file.Chunk.Target {
				t.Errorf("%s: derived %q, the server chose %q", file.Path, got.RelativeKey, file.Chunk.Target.RelativeKey)
			}
		case volume.FileKindChunkmap:
			if got := volume.TargetForDigest(file.Digest); got != file.Target {
				t.Errorf("%s: derived %q, the server chose %q", file.Path, got.RelativeKey, file.Target.RelativeKey)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no file entries to check")
	}
}

// TestCapturedManifestDecodes checks the other direction: the decoder reads
// the captured manifest, and re-encoding it reproduces the input.
func TestCapturedManifestDecodes(t *testing.T) {
	want := fixtureBytes(t, "manifest.jsonl")
	manifest, err := volume.DecodeManifest(want)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(volume.EncodeManifest(manifest)); got != string(want) {
		t.Errorf("re-encoding the captured manifest changed it\n got:\n%s\nwant:\n%s", got, want)
	}

	fixture := loadCaptureFixture(t)
	if manifest.Provenance.SourceURI != fixture.SourceURI {
		t.Errorf("provenance uri is %q, want %q", manifest.Provenance.SourceURI, fixture.SourceURI)
	}
	if manifest.Provenance.SourceFingerprint != volume.ProvenanceFingerprint ||
		manifest.Provenance.SourceFingerprintType != volume.ProvenanceFingerprintType {
		t.Errorf("provenance literals are %q/%q",
			manifest.Provenance.SourceFingerprint, manifest.Provenance.SourceFingerprintType)
	}

	// The empty file names the digest of no bytes at all, which is a real
	// object the push uploads.
	for _, file := range manifest.Files {
		if file.Path == "empty.txt" {
			if want := volume.Digest(blake3.Sum256(nil)); file.Chunk.Digest != want {
				t.Errorf("empty file names %s, want %s", file.Chunk.Digest, want)
			}
		}
	}
}

// TestStoredObjectDecodesByContentType reads the chunkmap exactly as the
// server stored it — the raw zstd bytes, with the media type the server
// reported alongside them — and checks that the decode path recovers the
// canonical JSONL.
//
// Every other test of that path decompresses bytes this repo's own fake
// compressed. These came off the real server, which decided on its own to
// store the chunkmap compressed; the media type is the only thing that says so,
// and a reader that guessed from the key would get it wrong.
func TestStoredObjectDecodesByContentType(t *testing.T) {
	stored := fixtureBytes(t, "chunkmap.v1+zstd.bin")
	contentType := strings.TrimSpace(strings.TrimPrefix(
		strings.ToLower(string(fixtureBytes(t, "chunkmap.content-type"))), "content-type:"))
	if contentType != "application/vnd.baseten.bdn.chunkmap.v1+zstd" {
		t.Fatalf("captured media type is %q", contentType)
	}

	download := func(context.Context, volume.ObjectDownload) (*volume.ObjectResult, error) {
		return &volume.ObjectResult{
			Body:        io.NopCloser(bytes.NewReader(stored)),
			ContentType: contentType,
			Size:        int64(len(stored)),
		}, nil
	}

	decoded, err := volume.FetchObject(context.Background(), download, newZstdReader,
		volume.ObjectDownload{Key: "objects/b3/aa/44/chunkmap"}, volume.MaxChunkmapBytes)
	if err != nil {
		t.Fatal(err)
	}
	if want := fixtureBytes(t, "chunkmap.jsonl"); string(decoded) != string(want) {
		t.Fatalf("decoded bytes differ from the captured canonical form\n got:\n%s\nwant:\n%s", decoded, want)
	}

	chunkmap, err := volume.DecodeChunkmap(decoded)
	if err != nil {
		t.Fatal(err)
	}
	fixture := loadCaptureFixture(t)
	if digest := volume.Digest(blake3.Sum256(decoded)); digest.String() != fixture.ChunkmapDigest {
		// The digest covers the uncompressed bytes, never what was stored.
		t.Errorf("digest of the decompressed bytes is %s, want %s", digest, fixture.ChunkmapDigest)
	}
	if len(chunkmap.Chunks) != 3 {
		t.Errorf("decoded %d chunks, want 3", len(chunkmap.Chunks))
	}
}

// buildFileEntry chunks and hashes one file the way a push would, without a
// network. The targets a real push records come from the server; this uses the
// derived form, which TestCapturedTargetsAreDerivable shows is the same.
func buildFileEntry(t *testing.T, root string, file volume.SourceFile) volume.FileEntry {
	t.Helper()
	entry := volume.FileEntry{Path: file.Path, Mode: file.Mode, Size: file.Size}

	chunkmap := buildChunkmap(t, root, file)
	if len(chunkmap.Chunks) == 1 {
		entry.Kind = volume.FileKindChunk
		entry.Chunk = chunkmap.Chunks[0]
		return entry
	}
	encoded := volume.EncodeChunkmap(chunkmap)
	entry.Kind = volume.FileKindChunkmap
	entry.Digest = volume.Digest(blake3.Sum256(encoded))
	entry.Target = volume.TargetForDigest(entry.Digest)
	return entry
}

func buildChunkmap(t *testing.T, root string, file volume.SourceFile) *volume.Chunkmap {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.Path)))
	if err != nil {
		t.Fatal(err)
	}

	chunkmap := &volume.Chunkmap{FileSize: file.Size}
	for _, span := range volume.ChunkRanges(file.Size) {
		chunk := body[span.Offset : span.Offset+span.Length]
		digest := volume.Digest(blake3.Sum256(chunk))
		chunkmap.Chunks = append(chunkmap.Chunks, volume.ChunkRef{
			Digest: digest,
			Length: span.Length,
			Offset: span.Offset,
			Target: volume.TargetForDigest(digest),
		})
	}
	return chunkmap
}
