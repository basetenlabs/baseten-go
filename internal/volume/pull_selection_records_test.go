package volume

import (
	"strings"
	"testing"
)

func TestManifestAndChunkmapGraphValidation(t *testing.T) {
	digest := testDigest(1)
	target := targetForDigest(digest)
	valid := encodeManifest(
		4,
		[]directoryEntry{{Mode: 0o755, Path: "dir"}},
		[]manifestFile{{
			Kind: fileKindChunk,
			Chunk: chunkEntry{
				Digest: digest, Length: 4, Target: target,
			},
			Mode: 0o644,
			Path: "dir/file",
			Size: 4,
		}},
		nil,
		"local://fixture",
	)
	manifest, err := decodeManifest(valid, uint64(len(valid)), 10)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.TotalSize != 4 || len(manifest.Files) != 1 {
		t.Fatalf("decoded manifest = %+v", manifest)
	}
	invalidPath := strings.Replace(string(valid), `"dir/file"`, `"../file"`, 1)
	if _, err := decodeManifest([]byte(invalidPath), uint64(len(invalidPath)), 10); err == nil {
		t.Fatal("traversing manifest path unexpectedly accepted")
	}
	wrongTotal := strings.Replace(string(valid), `"total_size":4`, `"total_size":5`, 1)
	if _, err := decodeManifest([]byte(wrongTotal), uint64(len(wrongTotal)), 10); err == nil {
		t.Fatal("wrong manifest total unexpectedly accepted")
	}
	chunkmapBody := encodeChunkmap(4, []chunkEntry{
		{Digest: digest, Length: 2, Offset: 0, Target: target},
		{Digest: digest, Length: 2, Offset: 2, Target: target},
	})
	if _, err := decodeChunkmap(chunkmapBody, uint64(len(chunkmapBody)), 4, 2); err != nil {
		t.Fatal(err)
	}
	gapped := strings.Replace(string(chunkmapBody), `"offset":2`, `"offset":3`, 1)
	if _, err := decodeChunkmap(
		[]byte(gapped),
		uint64(len(gapped)),
		4,
		2,
	); err == nil {
		t.Fatal("gapped chunkmap unexpectedly accepted")
	}
	_, err = selectManifest(validatedManifest{
		Files: []manifestFile{
			{Path: "first", Size: ^uint64(0)},
			{Path: "second", Size: 1},
		},
	}, []string{"first", "second"}, 10, defaultPortablePathLimits())
	if err == nil || !IsCode(err, ErrorProtocol) {
		t.Fatalf("selected-size overflow error = %v, want %s", err, ErrorProtocol)
	}
}

func TestManifestSymlinkGraphPreservesSafeDanglingAndSelectedLinks(t *testing.T) {
	dangling := validatedManifest{Symlinks: []symlinkEntry{{
		Mode: 0o777, Path: "link", Target: "missing/child",
	}}}
	if err := validateManifestStructure(
		dangling,
		10,
		defaultPortablePathLimits(),
	); err != nil {
		t.Fatalf("safe dangling symlink error = %v", err)
	}

	manifest := validatedManifest{
		Directories: []directoryEntry{
			{Mode: 0o755, Path: "keep"},
			{Mode: 0o755, Path: "omitted"},
		},
		Symlinks: []symlinkEntry{
			{Mode: 0o777, Path: "keep/link", Target: "../omitted/target"},
			{Mode: 0o777, Path: "omitted/target", Target: "missing"},
		},
	}
	selected, err := selectManifest(
		manifest,
		[]string{"keep"},
		10,
		defaultPortablePathLimits(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Symlinks) != 1 ||
		selected.Symlinks[0].Path != "keep/link" ||
		len(selected.Directories) != 1 {
		t.Fatalf("selected manifest = %+v", selected)
	}
}

func TestPortablePathLimitsIncludeImplicitParents(t *testing.T) {
	limits := portablePathLimits{maxPathBytes: 5, maxPathComponents: 3}
	if err := validatePortablePath("a/b/c", limits); err != nil {
		t.Fatalf("boundary path error = %v", err)
	}
	for _, path := range []string{"a/b/cd", "a/b/c/d"} {
		if err := validatePortablePath(path, limits); err == nil ||
			!IsCode(err, ErrorPreconditionFailed) {
			t.Fatalf("overflow path %q error = %v", path, err)
		}
	}
	if err := validateSymlinkTarget("link", "12345", limits); err != nil {
		t.Fatalf("boundary symlink target error = %v", err)
	}
	if err := validateSymlinkTarget("link", "123456", limits); err == nil ||
		!IsCode(err, ErrorPreconditionFailed) {
		t.Fatalf("symlink target overflow error = %v", err)
	}

	digest := testDigest(0x44)
	body := encodeManifest(
		1,
		nil,
		[]manifestFile{{
			Kind: fileKindChunk,
			Chunk: chunkEntry{
				Digest: digest, Length: 1, Target: targetForDigest(digest),
			},
			Mode: 0o600,
			Path: "a/b/c",
			Size: 1,
		}},
		nil,
		"local://fixture",
	)
	if _, err := decodeManifest(body, uint64(len(body)), 3, limits); err != nil {
		t.Fatalf("implicit-parent boundary error = %v", err)
	}
	if _, err := decodeManifest(body, uint64(len(body)), 2, limits); err == nil ||
		!IsCode(err, ErrorPreconditionFailed) {
		t.Fatalf("implicit-parent overflow error = %v", err)
	}
	if _, err := selectManifest(
		validatedManifest{Files: []manifestFile{{Path: "a/b/c", Size: 1}}},
		[]string{"a"},
		2,
		limits,
	); err == nil || !IsCode(err, ErrorPreconditionFailed) {
		t.Fatalf("selected implicit-parent overflow error = %v", err)
	}
}
