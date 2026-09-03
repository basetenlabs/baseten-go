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
// The tree includes entries chosen to make the capture discriminating: a
// directory with a child alongside a similarly named file (a, a/b, a.txt),
// on which canonical path order and plain bytewise order visibly disagree,
// and entry kinds that interleave rather than group. A regenerated tree must
// keep such a pair — a capture that both orderings reproduce pins neither
// the comparator nor the interleaving.
//
// The source path matters and is part of the fixture: it goes into the
// provenance record, which is inside the bytes the digest covers. Pushing the
// same tree from anywhere else produces a different version.
//
// What the capture proves, in its honest form: our manifest is byte-identical
// to the service's modulo the two ownership keys we deliberately do not emit —
// with the chunkmap still identical outright, digest included. The capturing
// producer records a uid and gid on every entry; ownership stays out of this
// client's manifests by the format owner's own ruling. So one reconciliation
// is applied before the manifest byte comparison, and only there: a
// whitelist-checking strip removes exactly those two keys with byte-level
// surgery (never by decoding and re-encoding, which would launder the capture
// through the very encoder under test) and refuses, by name, any key it does
// not model. The recorded manifest digest covers the raw bytes and therefore
// retires as a conformance pin: it remains only as fixture integrity, and our
// digests differ from this producer's for the same tree by exactly these two
// keys.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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

	// MTime is the modification time the tree was built with, which the
	// manifest records. The fixture's JSON also carries each entry's uid and
	// gid — the producer's own record of ownership — which nothing here
	// reads: ownership never enters this client's encoder, and the strip
	// transform is what reconciles the byte comparison.
	MTime string `json:"mtime"`
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
			mt := fixtureTime(t, entry.MTime)
			if err := os.Chtimes(abs, mt, mt); err != nil {
				t.Fatal(err)
			}
		case "symlink":
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(entry.Target, abs); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if err := setSymlinkTime(abs, fixtureTime(t, entry.MTime)); err != nil {
				t.Fatal(err)
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
	// Directory times go on here, after every write into the tree: writing an
	// entry into a directory moves the directory's mtime, so a time stamped
	// earlier would be stamped over. Before the chmod, so a read-only mode
	// never has to be worked around.
	for _, entry := range dirs {
		abs := filepath.Join(root, filepath.FromSlash(entry.Path))
		mt := fixtureTime(t, entry.MTime)
		if err := os.Chtimes(abs, mt, mt); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(abs, fixtureMode(t, entry.Mode)); err != nil {
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

// fixtureTime parses an entry's recorded time. Every entry of this fixture
// carries one — the capture was built with deterministic times — so absence
// is a broken fixture, not an option.
func fixtureTime(t *testing.T, mtime string) time.Time {
	t.Helper()
	if mtime == "" {
		t.Fatal("fixture entry has no mtime — the capture is built with deterministic times")
	}
	parsed, err := time.Parse(time.RFC3339Nano, mtime)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
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

	raw := fixtureBytes(t, "manifest.jsonl")
	want := mustStrip(t, raw)
	if bytes.Contains(want, []byte(`"uid":`)) || bytes.Contains(want, []byte(`"gid":`)) {
		t.Fatal("ownership keys survived the strip")
	}
	// The three truncation witnesses must survive the strip intact — the
	// transform must not have touched a byte it was not aimed at, and the
	// witnesses are what make this capture discriminate the fraction
	// formatting at all.
	for _, witness := range []string{
		`"mtime":"2026-03-04T16:50:15Z"`,
		`"mtime":"2026-03-04T16:50:15.5Z"`,
		`"mtime":"2026-03-04T16:50:15.123456789Z"`,
	} {
		if !bytes.Contains(want, []byte(witness)) {
			t.Fatalf("truncation witness %s is missing from the stripped capture", witness)
		}
	}
	// The claim, in its honest form: byte-identical to the service's
	// manifest modulo the two ownership keys we deliberately do not emit —
	// with the chunkmap still identical outright, digest included.
	if string(encoded) != string(want) {
		t.Fatalf("manifest bytes differ from the capture modulo ownership\n got:\n%s\nwant:\n%s", encoded, want)
	}

	// FIXTURE INTEGRITY, not a conformance pin: the captured digest covers
	// the full raw bytes, ownership included, so it cannot be compared
	// modulo a transform — the manifest-digest conformance pin is retired,
	// and our digests differ from this producer's for the same tree by
	// exactly the two stripped keys. This assertion only checks that the
	// checked-in bytes are still the ones the digest was recorded over; no
	// digest of the stripped form is minted or asserted anywhere.
	digest := volume.Digest(blake3.Sum256(raw))
	if digest.String() != fixture.ManifestDigest {
		t.Errorf("captured manifest bytes no longer match their recorded digest: %s vs %s", digest, fixture.ManifestDigest)
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
// the RAW captured manifest — ownership keys included, which is the
// unknown-key tolerance doing its job — and re-encoding it reproduces our
// canonical form of the same manifest, which is the capture minus exactly
// the ownership keys this encoder never emits.
func TestCapturedManifestDecodes(t *testing.T) {
	raw := fixtureBytes(t, "manifest.jsonl")
	manifest, err := volume.DecodeManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(volume.EncodeManifest(manifest)), string(mustStrip(t, raw)); got != want {
		t.Errorf("re-encoding the captured manifest is not the capture modulo ownership\n got:\n%s\nwant:\n%s", got, want)
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
	entry := volume.FileEntry{Path: file.Path, Mode: file.Mode, Size: file.Size, MTime: file.MTime}

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

// The capture's consumers fall into exactly two bins, and a new consumer must
// land in one deliberately:
//
//   - Bin 1, pure DECODE: runs on the RAW captured bytes, ownership keys
//     included — the decoder's unknown-key tolerance is itself conformance
//     evidence, and transforming first would launder that away.
//     TestCapturedTargetsAreDerivable, TestStoredObjectDecodesByContentType,
//     the decode half of TestCapturedManifestDecodes, and the transform's own
//     coverage leg all read raw.
//   - Bin 2, RE-ENCODE and byte-compare: takes the STRIPPED reference,
//     because this encoder never emits the ownership keys.
//     TestManifestMatchesServiceCapture and the re-encode half of
//     TestCapturedManifestDecodes. The chunkmap comparisons stay on the raw
//     bytes at full strength — the chunkmap carries no ownership keys, so
//     for it raw and stripped are the same bytes and its pin, digest
//     included, is unmodified.

// modelledCaptureKeys is the whole key vocabulary this transform accounts
// for, per record type, nested keys included. The strip removes exactly two
// of them — gid and uid, the ownership pair the capturing producer records
// on every entry and this encoder deliberately never emits. Two keys only:
// the format owner's review names other optional metadata (xattrs, link
// groups) that THIS capture does not contain, and a strip is never written
// for a key the producer did not emit — when one appears, the whitelist
// below refuses it by name so it gets decided about, not swallowed.
var modelledCaptureKeys = map[string]map[string]bool{
	"manifest_header": keySet("_type", "entry_count", "manifest_schema", "total_size"),
	"provenance":      keySet("_type", "source_fingerprint", "source_fingerprint_type", "source_uri"),
	"directory":       keySet("_type", "gid", "mode", "mtime", "path", "uid"),
	"symlink":         keySet("_type", "gid", "mode", "mtime", "path", "target", "uid"),
	"file": keySet("_type", "_kind", "chunk", "digest", "file_digest", "gid", "length",
		"mode", "mtime", "offset", "path", "relative_key", "size", "target", "uid"),
}

func keySet(keys ...string) map[string]bool {
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	return set
}

// stripOwnershipKeys removes exactly the "gid" and "uid" members the
// capturing producer records on every entry, plus one adjacent comma each,
// from the raw captured JSONL — and refuses everything it does not model. It
// is byte-level surgery on each line, never a decode-and-re-encode, which
// would launder the capture through the very encoder the capture exists to
// check. It is a pure function returning an error so its refusals are
// observable by the controls below; nothing in it touches testing.T.
func stripOwnershipKeys(raw []byte) ([]byte, error) {
	var out []byte
	for _, line := range bytes.SplitAfter(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			out = append(out, line...)
			continue
		}
		spans, err := ownershipSpans(line)
		if err != nil {
			return nil, err
		}
		kept := line
		for i := len(spans) - 1; i >= 0; i-- {
			kept = append(kept[:spans[i].start:spans[i].start], kept[spans[i].end:]...)
		}
		out = append(out, kept...)
	}
	return out, nil
}

type ownershipSpan struct {
	start, end int
}

// ownershipSpans walks one record's bytes — tracking strings, escapes, and
// object depth — returning the spans of the top-level "gid" and "uid"
// members, each widened by one adjacent comma. Every key on the line, at any
// depth, must be in the record type's modelled vocabulary; the transform is
// whitelist-checking, not merely surgical, so a key the producer adds
// tomorrow is a named refusal here rather than a byte-diff wall in the
// comparison that consumes the strip.
func ownershipSpans(line []byte) ([]ownershipSpan, error) {
	var spans []ownershipSpan
	seen := map[string]int{}
	recordType := ""
	depth := 0
	i := 0
	for i < len(line) {
		switch line[i] {
		case '"':
			keyStart := i
			key, next, err := scanJSONString(line, i)
			if err != nil {
				return nil, err
			}
			i = next
			if i >= len(line) || line[i] != ':' {
				continue // a string value, not a key
			}
			i++
			if recordType == "" {
				// The canonical form puts _type first on every record; the
				// whitelist is per record type, so it must be known before
				// any other key can be judged.
				if key != "_type" || i >= len(line) || line[i] != '"' {
					return nil, fmt.Errorf("strip cannot find a leading _type on the line: %s", line)
				}
				recordType, i, err = scanJSONString(line, i)
				if err != nil {
					return nil, err
				}
				if modelledCaptureKeys[recordType] == nil {
					return nil, fmt.Errorf("strip refuses unmodelled record type %q: %s", recordType, line)
				}
				continue
			}
			if !modelledCaptureKeys[recordType][key] {
				return nil, fmt.Errorf("strip refuses unmodelled key %q: the producer added a key this transform does not account for; decide about it, do not swallow it", key)
			}
			if key != "gid" && key != "uid" {
				continue
			}
			if depth != 1 {
				return nil, fmt.Errorf("strip refuses ownership key %q at nested depth %d: %s", key, depth, line)
			}
			seen[key]++
			if seen[key] > 1 {
				return nil, fmt.Errorf("strip refuses duplicate ownership key %q on one line: %s", key, line)
			}
			valStart := i
			for i < len(line) && line[i] >= '0' && line[i] <= '9' {
				i++
			}
			if i == valStart {
				return nil, fmt.Errorf("strip refuses non-numeric value for ownership key %q: %s", key, line)
			}
			start, end := keyStart, i
			switch {
			case start > 0 && line[start-1] == ',':
				start--
			case end < len(line) && line[end] == ',':
				end++
			default:
				return nil, fmt.Errorf("strip found ownership key %q with no adjacent comma: %s", key, line)
			}
			spans = append(spans, ownershipSpan{start: start, end: end})
		case '{':
			depth++
			i++
		case '}':
			depth--
			i++
		default:
			i++
		}
	}
	return spans, nil
}

// scanJSONString reads the string starting at the opening quote line[i],
// returning its contents and the index just past the closing quote.
func scanJSONString(line []byte, i int) (string, int, error) {
	start := i + 1
	j := start
	for j < len(line) {
		switch line[j] {
		case '\\':
			j += 2
		case '"':
			return string(line[start:j]), j + 1, nil
		default:
			j++
		}
	}
	return "", 0, fmt.Errorf("unterminated string in captured line: %s", line)
}

// mustStrip is the call-site shape every consuming test uses.
func mustStrip(t *testing.T, raw []byte) []byte {
	t.Helper()
	stripped, err := stripOwnershipKeys(raw)
	if err != nil {
		t.Fatal(err)
	}
	return stripped
}

// TestCaptureCarriesOwnershipOnEveryEntryLine is the coverage leg of the
// strip: today's capture records ownership on every entry line — exactly one
// gid and one uid each — and none on the header or provenance. If a future
// capture stops recording ownership, this goes red so the strip transform is
// REMOVED, not silently reduced to a no-op that guards nothing.
func TestCaptureCarriesOwnershipOnEveryEntryLine(t *testing.T) {
	raw := fixtureBytes(t, "manifest.jsonl")
	entries := 0
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		spans, err := ownershipSpans(line)
		if err != nil {
			t.Fatal(err)
		}
		isEntry := bytes.Contains(line, []byte(`"_type":"directory"`)) ||
			bytes.Contains(line, []byte(`"_type":"file"`)) ||
			bytes.Contains(line, []byte(`"_type":"symlink"`))
		if isEntry {
			entries++
			if len(spans) != 2 {
				t.Errorf("entry line carries %d ownership keys, want the gid and uid pair: %s", len(spans), line)
			}
		} else if len(spans) != 0 {
			t.Errorf("non-entry line carries ownership keys: %s", line)
		}
	}
	if entries == 0 {
		t.Fatal("no entry lines enumerated — the coverage leg measured nothing")
	}
}

// TestStripRefusesWhatItDoesNotModel pins the whitelist by planting the real
// future scenario: "xattrs" is one of the optional metadata keys the format
// owner's review names and this capture does not contain. Its arrival must be
// a named refusal at the transform — legible, decidable — never a silent pass
// into a byte-diff wall. The other rows pin the remaining refusals.
func TestStripRefusesWhatItDoesNotModel(t *testing.T) {
	rows := map[string]struct{ line, want string }{
		"future producer key": {
			`{"_type":"directory","gid":0,"mode":"0755","uid":0,"xattrs":{}}`,
			`strip refuses unmodelled key "xattrs": the producer added a key this transform does not account for; decide about it, do not swallow it`,
		},
		"nested ownership": {
			`{"_type":"file","_kind":"chunk","chunk":{"digest":"b3:aa","length":1,"offset":0,"uid":0},"mode":"0644","path":"f"}`,
			`at nested depth`,
		},
		"duplicate ownership": {
			`{"_type":"directory","gid":0,"gid":1,"mode":"0755"}`,
			`duplicate ownership key "gid"`,
		},
		"non-numeric ownership": {
			`{"_type":"directory","gid":"root","mode":"0755"}`,
			`non-numeric value for ownership key "gid"`,
		},
		"unmodelled record type": {
			`{"_type":"ownership","uid":0}`,
			`unmodelled record type "ownership"`,
		},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			_, err := stripOwnershipKeys([]byte(row.line + "\n"))
			if err == nil {
				t.Fatalf("the strip accepted what it does not model: %s", row.line)
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Fatalf("refusal does not name the problem\n got: %v\nwant a message containing: %s", err, row.want)
			}
		})
	}
}

// TestStripSurgeryIsExact pins the removal itself on a fully modelled line:
// exactly the two keys and one comma each, nothing else moved. A fuzzy strip
// — one swallowing a second comma, or trimming a neighbor — breaks the byte
// equality here by construction.
func TestStripSurgeryIsExact(t *testing.T) {
	line := `{"_type":"directory","gid":0,"mode":"0755","mtime":"2026-01-02T03:04:05Z","path":"a","uid":0}` + "\n"
	want := `{"_type":"directory","mode":"0755","mtime":"2026-01-02T03:04:05Z","path":"a"}` + "\n"
	got, err := stripOwnershipKeys([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("strip surgery is not exact\n got: %s\nwant: %s", got, want)
	}
}
