package volume

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
)

// testDigest builds a recognizable digest whose every byte is value, so a
// golden string can name it by its repeated hex pair.
func testDigest(value byte) Digest {
	var d Digest
	for i := range d {
		d[i] = value
	}
	return d
}

func TestDigestParsing(t *testing.T) {
	d := testDigest(0xab)
	require.Equal(t, "b3:"+strings.Repeat("ab", 32), d.String())

	parsed, err := ParseDigest(d.String())
	require.NoError(t, err)
	require.Equal(t, d, parsed)

	for _, invalid := range []string{
		d.Hex(),                            // no algorithm prefix
		"sha256:" + d.Hex(),                // wrong algorithm
		"b3:short",                         // too short
		"b3:" + strings.Repeat("z", 64),    // not hex
		"b3:" + strings.Repeat("AB", 32),   // uppercase
		"b3:" + strings.Repeat("ab", 32-1), // one byte short
	} {
		t.Run(invalid, func(t *testing.T) {
			_, err := ParseDigest(invalid)
			require.Error(t, err)
		})
	}
}

func TestTargetForDigest(t *testing.T) {
	d := testDigest(0xab)
	require.Equal(t, "objects/b3/ab/ab/"+strings.Repeat("ab", 32), TargetForDigest(d).RelativeKey)

	var mixed Digest
	mixed[0], mixed[1] = 0x01, 0x9f
	require.Equal(t, "objects/b3/01/9f/"+mixed.Hex(), TargetForDigest(mixed).RelativeKey)
}

// TestEncodeChunkmapGolden pins the chunkmap wire bytes. The golden string is
// the canonical encoding of the same records.
func TestEncodeChunkmapGolden(t *testing.T) {
	first, second := testDigest(0x11), testDigest(0x22)
	got := EncodeChunkmap(&Chunkmap{
		FileSize: 5,
		Chunks: []ChunkRef{
			{Digest: first, Length: 3, Offset: 0, Target: TargetForDigest(first)},
			{Digest: second, Length: 2, Offset: 3, Target: TargetForDigest(second)},
		},
	})

	want := `{"_type":"chunkmap_header","chunk_count":2,"file_size":5}` + "\n" +
		`{"_type":"chunk","digest":"` + first.String() + `","length":3,"offset":0,"target":{"relative_key":"` +
		TargetForDigest(first).RelativeKey + `"}}` + "\n" +
		`{"_type":"chunk","digest":"` + second.String() + `","length":2,"offset":3,"target":{"relative_key":"` +
		TargetForDigest(second).RelativeKey + `"}}` + "\n"
	require.Equal(t, want, string(got))
}

// TestEncodeChunkmapOwnsNothing pins that encoding leaves the caller's
// chunks exactly as given — the same promise the manifest encoder makes.
// The emission is still offset-ordered; the caller's slice is not the
// scratch space that produces it.
func TestEncodeChunkmapOwnsNothing(t *testing.T) {
	first, second := testDigest(0x11), testDigest(0x22)
	shuffled := &Chunkmap{
		FileSize: 5,
		Chunks: []ChunkRef{
			{Digest: second, Length: 2, Offset: 3, Target: TargetForDigest(second)},
			{Digest: first, Length: 3, Offset: 0, Target: TargetForDigest(first)},
		},
	}
	encoded := EncodeChunkmap(shuffled)

	if shuffled.Chunks[0].Offset != 3 || shuffled.Chunks[1].Offset != 0 {
		t.Errorf("encoding reordered the caller's chunks: offsets %d, %d",
			shuffled.Chunks[0].Offset, shuffled.Chunks[1].Offset)
	}
	sorted := &Chunkmap{FileSize: 5, Chunks: []ChunkRef{shuffled.Chunks[1], shuffled.Chunks[0]}}
	require.Equal(t, string(EncodeChunkmap(sorted)), string(encoded))
}

// TestEncodeManifestGolden pins the manifest wire bytes across every record
// type a push emits. The "z<file" path is deliberate: an encoder that HTML
// escapes, as encoding/json does by default, would write < there and
// produce a manifest digest no other client agrees with.
func TestEncodeManifestGolden(t *testing.T) {
	chunkDigest, mapDigest := testDigest(0x11), testDigest(0x33)
	got := EncodeManifest(&Manifest{
		Provenance: Provenance{
			SourceFingerprint:     ProvenanceFingerprint,
			SourceFingerprintType: ProvenanceFingerprintType,
			SourceURI:             "local://fixture",
		},
		Directories: []DirectoryEntry{{Path: "a", Mode: 0o750}},
		Files: []FileEntry{
			{
				Path:  "z<file",
				Mode:  0o640,
				Kind:  FileKindChunk,
				Size:  3,
				Chunk: ChunkRef{Digest: chunkDigest, Length: 3, Target: TargetForDigest(chunkDigest)},
			},
			{
				Path:   "big",
				Mode:   0o644,
				Kind:   FileKindChunkmap,
				Size:   9,
				Digest: mapDigest,
				Target: TargetForDigest(mapDigest),
			},
		},
		Symlinks: []SymlinkEntry{{Path: "link", Target: "z<file", Mode: 0o777}},
	})

	want := `{"_type":"manifest_header","entry_count":4,"manifest_schema":"v1","total_size":12}` + "\n" +
		`{"_type":"provenance","source_fingerprint":"local","source_fingerprint_type":"local_push","source_uri":"local://fixture"}` + "\n" +
		`{"_type":"directory","mode":"0750","path":"a"}` + "\n" +
		`{"_type":"file","_kind":"chunkmap","digest":"` + mapDigest.String() + `","mode":"0644","path":"big","size":9,` +
		`"target":{"relative_key":"` + TargetForDigest(mapDigest).RelativeKey + `"}}` + "\n" +
		`{"_type":"symlink","mode":"0777","path":"link","target":"z<file"}` + "\n" +
		`{"_type":"file","_kind":"chunk","chunk":{"digest":"` + chunkDigest.String() +
		`","length":3,"offset":0,"target":{"relative_key":"` + TargetForDigest(chunkDigest).RelativeKey +
		`"}},"mode":"0640","path":"z<file"}` + "\n"
	require.Equal(t, want, string(got))
}

// TestEncodeManifestSlabmapGolden covers the one file kind push never writes.
// It is here so a slabmap manifest read from the server survives a decode and
// re-encode unchanged, which is what makes the round-trip test below total.
func TestEncodeManifestSlabmapGolden(t *testing.T) {
	slab, file := testDigest(0x44), testDigest(0x55)
	got := EncodeManifest(&Manifest{
		Provenance: Provenance{SourceFingerprint: "s", SourceFingerprintType: "t", SourceURI: "u"},
		Files: []FileEntry{{
			Path:       "packed",
			Mode:       0o600,
			Kind:       FileKindSlabmap,
			Size:       7,
			Digest:     slab,
			FileDigest: file,
			Target:     TargetForDigest(slab),
		}},
	})

	want := `{"_type":"manifest_header","entry_count":1,"manifest_schema":"v1","total_size":7}` + "\n" +
		`{"_type":"provenance","source_fingerprint":"s","source_fingerprint_type":"t","source_uri":"u"}` + "\n" +
		`{"_type":"file","_kind":"slabmap","digest":"` + slab.String() + `","file_digest":"` + file.String() +
		`","mode":"0600","path":"packed","size":7,"target":{"relative_key":"` + TargetForDigest(slab).RelativeKey + `"}}` + "\n"
	require.Equal(t, want, string(got))
}

// TestEncodeManifestOrderIndependent checks that the bytes depend on content
// and not on the order a walk produced entries in.
func TestEncodeManifestOrderIndependent(t *testing.T) {
	d := testDigest(0x11)
	build := func(dirs []DirectoryEntry, files []FileEntry, links []SymlinkEntry) string {
		return string(EncodeManifest(&Manifest{Directories: dirs, Files: files, Symlinks: links}))
	}
	file := func(path string) FileEntry {
		return FileEntry{Path: path, Mode: 0o644, Kind: FileKindChunk,
			Chunk: ChunkRef{Digest: d, Length: 1, Target: TargetForDigest(d)}}
	}
	sorted := build(
		[]DirectoryEntry{{Path: "a"}, {Path: "b"}},
		[]FileEntry{file("a/one"), file("b/two")},
		[]SymlinkEntry{{Path: "x", Target: "a"}, {Path: "y", Target: "b"}},
	)
	shuffled := build(
		[]DirectoryEntry{{Path: "b"}, {Path: "a"}},
		[]FileEntry{file("b/two"), file("a/one")},
		[]SymlinkEntry{{Path: "y", Target: "b"}, {Path: "x", Target: "a"}},
	)
	require.Equal(t, sorted, shuffled)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// TestPathComponentCompare pins the canonical order byte by byte. The pairs
// where it disagrees with plain bytewise comparison are the point: the
// separator sorts below '.' and '-', so a directory's children stay together
// instead of a similarly named sibling landing between them.
func TestPathComponentCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"a", "a", 0},
		{"a", "a/b", -1},      // a proper prefix sorts first
		{"a/b", "a/z", -1},    // within one directory, bytewise
		{"a/z", "a.txt", -1},  // '/' below '.': the subtree stays together
		{"a/b", "a-b", -1},    // '/' below '-' as well
		{"a/b/c", "a/c", -1},  // the deeper subtree comes first
		{"b", "a/z", 1},       // the first byte decides across trees
		{"a/c", "ab/c", -1},   // byte-mapped compare, not component-at-a-time
		{"a", "a/", -1},       // decoder leniency admits a trailing slash
		{"a/b", "a\x00b", -1}, // NUL stays above the separator: the +1 at work
	}
	for _, c := range cases {
		if got := sign(pathComponentCompare(c.a, c.b)); got != c.want {
			t.Errorf("pathComponentCompare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := sign(pathComponentCompare(c.b, c.a)); got != -c.want {
			t.Errorf("pathComponentCompare(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

// TestPathComponentCompareAgainstReference holds the comparator to its
// definition rather than to enumerated examples: for valid paths, canonical
// order is "split on the separator, compare the component lists
// lexicographically, a shorter prefix first" — which is exactly what
// slices.Compare does over the split. Every pair from the generated corpus
// must agree. The corpus stays within what ValidatePath admits; the edges it
// cannot reach (NUL, a trailing slash) are pinned by the table above.
func TestPathComponentCompareAgainstReference(t *testing.T) {
	segments := []string{"a", "b", "z", "ab", "a.txt", "x-y", "deep", "0", "file.bin", "aa"}
	var paths []string
	for _, first := range segments {
		paths = append(paths, first)
		for _, second := range segments {
			paths = append(paths, first+"/"+second)
			for _, third := range segments[:4] {
				paths = append(paths, first+"/"+second+"/"+third)
			}
		}
	}
	for _, a := range paths {
		for _, b := range paths {
			want := sign(slices.Compare(strings.Split(a, "/"), strings.Split(b, "/")))
			if got := sign(pathComponentCompare(a, b)); got != want {
				t.Fatalf("pathComponentCompare(%q, %q) = %d, the component reference says %d", a, b, got, want)
			}
		}
	}
}

// TestEncodeManifestInterleavesCanonically is the distinguishing golden. On
// this tree the canonical order — a, a/b, a/z, a.txt — differs from the
// former grouped emission and from a plain bytewise sort alike: either of
// those puts a.txt before the children of a, so an encoder that regresses to
// either fails here.
func TestEncodeManifestInterleavesCanonically(t *testing.T) {
	d := testDigest(0x11)
	entry := func(path string) FileEntry {
		return FileEntry{Path: path, Mode: 0o644, Kind: FileKindChunk, Size: 1,
			Chunk: ChunkRef{Digest: d, Length: 1, Target: TargetForDigest(d)}}
	}
	encoded := EncodeManifest(&Manifest{
		Provenance:  Provenance{SourceFingerprint: "s", SourceFingerprintType: "t", SourceURI: "u"},
		Directories: []DirectoryEntry{{Path: "a", Mode: 0o755}},
		Files:       []FileEntry{entry("a.txt"), entry("a/b")},
		Symlinks:    []SymlinkEntry{{Path: "a/z", Target: "a/b", Mode: 0o777}},
	})

	var paths []string
	for _, line := range strings.Split(strings.TrimSuffix(string(encoded), "\n"), "\n") {
		var record struct {
			Type string `json:"_type"`
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Type == "manifest_header" || record.Type == "provenance" {
			continue
		}
		paths = append(paths, record.Path)
	}
	want := []string{"a", "a/b", "a/z", "a.txt"}
	if !slices.Equal(paths, want) {
		t.Errorf("entries emitted as %q, want %q", paths, want)
	}
}

func TestJSONStringEscaping(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "hello", `"hello"`},
		{"quote", `a"b`, `"a\"b"`},
		{"backslash", `a\b`, `"a\\b"`},
		{"newline", "a\nb", `"a\nb"`},
		{"tab", "a\tb", `"a\tb"`},
		{"carriage return", "a\rb", `"a\rb"`},
		{"backspace", "a\bb", `"a\bb"`},
		{"form feed", "a\fb", `"a\fb"`},
		{"other control", "a\x01b", `"a\u0001b"`},
		// The canonical encoding leaves these alone; encoding/json escapes the
		// HTML set by default and the line separators unconditionally.
		{"html", "<&>", `"<&>"`},
		{"line separator", "a\u2028b", "\"a\u2028b\""},
		{"paragraph separator", "a\u2029b", "\"a\u2029b\""},
		{"non-ascii", "héllo", `"héllo"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, string(appendJSONString(nil, tc.in)))
		})
	}
}

func TestModeEncoding(t *testing.T) {
	tests := []struct {
		mode uint16
		want string
	}{
		{0o644, `"mode":"0644"`},
		{0o755, `"mode":"0755"`},
		{0o777, `"mode":"0777"`},
		{0o7, `"mode":"0007"`},
		{0, `"mode":"0000"`},
		// setuid, setgid, and sticky are inside the recorded mask.
		{0o4755, `"mode":"4755"`},
		{0o7777, `"mode":"7777"`},
	}
	for _, tc := range tests {
		// Every field helper writes its own leading comma; a record's opening
		// brace is followed by the _type discriminator, never by a mode.
		require.Equal(t, ","+tc.want, string(appendMode(nil, tc.mode)))

		parsed, err := parseMode(strings.Trim(strings.TrimPrefix(tc.want, `"mode":`), `"`))
		require.NoError(t, err)
		require.Equal(t, tc.mode, parsed)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	chunk, chunkmap, slab, file := testDigest(0x11), testDigest(0x22), testDigest(0x33), testDigest(0x44)
	want := &Manifest{
		Provenance: Provenance{
			SourceFingerprint:     ProvenanceFingerprint,
			SourceFingerprintType: ProvenanceFingerprintType,
			SourceURI:             "file:///tmp/tree",
		},
		Directories: []DirectoryEntry{{Path: "dir", Mode: 0o755}, {Path: "ro", Mode: 0o555}},
		Files: []FileEntry{
			{Path: "dir/small", Mode: 0o644, Kind: FileKindChunk, Size: 4,
				Chunk: ChunkRef{Digest: chunk, Length: 4, Target: TargetForDigest(chunk)}},
			{Path: "dir/empty", Mode: 0o600, Kind: FileKindChunk,
				Chunk: ChunkRef{Digest: chunk, Target: TargetForDigest(chunk)}},
			{Path: "dir/large", Mode: 0o755, Kind: FileKindChunkmap, Size: 99,
				Digest: chunkmap, Target: TargetForDigest(chunkmap)},
			{Path: "dir/packed", Mode: 0o644, Kind: FileKindSlabmap, Size: 7,
				Digest: slab, FileDigest: file, Target: TargetForDigest(slab)},
		},
		Symlinks: []SymlinkEntry{{Path: "dir/link", Target: "../elsewhere", Mode: 0o777}},
	}
	encoded := EncodeManifest(want)

	got, err := DecodeManifest(encoded)
	require.NoError(t, err)
	require.Equal(t, string(encoded), string(EncodeManifest(got)))
	require.Equal(t, want.Provenance, got.Provenance)
	require.Len(t, got.Directories, 2)
	require.Len(t, got.Files, 4)
	require.Len(t, got.Symlinks, 1)
	require.Equal(t, uint64(110), got.TotalSize())
	require.Equal(t, uint64(7), got.EntryCount())
}

func TestChunkmapRoundTrip(t *testing.T) {
	first, second := testDigest(0x11), testDigest(0x22)
	want := &Chunkmap{FileSize: 5, Chunks: []ChunkRef{
		{Digest: first, Length: 3, Offset: 0, Target: TargetForDigest(first)},
		{Digest: second, Length: 2, Offset: 3, Target: TargetForDigest(second)},
	}}
	encoded := EncodeChunkmap(want)

	got, err := DecodeChunkmap(encoded)
	require.NoError(t, err)
	require.Equal(t, string(encoded), string(EncodeChunkmap(got)))
	require.Equal(t, want.FileSize, got.FileSize)
	require.Len(t, got.Chunks, 2)
	require.Equal(t, want.Chunks[1], got.Chunks[1])
}

// TestDecodeIgnoresUnknownFields pins the format's forward compatibility
// contract: a newer server may add keys and record types, and this client must
// read past them rather than fail.
func TestDecodeIgnoresUnknownFields(t *testing.T) {
	d := testDigest(0xaa)
	body := `{"_type":"manifest_header","entry_count":2,"manifest_schema":"v1","total_size":100,"future":1}` + "\n" +
		`{"_type":"provenance","source_fingerprint":"local","source_fingerprint_type":"local_push","source_uri":"u"}` + "\n" +
		`{"_type":"directory","mode":"0755","path":"d","future_field":"hello","another":42}` + "\n" +
		// A record type this client has never heard of.
		`{"_type":"prefix_provenance","prefix":"d/","source_uri":"u"}` + "\n" +
		// The wire format accepts _kind ahead of _type on the way in, even
		// though writers only ever emit the other order.
		`{"_kind":"chunkmap","_type":"file","digest":"` + d.String() + `","mode":"0644","new_field":"v2",` +
		`"path":"d/f","size":100,"target":{"relative_key":"objects/b3/aa/aa/x"}}` + "\n"

	m, err := DecodeManifest([]byte(body))
	require.NoError(t, err)
	require.Len(t, m.Directories, 1)
	require.Len(t, m.Files, 1)
	require.Equal(t, "d/f", m.Files[0].Path)
	require.Equal(t, d, m.Files[0].Digest)
	require.Equal(t, uint64(100), m.Files[0].Size)
	require.Equal(t, "u", m.Provenance.SourceURI)
}

// TestEncodeFileRecordRejectsAnUnsettledKind covers a state that cannot be
// reached by construction but would be silent if it were: the record would
// lose every key after the discriminators, and those bytes become a digest.
func TestEncodeFileRecordRejectsAnUnsettledKind(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("encoding a file entry with no kind should not have succeeded")
		}
	}()
	EncodeManifest(&Manifest{Files: []FileEntry{{Path: "f", Mode: 0o644}}})
}

func TestDecodeManifestRejectsCorruption(t *testing.T) {
	d := testDigest(0xaa)
	header := `{"_type":"manifest_header","entry_count":1,"manifest_schema":"v1","total_size":5}` + "\n"
	fileRecord := `{"_type":"file","_kind":"chunk","chunk":{"digest":"` + d.String() +
		`","length":5,"offset":0,"target":{"relative_key":"objects/b3/aa/bb/stub"}},"mode":"0644","path":"f"}` + "\n"
	provenance := `{"_type":"provenance","source_fingerprint":"local",` +
		`"source_fingerprint_type":"local_push","source_uri":"file:///a"}` + "\n"

	tests := map[string]string{
		// Two provenance records mean the manifest disagrees with itself about
		// where it came from, and taking either would be a guess.
		"two provenances":      header + provenance + provenance + fileRecord,
		"no header":            fileRecord,
		"two headers":          header + header + fileRecord,
		"entry count off":      strings.Replace(header, `"entry_count":1`, `"entry_count":2`, 1) + fileRecord,
		"total size off":       strings.Replace(header, `"total_size":5`, `"total_size":6`, 1) + fileRecord,
		"bad digest":           header + strings.Replace(fileRecord, d.String(), "sha256:"+d.Hex(), 1),
		"bad mode":             header + strings.Replace(fileRecord, `"mode":"0644"`, `"mode":"nope"`, 1),
		"unknown kind":         header + strings.Replace(fileRecord, `"_kind":"chunk"`, `"_kind":"tarball"`, 1),
		"chunk kind, no chunk": header + `{"_type":"file","_kind":"chunk","mode":"0644","path":"f"}` + "\n",
		"not json":             header + "{oops\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeManifest([]byte(body))
			require.Error(t, err)
		})
	}

	// One provenance record is the normal case, and none is tolerated:
	// provenance describes where the manifest came from, not what is in it, so
	// a manifest without it still materializes correctly.
	_, err := DecodeManifest([]byte(header + provenance + fileRecord))
	require.NoError(t, err)
	_, err = DecodeManifest([]byte(header + fileRecord))
	require.NoError(t, err)
}

func TestDecodeChunkmapRejectsCorruption(t *testing.T) {
	first, second := testDigest(0x11), testDigest(0x22)
	chunk := func(d Digest, length, offset uint64) string {
		return `{"_type":"chunk","digest":"` + d.String() + `","length":` +
			itoa(length) + `,"offset":` + itoa(offset) + `,"target":{"relative_key":"objects/b3/aa/bb/stub"}}` + "\n"
	}
	header := func(count, size uint64) string {
		return `{"_type":"chunkmap_header","chunk_count":` + itoa(count) + `,"file_size":` + itoa(size) + `}` + "\n"
	}

	tests := map[string]string{
		"no header":              chunk(first, 3, 0),
		"count off":              header(3, 5) + chunk(first, 3, 0) + chunk(second, 2, 3),
		"size off":               header(2, 6) + chunk(first, 3, 0) + chunk(second, 2, 3),
		"gap":                    header(2, 6) + chunk(first, 3, 0) + chunk(second, 2, 4),
		"overlap":                header(2, 4) + chunk(first, 3, 0) + chunk(second, 2, 2),
		"does not start at zero": header(1, 3) + chunk(first, 3, 1),
		"zero length":            header(1, 0) + chunk(first, 0, 0),
		"no chunks":              header(0, 0),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeChunkmap([]byte(body))
			require.Error(t, err)
		})
	}

	valid := header(2, 5) + chunk(first, 3, 0) + chunk(second, 2, 3)
	_, err := DecodeChunkmap([]byte(valid))
	require.NoError(t, err)
	// A missing trailing newline is tolerated on the way in, even though every
	// encode writes one.
	_, err = DecodeChunkmap([]byte(strings.TrimSuffix(valid, "\n")))
	require.NoError(t, err)
}

func itoa(v uint64) string {
	return strconv.FormatUint(v, 10)
}

// TestAppendModeMasksToTheRecordedBits pins the encode-side narrowing. The
// mode is inside the digest, so a caller handing in a Go file mode's type
// bits must not produce a manifest nobody else can reproduce for the tree.
func TestAppendModeMasksToTheRecordedBits(t *testing.T) {
	// 0o40755 is a directory's mode with its type bit set — what a caller
	// gets from fs.FileMode if it forgets to narrow.
	withTypeBits := EncodeManifest(&Manifest{
		Directories: []DirectoryEntry{{Path: "d", Mode: 0o40755}},
	})
	narrowed := EncodeManifest(&Manifest{
		Directories: []DirectoryEntry{{Path: "d", Mode: 0o755}},
	})
	require.Equal(t, string(narrowed), string(withTypeBits))
	require.True(t, strings.Contains(string(withTypeBits), `"mode":"0755"`),
		"encoded mode: %s", withTypeBits)

	// The setuid, setgid and sticky bits are inside the mask and must survive.
	kept := EncodeManifest(&Manifest{
		Directories: []DirectoryEntry{{Path: "d", Mode: 0o7755}},
	})
	require.True(t, strings.Contains(string(kept), `"mode":"7755"`),
		"encoded mode: %s", kept)
}

// TestParseModeRefusesAboveTheMask covers the decode side, which does the
// opposite of the encoder: it refuses rather than narrows, because masking
// would silently alter what was received and leaving it would let a mode the
// format forbids reach code that assumes it is already narrowed.
func TestParseModeRefusesAboveTheMask(t *testing.T) {
	for _, s := range []string{"40755", "10000", "177777"} {
		_, err := parseMode(s)
		require.Error(t, err)
	}
	// Everything inside the mask still decodes, including the three bits
	// above the permission bits.
	for _, s := range []string{"0644", "7777", "0"} {
		mode, err := parseMode(s)
		require.NoError(t, err)
		require.True(t, mode <= ModeMask, "mode %q decoded to %o", s, mode)
	}
}

// TestDecodeNormalizesPreRulePaths covers manifests published before the
// containment rule, whose entry paths can be root-anchored. Readers normalize
// them — strip the leading slashes — rather than refuse the volume, and they
// do it at decode, the one point every consumer sits behind: the containment
// walk, the link namespace, and materialization all see the same normalized
// form. The manifest records what it normalized, verbatim, for the pull path
// to surface once the reporting shape is settled. Push stays strict: a scan
// never produces a root-anchored path and validation still refuses one.
func TestDecodeNormalizesPreRulePaths(t *testing.T) {
	d := testDigest(0xaa)
	body := `{"_type":"manifest_header","entry_count":4,"manifest_schema":"v1","total_size":5}` + "\n" +
		`{"_type":"directory","mode":"0755","path":"/a"}` + "\n" +
		`{"_type":"directory","mode":"0755","path":"///deep"}` + "\n" +
		`{"_type":"file","_kind":"chunk","chunk":{"digest":"` + d.String() +
		`","length":5,"offset":0,"target":{"relative_key":"objects/b3/aa/bb/stub"}},"mode":"0644","path":"/a/b"}` + "\n" +
		`{"_type":"symlink","mode":"0777","path":"/l","target":"a/b"}` + "\n"

	m, err := DecodeManifest([]byte(body))
	require.NoError(t, err)
	require.Equal(t, "a", m.Directories[0].Path)
	require.Equal(t, "deep", m.Directories[1].Path)
	require.Equal(t, "a/b", m.Files[0].Path)
	require.Equal(t, "l", m.Symlinks[0].Path)
	require.Len(t, m.NormalizedPaths, 4)
	for i, want := range []NormalizedPath{
		{Raw: "/a", Path: "a"}, {Raw: "///deep", Path: "deep"},
		{Raw: "/a/b", Path: "a/b"}, {Raw: "/l", Path: "l"},
	} {
		require.Equal(t, want, m.NormalizedPaths[i])
	}

	// The single normalization point is what makes this pass: containment and
	// the link namespace validate the normalized paths and resolve the link
	// against them. Before normalization the same manifest failed here. Each
	// normalized path is reported as a typed finding — the warning channel's
	// charter is findings that did not stop the download, and a silently
	// rewritten path would be the one silent mutation in it.
	warnings, err := CheckManifestContainment(m)
	require.NoError(t, err)
	require.Len(t, warnings, 4)
	for i, want := range []string{"a", "deep", "a/b", "l"} {
		require.Equal(t, WarningPathNormalized, warnings[i].Kind)
		require.Equal(t, want, warnings[i].Path)
		require.Contains(t, warnings[i].Detail, "root-anchored")
	}

	// The push side is unchanged: a root-anchored path is still refused.
	require.Error(t, ValidatePath("/a/b"))
}

// TestValidateObjectTarget mirrors, check for check, what the service
// requires of a relative_key before it will build a storage key from one.
// The dot-dot rule is a substring match rather than component-wise — the
// service refuses any occurrence — so a key like "aa..bb" is refused here
// too, exactly as it would be there.
func TestValidateObjectTarget(t *testing.T) {
	require.NoError(t, ValidateObjectTarget(Target{RelativeKey: "objects/b3/aa/bb/abc"}))

	hostile := map[string]struct{ key, want string }{
		"empty":            {"", "is empty"},
		"leading slash":    {"/objects/b3/aa/bb/x", "anchored at the root"},
		"dot-dot escape":   {"objects/b3/../../../etc/creds", `contains ".."`},
		"embedded dot-dot": {"objects/b3/aa..bb/x", `contains ".."`},
		"nul byte":         {"objects/b3/aa/bb/x\x00y", "NUL byte"},
		"wrong prefix":     {"stuff/x", "objects/b3/"},
		// versions/ keys are real service keys, but they are mutable state:
		// a manifest record must name content-addressed bytes, never a key
		// whose contents can change under the digest.
		"mutable versions key": {"versions/v1/head", "objects/b3/"},
	}
	for name, tc := range hostile {
		t.Run(name, func(t *testing.T) {
			err := ValidateObjectTarget(Target{RelativeKey: tc.key})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestDecodeRefusesHostileObjectTargets drives one hostile key through each
// decode intake — a manifest's inline chunk, a manifest's chunkmap-kind file
// record, and a chunkmap's chunk record — so no intake accepts what the
// validator refuses. The digest check and lease scoping would contain the
// damage, but a key aimed outside the namespace is refused rather than
// fetched from.
func TestDecodeRefusesHostileObjectTargets(t *testing.T) {
	d := testDigest(0xaa)
	hostileTarget := `{"relative_key":"objects/b3/../../creds"}`

	t.Run("manifest inline chunk", func(t *testing.T) {
		body := `{"_type":"manifest_header","entry_count":1,"manifest_schema":"v1","total_size":5}` + "\n" +
			`{"_type":"file","_kind":"chunk","chunk":{"digest":"` + d.String() +
			`","length":5,"offset":0,"target":` + hostileTarget + `},"mode":"0644","path":"f"}` + "\n"
		_, err := DecodeManifest([]byte(body))
		require.Error(t, err)
		require.Contains(t, err.Error(), `contains ".."`)
	})

	t.Run("manifest chunkmap file", func(t *testing.T) {
		body := `{"_type":"manifest_header","entry_count":1,"manifest_schema":"v1","total_size":9}` + "\n" +
			`{"_type":"file","_kind":"chunkmap","digest":"` + d.String() +
			`","mode":"0644","path":"f","size":9,"target":` + hostileTarget + `}` + "\n"
		_, err := DecodeManifest([]byte(body))
		require.Error(t, err)
		require.Contains(t, err.Error(), `contains ".."`)
	})

	t.Run("chunkmap chunk", func(t *testing.T) {
		body := `{"_type":"chunkmap_header","chunk_count":1,"file_size":5}` + "\n" +
			`{"_type":"chunk","digest":"` + d.String() +
			`","length":5,"offset":0,"target":` + hostileTarget + `}` + "\n"
		_, err := DecodeChunkmap([]byte(body))
		require.Error(t, err)
		require.Contains(t, err.Error(), `contains ".."`)
	})
}

// TestClampMTime pins the wire-range clamp. The floor is year zero — below
// the zero time's year one — so no clamped value can collide with the
// zero-means-omit sentinel; the cap is the last representable instant of the
// four-digit-year range.
func TestClampMTime(t *testing.T) {
	require.True(t, clampMTime(time.Time{}).IsZero(), "the zero time must stay the omit sentinel")
	in := time.Date(2024, 5, 6, 7, 8, 9, 123, time.UTC)
	require.True(t, clampMTime(in).Equal(in), "an in-range time must pass through untouched")
	require.True(t, clampMTime(time.Date(99999, 1, 1, 0, 0, 0, 0, time.UTC)).Equal(mtimeCap),
		"a far-future time must clamp to the cap")
	require.True(t, clampMTime(time.Date(-5, 1, 1, 0, 0, 0, 0, time.UTC)).Equal(mtimeFloor),
		"a pre-range time must clamp to the floor")
	require.False(t, mtimeFloor.IsZero(), "the floor must never read as the omit sentinel")
}

// TestEncodeManifestMTimeGolden pins the wire form of mtime with the three
// truncation witnesses: the format is not fixed-width — trailing zeros of
// the fraction truncate, so half a second is ".5", never ".500000000" — and
// the bytes are digest-covered, so the truncation behavior itself is pinned.
// The struct-built goldens above stay mtime-free on purpose: no scan
// produces a zero mtime, so those manifests are what keeps the omit branch
// tested.
func TestEncodeManifestMTimeGolden(t *testing.T) {
	chunk := testDigest(0xaa)
	wholeSecond := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	halfSecond := time.Date(2024, 5, 6, 7, 8, 9, 500000000, time.UTC)
	irregular := time.Date(2024, 5, 6, 7, 8, 9, 123456789, time.UTC)

	m := &Manifest{
		Provenance: Provenance{
			SourceFingerprint:     ProvenanceFingerprint,
			SourceFingerprintType: ProvenanceFingerprintType,
			SourceURI:             "file:///tmp/tree",
		},
		Directories: []DirectoryEntry{{Path: "dir", Mode: 0o755, MTime: wholeSecond}},
		Files: []FileEntry{{Path: "dir/f", Mode: 0o644, Kind: FileKindChunk, Size: 4, MTime: halfSecond,
			Chunk: ChunkRef{Digest: chunk, Length: 4, Target: TargetForDigest(chunk)}}},
		Symlinks: []SymlinkEntry{{Path: "dir/l", Target: "f", Mode: SymlinkMode, MTime: irregular}},
	}

	want := `{"_type":"manifest_header","entry_count":3,"manifest_schema":"v1","total_size":4}` + "\n" +
		`{"_type":"provenance","source_fingerprint":"local","source_fingerprint_type":"local_push","source_uri":"file:///tmp/tree"}` + "\n" +
		`{"_type":"directory","mode":"0755","mtime":"2024-05-06T07:08:09Z","path":"dir"}` + "\n" +
		`{"_type":"file","_kind":"chunk","chunk":{"digest":"` + chunk.String() +
		`","length":4,"offset":0,"target":{"relative_key":"` + TargetForDigest(chunk).RelativeKey +
		`"}},"mode":"0644","mtime":"2024-05-06T07:08:09.5Z","path":"dir/f"}` + "\n" +
		`{"_type":"symlink","mode":"0777","mtime":"2024-05-06T07:08:09.123456789Z","path":"dir/l","target":"f"}` + "\n"
	require.Equal(t, want, string(EncodeManifest(m)))

	// The round trip preserves every recorded time exactly.
	decoded, err := DecodeManifest(EncodeManifest(m))
	require.NoError(t, err)
	require.True(t, decoded.Directories[0].MTime.Equal(wholeSecond), "directory mtime did not survive")
	require.True(t, decoded.Files[0].MTime.Equal(halfSecond), "file mtime did not survive")
	require.True(t, decoded.Symlinks[0].MTime.Equal(irregular), "symlink mtime did not survive")
}

// TestDecodeMTimeMixedAndMalformed covers the two decoder rules: a document
// may carry mtime on some entries and not others — manifests written before
// the key existed omit it everywhere, and entries with unknown times omit it
// individually — while a key that is present and malformed is refused like
// every other checked field.
func TestDecodeMTimeMixedAndMalformed(t *testing.T) {
	d := testDigest(0xaa)

	mixed := `{"_type":"manifest_header","entry_count":2,"manifest_schema":"v1","total_size":5}` + "\n" +
		`{"_type":"directory","mode":"0755","mtime":"2024-05-06T07:08:09Z","path":"dir"}` + "\n" +
		`{"_type":"file","_kind":"chunk","chunk":{"digest":"` + d.String() +
		`","length":5,"offset":0,"target":{"relative_key":"objects/b3/aa/bb/stub"}},"mode":"0644","path":"dir/f"}` + "\n"
	m, err := DecodeManifest([]byte(mixed))
	require.NoError(t, err)
	require.False(t, m.Directories[0].MTime.IsZero(), "the present mtime was dropped")
	require.True(t, m.Files[0].MTime.IsZero(), "the absent mtime was invented")

	malformed := map[string]string{
		"not a time":      "whenever",
		"wrong shape":     "2024-05-06 07:08:09",
		"five-digit year": "10000-01-01T00:00:00Z",
	}
	for name, mt := range malformed {
		t.Run(name, func(t *testing.T) {
			body := `{"_type":"manifest_header","entry_count":1,"manifest_schema":"v1","total_size":0}` + "\n" +
				`{"_type":"directory","mode":"0755","mtime":"` + mt + `","path":"dir"}` + "\n"
			_, err := DecodeManifest([]byte(body))
			require.Error(t, err)
			require.Contains(t, err.Error(), "mtime")
		})
	}
}

// TestMTimeClampKeepsTheWireParseable is the pipeline form of the clamp: the
// formatter never errors, so an out-of-range time would silently become
// digest-covered bytes the parser refuses. What the scanner clamps must
// encode to bytes the decoder accepts, at both ends of the range.
func TestMTimeClampKeepsTheWireParseable(t *testing.T) {
	for name, hostile := range map[string]time.Time{
		"far future": time.Date(99999, 1, 1, 0, 0, 0, 0, time.UTC),
		"pre range":  time.Date(-5, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		t.Run(name, func(t *testing.T) {
			m := &Manifest{Directories: []DirectoryEntry{{Path: "d", Mode: 0o755, MTime: clampMTime(hostile)}}}
			decoded, err := DecodeManifest(EncodeManifest(m))
			require.NoError(t, err)
			require.False(t, decoded.Directories[0].MTime.IsZero(), "the clamped time must still be recorded, not omitted")
		})
	}
}

// TestEncoderClampsWithoutAScan pins the encoder-side clamp: a manifest built
// by hand — no scan, no filesystem — reaches the encoder with whatever time
// its builder set, and the bytes must still be readable by our own decoder.
// The scan-site clamps cannot see this path. Idempotence makes the double
// application safe: clamping a clamped time changes nothing.
func TestEncoderClampsWithoutAScan(t *testing.T) {
	m := &Manifest{Directories: []DirectoryEntry{
		{Path: "future", Mode: 0o755, MTime: time.Date(99999, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Path: "past", Mode: 0o755, MTime: time.Date(-5, 1, 1, 0, 0, 0, 0, time.UTC)},
	}}
	decoded, err := DecodeManifest(EncodeManifest(m))
	require.NoError(t, err)
	require.True(t, decoded.Directories[0].MTime.Equal(mtimeCap), "the far-future time must land on the cap")
	require.True(t, decoded.Directories[1].MTime.Equal(mtimeFloor), "the pre-range time must land on the floor")

	require.True(t, clampMTime(clampMTime(time.Date(99999, 1, 1, 0, 0, 0, 0, time.UTC))).Equal(mtimeCap),
		"clampMTime must be idempotent, or the double application could drift")
}
