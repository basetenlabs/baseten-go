package separatemoduletests_test

import (
	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/zeebo/blake3"
	"hash"
	"strings"
	"testing"
)

func newZeeboBLAKE3() hash.Hash {
	return blake3.New()
}

func TestZeeboBLAKE3CanonicalRecordGoldensMatchReferenceEncoder(t *testing.T) {
	firstHex := strings.Repeat("11", 32)
	secondHex := strings.Repeat("22", 32)
	firstDigest := "b3:" + firstHex
	secondDigest := "b3:" + secondHex
	firstTarget := "objects/b3/11/11/" + firstHex
	secondTarget := "objects/b3/22/22/" + secondHex
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "chunkmap",
			body: `{"_type":"chunkmap_header","chunk_count":2,"file_size":5}` + "\n" +
				`{"_type":"chunk","digest":"` + firstDigest + `","length":3,"offset":0,"target":{"relative_key":"` + firstTarget + `"}}` + "\n" +
				`{"_type":"chunk","digest":"` + secondDigest + `","length":2,"offset":3,"target":{"relative_key":"` + secondTarget + `"}}` + "\n",
			want: "b3:2701a76368441ffafaf6de69b612873c2fcb5f8ff4b6435dc0fdc6e0d41925af",
		},
		{
			name: "manifest",
			body: `{"_type":"manifest_header","entry_count":3,"manifest_schema":"v1","total_size":3}` + "\n" +
				`{"_type":"provenance","source_fingerprint":"local","source_fingerprint_type":"local_push","source_uri":"local://fixture"}` + "\n" +
				`{"_type":"directory","mode":"0750","path":"a"}` + "\n" +
				`{"_type":"file","_kind":"chunk","chunk":{"digest":"` + firstDigest + `","length":3,"offset":0,"target":{"relative_key":"` + firstTarget + `"}},"mode":"0640","path":"z<file"}` + "\n" +
				`{"_type":"symlink","mode":"0777","path":"link","target":"z<file"}` + "\n",
			want: "b3:331a3b17750c3d759c82d47d55a0875959c2b031ea215626cc459d760549f070",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := volume.Digest(blake3.Sum256([]byte(test.body))).String(); got != test.want {
				t.Fatalf("digest = %s, want %s", got, test.want)
			}
		})
	}
}
