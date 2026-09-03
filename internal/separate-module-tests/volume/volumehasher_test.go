// Tests that a real BLAKE3 implementation satisfies the hasher contract the
// volume package states in its documentation. The root module has no
// dependencies, so the one thing it cannot check for itself is that the
// hashing seam it asks callers to fill is fillable as documented.
package separatemoduletests_test

import (
	"hash"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/zeebo/blake3"
)

func newHasher() hash.Hash { return blake3.New() }

func TestBlake3SatisfiesHasherContract(t *testing.T) {
	if err := volume.CheckHasher(newHasher); err != nil {
		t.Fatalf("blake3.New does not satisfy the hasher contract: %v", err)
	}
}

// TestBlake3DigestsMatchPublishedVectors pins the digests the whole protocol
// is addressed by, so a dependency upgrade that changed them would fail here
// rather than in a volume no client can resolve.
func TestBlake3DigestsMatchPublishedVectors(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{"empty", nil, "b3:af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262"},
		{"abc", []byte("abc"), "b3:6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85"},
		{
			// The 1024-byte prefix of the reference test vector input, which
			// crosses BLAKE3's chunk boundary and so exercises its tree mode.
			name:  "one chunk",
			input: vectorInput(1024),
			want:  "b3:42214739f095a406f3fc83deb889744ac00df831c10daa55189b5d121c855af7",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := volume.HashBytes(newHasher, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != tc.want {
				t.Fatalf("digest of %s = %s, want %s", tc.name, got, tc.want)
			}
		})
	}
}

// TestHashedManifestIsStable checks the two halves of content addressing
// together: the canonical encoder's bytes and a real hash over them.
func TestHashedManifestIsStable(t *testing.T) {
	digest, err := volume.HashBytes(newHasher, nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest := &volume.Manifest{
		Provenance: volume.Provenance{
			SourceFingerprint:     volume.ProvenanceFingerprint,
			SourceFingerprintType: volume.ProvenanceFingerprintType,
			SourceURI:             "file:///tmp/tree",
		},
		Directories: []volume.DirectoryEntry{{Path: "dir", Mode: 0o755}},
		Files: []volume.FileEntry{{
			Path:  "dir/empty",
			Mode:  0o644,
			Kind:  volume.FileKindChunk,
			Chunk: volume.ChunkRef{Digest: digest, Target: volume.TargetForDigest(digest)},
		}},
	}
	encoded := volume.EncodeManifest(manifest)
	want := strings.Join([]string{
		`{"_type":"manifest_header","entry_count":2,"manifest_schema":"v1","total_size":0}`,
		`{"_type":"provenance","source_fingerprint":"local","source_fingerprint_type":"local_push","source_uri":"file:///tmp/tree"}`,
		`{"_type":"directory","mode":"0755","path":"dir"}`,
		`{"_type":"file","_kind":"chunk","chunk":{"digest":"` + digest.String() +
			`","length":0,"offset":0,"target":{"relative_key":"` +
			volume.TargetForDigest(digest).RelativeKey + `"}},"mode":"0644","path":"dir/empty"}`,
		"",
	}, "\n")
	if string(encoded) != want {
		t.Fatalf("manifest bytes\n got: %s\nwant: %s", encoded, want)
	}

	manifestDigest, err := volume.HashBytes(newHasher, encoded)
	if err != nil {
		t.Fatal(err)
	}
	const wantDigest = "b3:643f3a096ed8749f1b4449e3a66d16deb15770e1db12eb156e692624bd5856b4"
	if manifestDigest.String() != wantDigest {
		t.Fatalf("manifest digest = %s, want %s", manifestDigest, wantDigest)
	}
}

// vectorInput builds the BLAKE3 reference test-vector input: the bytes
// 0, 1, ... 250, repeating.
func vectorInput(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}
