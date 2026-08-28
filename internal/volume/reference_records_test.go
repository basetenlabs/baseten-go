package volume

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func testDigest(value byte) Digest {
	var digest Digest
	for index := range digest {
		digest[index] = value
	}
	return digest
}

func TestDigestAndTargetStrictness(t *testing.T) {
	digest := testDigest(0xab)
	parsed, err := ParseDigest(digest.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != digest {
		t.Fatalf("parsed digest = %v, want %v", parsed, digest)
	}
	for _, invalid := range []string{
		digest.Hex(),
		"sha256:" + digest.Hex(),
		"b3:short",
		"b3:" + strings.Repeat("z", 64),
	} {
		if _, err := ParseDigest(invalid); err == nil {
			t.Errorf("ParseDigest(%q) unexpectedly succeeded", invalid)
		}
	}
	target := targetForDigest(digest)
	want := "objects/b3/ab/ab/" + strings.Repeat("ab", 32)
	if target.RelativeKey != want {
		t.Fatalf("target = %q, want %q", target.RelativeKey, want)
	}
}

func TestCanonicalJSONLMatchesReferenceEncoderGolden(t *testing.T) {
	first := testDigest(0x11)
	second := testDigest(0x22)
	chunks := []chunkEntry{
		{Digest: first, Length: 3, Offset: 0, Target: targetForDigest(first)},
		{Digest: second, Length: 2, Offset: 3, Target: targetForDigest(second)},
	}
	gotChunkmap := string(encodeChunkmap(5, chunks))
	wantChunkmap :=
		`{"_type":"chunkmap_header","chunk_count":2,"file_size":5}` + "\n" +
			`{"_type":"chunk","digest":"` + first.String() + `","length":3,"offset":0,"target":{"relative_key":"` +
			targetForDigest(first).RelativeKey + `"}}` + "\n" +
			`{"_type":"chunk","digest":"` + second.String() + `","length":2,"offset":3,"target":{"relative_key":"` +
			targetForDigest(second).RelativeKey + `"}}` + "\n"
	if gotChunkmap != wantChunkmap {
		t.Fatalf("chunkmap mismatch\ngot:  %s\nwant: %s", gotChunkmap, wantChunkmap)
	}

	gotManifest := string(encodeManifest(
		3,
		[]directoryEntry{{Mode: 0o750, Path: "a"}},
		[]manifestFile{{
			Kind: fileKindChunk,
			Chunk: chunkEntry{
				Digest: first, Length: 3, Target: targetForDigest(first),
			},
			Mode: 0o640,
			Path: "z<file",
			Size: 3,
		}},
		[]symlinkEntry{{Mode: 0o777, Path: "link", Target: "z<file"}},
		"local://fixture",
	))
	wantManifest :=
		`{"_type":"manifest_header","entry_count":3,"manifest_schema":"v1","total_size":3}` + "\n" +
			`{"_type":"provenance","source_fingerprint":"local","source_fingerprint_type":"local_push","source_uri":"local://fixture"}` + "\n" +
			`{"_type":"directory","mode":"0750","path":"a"}` + "\n" +
			`{"_type":"file","_kind":"chunk","chunk":{"digest":"` + first.String() +
			`","length":3,"offset":0,"target":{"relative_key":"` + targetForDigest(first).RelativeKey +
			`"}},"mode":"0640","path":"z<file"}` + "\n" +
			`{"_type":"symlink","mode":"0777","path":"link","target":"z<file"}` + "\n"
	if gotManifest != wantManifest {
		t.Fatalf("manifest mismatch\ngot:  %s\nwant: %s", gotManifest, wantManifest)
	}
}

func TestManifestSymlinkGraphRejectsComposedEscape(t *testing.T) {
	manifest := validatedManifest{
		Directories: []directoryEntry{{Mode: 0o755, Path: "dir"}},
		Symlinks: []symlinkEntry{
			{Mode: 0o777, Path: "dir/jump", Target: "../outside"},
			{Mode: 0o777, Path: "escape", Target: "dir/jump/../.."},
		},
	}
	if err := validateManifestStructure(
		manifest,
		10,
		defaultPortablePathLimits(),
	); err == nil || !IsCode(err, ErrorProtocol) {
		t.Fatalf("composed symlink escape error = %v, want %s", err, ErrorProtocol)
	}
	body := encodeManifest(
		0,
		manifest.Directories,
		nil,
		manifest.Symlinks,
		"local://fixture",
	)
	if _, err := decodeManifest(body, uint64(len(body)), 10); err == nil ||
		!IsCode(err, ErrorProtocol) {
		t.Fatalf("decoded composed symlink escape error = %v, want %s", err, ErrorProtocol)
	}
}

func TestManifestSymlinkGraphRejectsCyclesDepthAndTypeConflicts(t *testing.T) {
	t.Run("cycle", func(t *testing.T) {
		manifest := validatedManifest{Symlinks: []symlinkEntry{
			{Mode: 0o777, Path: "a", Target: "b"},
			{Mode: 0o777, Path: "b", Target: "a"},
		}}
		if err := validateManifestStructure(
			manifest,
			10,
			defaultPortablePathLimits(),
		); err == nil || !IsCode(err, ErrorProtocol) {
			t.Fatalf("cycle error = %v, want %s", err, ErrorProtocol)
		}
	})

	t.Run("bounded repeated traversal", func(t *testing.T) {
		manifest := validatedManifest{
			Directories: []directoryEntry{{Mode: 0o755, Path: "directory"}},
			Symlinks: []symlinkEntry{
				{Mode: 0o777, Path: "a", Target: "b/../b"},
				{Mode: 0o777, Path: "b", Target: "directory"},
			},
		}
		if err := validateManifestStructure(
			manifest,
			10,
			defaultPortablePathLimits(),
		); err != nil {
			t.Fatalf("bounded repeated symlink traversal error = %v", err)
		}
	})

	t.Run("deep expansion", func(t *testing.T) {
		manifest := validatedManifest{Symlinks: []symlinkEntry{
			{Mode: 0o777, Path: "a", Target: "b"},
			{Mode: 0o777, Path: "b", Target: "c"},
			{Mode: 0o777, Path: "c", Target: "d"},
			{Mode: 0o777, Path: "d", Target: "e"},
			{Mode: 0o777, Path: "e", Target: "missing"},
		}}
		limits := portablePathLimits{maxPathBytes: 64, maxPathComponents: 4}
		if err := validateManifestStructure(manifest, 10, limits); err == nil ||
			!IsCode(err, ErrorPreconditionFailed) {
			t.Fatalf("deep expansion error = %v, want %s", err, ErrorPreconditionFailed)
		}
	})

	t.Run("file as directory component", func(t *testing.T) {
		manifest := validatedManifest{
			Files: []manifestFile{{Path: "file"}},
			Symlinks: []symlinkEntry{{
				Mode: 0o777, Path: "link", Target: "file/child",
			}},
		}
		if err := validateManifestStructure(
			manifest,
			10,
			defaultPortablePathLimits(),
		); err == nil || !IsCode(err, ErrorProtocol) {
			t.Fatalf("component conflict error = %v, want %s", err, ErrorProtocol)
		}
	})

	t.Run("absolute target", func(t *testing.T) {
		manifest := validatedManifest{Symlinks: []symlinkEntry{{
			Mode: 0o777, Path: "link", Target: "/outside",
		}}}
		if err := validateManifestStructure(
			manifest,
			10,
			defaultPortablePathLimits(),
		); err == nil || !IsCode(err, ErrorProtocol) {
			t.Fatalf("absolute target error = %v, want %s", err, ErrorProtocol)
		}
	})
}

func TestManifestSymlinkResolverMemoizesExpansion(t *testing.T) {
	const symlinkCount = maximumPortablePathComponents
	limits := defaultPortablePathLimits()
	index, err := newManifestPathIndex(symlinkCount, limits)
	if err != nil {
		t.Fatal(err)
	}
	for position := range symlinkCount {
		path := fmt.Sprintf("link-%03d", position)
		target := "missing"
		if position+1 < symlinkCount {
			target = fmt.Sprintf("link-%03d", position+1)
		}
		if err := index.insert(path, manifestPathSymlink, target); err != nil {
			t.Fatal(err)
		}
	}
	resolver := &manifestSymlinkResolver{
		index:    index,
		states:   make(map[*manifestPathNode]symlinkResolveState, symlinkCount),
		resolved: make(map[*manifestPathNode]symlinkResolution, symlinkCount),
	}
	if err := resolver.validate(); err != nil {
		t.Fatal(err)
	}
	if resolver.expansions != symlinkCount {
		t.Fatalf("symlink expansions = %d, want %d", resolver.expansions, symlinkCount)
	}
	if resolver.componentsVisited != symlinkCount {
		t.Fatalf(
			"symlink component visits = %d, want %d",
			resolver.componentsVisited,
			symlinkCount,
		)
	}
}

func TestManifestCanonicalDecodingRejectsEquivalentWireVariants(t *testing.T) {
	digest := testDigest(0x41)
	valid := encodeManifest(
		1,
		[]directoryEntry{
			{Mode: 0o755, Path: "a"},
			{Mode: 0o700, Path: "b"},
		},
		[]manifestFile{{
			Kind: fileKindChunk,
			Chunk: chunkEntry{
				Digest: digest,
				Length: 1,
				Target: targetForDigest(digest),
			},
			Mode: 0o644,
			Path: "c",
			Size: 1,
		}},
		nil,
		"local://fixture",
	)
	lines := bytes.Split(valid[:len(valid)-1], []byte{'\n'})
	unordered := bytes.Join(
		[][]byte{lines[0], lines[1], lines[3], lines[2], lines[4]},
		[]byte{'\n'},
	)
	unordered = append(unordered, '\n')
	invalidUTF8 := append([]byte(nil), valid...)
	path := bytes.Index(invalidUTF8, []byte(`"path":"a"`))
	if path < 0 {
		t.Fatal("fixture path unavailable")
	}
	invalidUTF8[path+len(`"path":"`)] = 0xff

	tests := map[string][]byte{
		"invalid UTF-8": invalidUTF8,
		"duplicate key": []byte(strings.Replace(
			string(valid),
			`"mode":"0755"`,
			`"mode":"0755","mode":"0755"`,
			1,
		)),
		"unknown key": []byte(strings.Replace(
			string(valid),
			`"mode":"0755"`,
			`"mode":"0755","unknown":true`,
			1,
		)),
		"unknown nested key": []byte(strings.Replace(
			string(valid),
			`"target":{"relative_key":`,
			`"target":{"extra":0,"relative_key":`,
			1,
		)),
		"reordered header keys": []byte(strings.Replace(
			string(valid),
			`{"_type":"manifest_header","entry_count":3`,
			`{"entry_count":3,"_type":"manifest_header"`,
			1,
		)),
		"whitespace": []byte(strings.Replace(
			string(valid),
			`{"_type":"manifest_header"`,
			`{ "_type":"manifest_header"`,
			1,
		)),
		"short mode": []byte(strings.Replace(
			string(valid),
			`"mode":"0755"`,
			`"mode":"755"`,
			1,
		)),
		"record order":    unordered,
		"extra record":    append(append([]byte(nil), valid...), []byte("{}\n")...),
		"empty record":    append(append([]byte(nil), valid...), '\n'),
		"missing newline": append([]byte(nil), valid[:len(valid)-1]...),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeManifest(body, uint64(len(body)), 10); err == nil {
				t.Fatal("noncanonical manifest unexpectedly accepted")
			}
		})
	}
	if _, err := decodeManifest(valid, uint64(len(valid)-1), 10); err == nil {
		t.Fatal("manifest exceeding its decoded byte bound unexpectedly accepted")
	}
}

func TestMetadataScanningBoundsDenseAndLargeRecords(t *testing.T) {
	dense := bytes.Repeat([]byte{'\n'}, 1<<20)
	if _, err := decodeManifest(dense, uint64(len(dense)), 1); err == nil {
		t.Fatal("newline-dense manifest unexpectedly accepted")
	}
	if _, err := decodeChunkmap(dense, uint64(len(dense)), 0, 1); err == nil {
		t.Fatal("newline-dense chunkmap unexpectedly accepted")
	}

	longPath := strings.Repeat("p", defaultMaxPortablePathBytes)
	largeRecord := encodeManifest(
		0,
		[]directoryEntry{{Mode: 0o755, Path: longPath}},
		nil,
		nil,
		"local://fixture",
	)
	manifest, err := decodeManifest(largeRecord, uint64(len(largeRecord)), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Directories) != 1 || manifest.Directories[0].Path != longPath {
		t.Fatal("large canonical metadata record changed during decoding")
	}

	var excessiveRecords []byte
	excessiveRecords = appendManifestHeaderRecord(excessiveRecords, manifestHeaderWire{
		ManifestSchema: defaultManifestSchema,
	})
	excessiveRecords = append(excessiveRecords, '\n')
	excessiveRecords = appendProvenanceRecord(excessiveRecords, "provenance", provenanceWire{
		SourceFingerprint:     "source",
		SourceFingerprintType: "type",
		SourceURI:             "uri",
	})
	excessiveRecords = append(excessiveRecords, '\n')
	for _, prefix := range []string{"a/", "b/", "c/", "d/"} {
		excessiveRecords = appendProvenanceRecord(
			excessiveRecords,
			"prefix_provenance",
			provenanceWire{
				Prefix:                prefix,
				SourceFingerprint:     "source",
				SourceFingerprintType: "type",
				SourceURI:             "uri",
			},
		)
		excessiveRecords = append(excessiveRecords, '\n')
	}
	if _, err := decodeManifest(
		excessiveRecords,
		uint64(len(excessiveRecords)),
		1,
	); err == nil {
		t.Fatal("manifest record limit unexpectedly ignored")
	}
}

func TestChunkmapCanonicalDecodingRejectsWireVariants(t *testing.T) {
	digest := testDigest(0x52)
	valid := encodeChunkmap(1, []chunkEntry{{
		Digest: digest,
		Length: 1,
		Target: targetForDigest(digest),
	}})
	lines := bytes.Split(valid[:len(valid)-1], []byte{'\n'})
	reorderedRecords := append(append([]byte(nil), lines[1]...), '\n')
	reorderedRecords = append(reorderedRecords, lines[0]...)
	reorderedRecords = append(reorderedRecords, '\n')
	invalidUTF8 := append([]byte(nil), valid...)
	invalidUTF8[1] = 0xff
	tests := map[string][]byte{
		"invalid UTF-8": invalidUTF8,
		"duplicate key": []byte(strings.Replace(
			string(valid),
			`"length":1`,
			`"length":1,"length":1`,
			1,
		)),
		"unknown key": []byte(strings.Replace(
			string(valid),
			`"length":1`,
			`"extra":0,"length":1`,
			1,
		)),
		"reordered keys": []byte(strings.Replace(
			string(valid),
			string(lines[1]),
			`{"digest":"`+digest.String()+
				`","_type":"chunk","length":1,"offset":0,"target":{"relative_key":"`+
				targetForDigest(digest).RelativeKey+`"}}`,
			1,
		)),
		"whitespace": []byte(strings.Replace(
			string(valid),
			`{"_type":"chunk"`,
			`{ "_type":"chunk"`,
			1,
		)),
		"record order": reorderedRecords,
		"extra record": append(
			append([]byte(nil), valid...),
			appendChunkRecord(nil, chunkEntry{
				Digest: digest,
				Length: 1,
				Offset: 1,
				Target: targetForDigest(digest),
			})...,
		),
	}
	tests["extra record"] = append(tests["extra record"], '\n')
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeChunkmap(body, uint64(len(body)), 1, 1); err == nil {
				t.Fatal("noncanonical chunkmap unexpectedly accepted")
			}
		})
	}
}

func TestManifestModesPreserveReferenceEncoderRange(t *testing.T) {
	body := encodeManifest(
		0,
		[]directoryEntry{{Mode: ^uint16(0), Path: "directory"}},
		nil,
		nil,
		"local://fixture",
	)
	manifest, err := decodeManifest(body, uint64(len(body)), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := manifest.Directories[0].Mode; got != ^uint16(0) {
		t.Fatalf("decoded mode = %06o, want %06o", got, ^uint16(0))
	}
	noncanonical := strings.Replace(string(body), `"177777"`, `"0177777"`, 1)
	if _, err := decodeManifest(
		[]byte(noncanonical),
		uint64(len(noncanonical)),
		1,
	); err == nil {
		t.Fatal("noncanonical padded mode unexpectedly accepted")
	}
}

func TestEmptyManifestFileRequiresBLAKE3EmptyDigest(t *testing.T) {
	wrong := testDigest(0x91)
	body := encodeManifest(
		0,
		nil,
		[]manifestFile{{
			Kind: fileKindChunk,
			Chunk: chunkEntry{
				Digest: wrong,
				Target: targetForDigest(wrong),
			},
			Mode: 0o600,
			Path: "empty",
		}},
		nil,
		"local://fixture",
	)
	if _, err := decodeManifest(body, uint64(len(body)), 1); err == nil ||
		!IsCode(err, ErrorIntegrity) {
		t.Fatalf("empty digest error = %v, want %s", err, ErrorIntegrity)
	}
}
