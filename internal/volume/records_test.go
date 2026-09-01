package volume

import (
	"strconv"
	"strings"
	"testing"

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
		`{"_type":"file","_kind":"chunk","chunk":{"digest":"` + chunkDigest.String() +
		`","length":3,"offset":0,"target":{"relative_key":"` + TargetForDigest(chunkDigest).RelativeKey +
		`"}},"mode":"0640","path":"z<file"}` + "\n" +
		`{"_type":"symlink","mode":"0777","path":"link","target":"z<file"}` + "\n"
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

// TestEncodeManifestSortsGroups checks that the bytes depend on content and
// not on the order a walk produced entries in.
func TestEncodeManifestSortsGroups(t *testing.T) {
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
		`","length":5,"offset":0,"target":{"relative_key":"k"}},"mode":"0644","path":"f"}` + "\n"
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
			itoa(length) + `,"offset":` + itoa(offset) + `,"target":{"relative_key":"k"}}` + "\n"
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
