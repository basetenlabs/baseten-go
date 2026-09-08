package client

import (
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
)

func TestParseVolumeRef(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  VolumeRef
		level VolumeRefLevel
		// canonical is the String output when it differs from input.
		canonical string
	}{
		{
			name:      "namespace",
			input:     "bdn:weights",
			want:      VolumeRef{Namespace: "weights"},
			level:     VolumeRefLevelNamespace,
			canonical: "bdn:weights/",
		},
		{
			name:  "namespace with trailing slash is the same ref",
			input: "bdn:weights/",
			want:  VolumeRef{Namespace: "weights"},
			level: VolumeRefLevelNamespace,
		},
		{
			name:  "volume",
			input: "bdn:weights/llama",
			want:  VolumeRef{Namespace: "weights", Volume: "llama"},
			level: VolumeRefLevelVolume,
		},
		{
			name:  "tag",
			input: "bdn:weights/llama:prod",
			want:  VolumeRef{Namespace: "weights", Volume: "llama", Tag: "prod"},
			level: VolumeRefLevelPoint,
		},
		{
			name:  "digest prefix",
			input: "bdn:weights/llama@b3:a1b2c3d4e5f6",
			want:  VolumeRef{Namespace: "weights", Volume: "llama", Digest: "b3:a1b2c3d4e5f6"},
			level: VolumeRefLevelPoint,
		},
		{
			name:      "digest hex is folded",
			input:     "bdn:weights/llama@B3:A1B2C3D4E5F6",
			want:      VolumeRef{Namespace: "weights", Volume: "llama", Digest: "b3:a1b2c3d4e5f6"},
			level:     VolumeRefLevelPoint,
			canonical: "bdn:weights/llama@b3:a1b2c3d4e5f6",
		},
		{
			name:  "root path",
			input: "bdn:weights/llama/",
			want:  VolumeRef{Namespace: "weights", Volume: "llama", Path: "/"},
			level: VolumeRefLevelPath,
		},
		{
			name:  "path",
			input: "bdn:weights/llama/config/model.json",
			want:  VolumeRef{Namespace: "weights", Volume: "llama", Path: "/config/model.json"},
			level: VolumeRefLevelPath,
		},
		{
			name:      "trailing slash on a path is dropped",
			input:     "bdn:weights/llama/config/",
			want:      VolumeRef{Namespace: "weights", Volume: "llama", Path: "/config"},
			level:     VolumeRefLevelPath,
			canonical: "bdn:weights/llama/config",
		},
		{
			name:  "tag and path together",
			input: "bdn:weights/llama:prod/config/model.json",
			want:  VolumeRef{Namespace: "weights", Volume: "llama", Tag: "prod", Path: "/config/model.json"},
			level: VolumeRefLevelPath,
		},
		{
			name:  "selector characters are free inside a path",
			input: "bdn:weights/llama/model@v2:1.bin",
			want:  VolumeRef{Namespace: "weights", Volume: "llama", Path: "/model@v2:1.bin"},
			level: VolumeRefLevelPath,
			// Both are outside the unreserved set, so both come back encoded.
			canonical: "bdn:weights/llama/model%40v2%3A1.bin",
		},
		{
			name:      "names fold to lowercase",
			input:     "bdn:Weights/Llama",
			want:      VolumeRef{Namespace: "weights", Volume: "llama"},
			level:     VolumeRefLevelVolume,
			canonical: "bdn:weights/llama",
		},
		{
			name:  "tags are case-sensitive",
			input: "bdn:weights/llama:Prod",
			want:  VolumeRef{Namespace: "weights", Volume: "llama", Tag: "Prod"},
			level: VolumeRefLevelPoint,
		},
		{
			name:      "percent-encoded path segment is decoded once",
			input:     "bdn:weights/llama/a%20b/c",
			want:      VolumeRef{Namespace: "weights", Volume: "llama", Path: "/a b/c"},
			level:     VolumeRefLevelPath,
			canonical: "bdn:weights/llama/a%20b/c",
		},
		{
			name:  "surrounding space is ignored",
			input: "  bdn:weights/llama  ",
			want:  VolumeRef{Namespace: "weights", Volume: "llama"},
			level: VolumeRefLevelVolume,
			// String renders the trimmed ref.
			canonical: "bdn:weights/llama",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseVolumeRef(test.input)
			require.NoError(t, err)
			require.Equal(t, test.want, got)
			require.Equal(t, test.level, got.Level())

			want := test.canonical
			if want == "" {
				want = test.input
			}
			require.Equal(t, want, got.String())
		})
	}
}

func TestParseVolumeRefRoundTrips(t *testing.T) {
	// Whatever String writes, ParseVolumeRef must read back to the same value.
	for _, input := range []string{
		"bdn:weights/",
		"bdn:weights/llama",
		"bdn:weights/llama:prod",
		"bdn:weights/llama@b3:a1b2c3d4e5f6",
		"bdn:weights/llama/",
		"bdn:weights/llama/config/model.json",
		"bdn:weights/llama:prod/config/model.json",
		"bdn:weights/llama/a%20b",
		"bdn:weights/llama/model%40v2.bin",
	} {
		t.Run(input, func(t *testing.T) {
			first, err := ParseVolumeRef(input)
			require.NoError(t, err)
			second, err := ParseVolumeRef(first.String())
			require.NoError(t, err)
			require.Equal(t, first, second)
			require.Equal(t, first.String(), second.String())
		})
	}
}

func TestParseVolumeRefErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
		// contains is a distinctive fragment of the message, so a test fails
		// when a ref is refused for the wrong reason rather than merely refused.
		contains string
	}{
		{"empty", "", "want a ref beginning"},
		{"no scheme", "weights/llama", "want a ref beginning"},
		{"bare digest", "b3:a1b2c3", "want a ref beginning"},
		{"wrong scheme", "s3://weights/llama", "want a ref beginning"},

		{"bdn:// volume", "bdn://weights/llama", "bdn:// is not supported"},
		{"bdn:// namespace", "bdn://weights", "bdn:// is not supported"},
		{"object ref", "bdn+obj://weights/manifest@b3:a1b2", "object refs"},

		{"no namespace", "bdn:", "no namespace"},
		{"no namespace with slash", "bdn:/llama", "no namespace"},
		{"selector on a namespace", "bdn:weights:prod", "takes no selector"},
		{"digest on a namespace", "bdn:weights@a1b2", "takes no selector"},

		{"namespace too short", "bdn:w/llama", "2 to 256 characters"},
		{"namespace leading digit", "bdn:1weights/llama", "must begin with a letter"},
		{"namespace underscore", "bdn:we_ights/llama", "must begin with a letter"},
		{"reserved namespace", "bdn:namespaces/llama", `namespace "namespaces" is reserved`},
		{"reserved namespace resolve", "bdn:resolve/llama", `namespace "resolve" is reserved`},

		{"no volume", "bdn:weights//config", "no volume"},
		{"volume too short", "bdn:weights/l", "2 to 256 characters"},
		{"volume leading digit", "bdn:weights/1llama", "must begin with a letter"},
		{"reserved volume", "bdn:weights/manifests", `volume "manifests" is reserved`},
		{"reserved volume objects", "bdn:weights/objects", `volume "objects" is reserved`},

		{"empty tag", "bdn:weights/llama:", "no tag after"},
		{"tag with colon", "bdn:weights/llama:a:b", `tag "a:b"`},
		{"tag leading dot", "bdn:weights/llama:.prod", `tag ".prod"`},

		{"empty digest", "bdn:weights/llama@", "no digest after"},
		{"digest with no algorithm", "bdn:weights/llama@a1b2c3", `must begin "b3:"`},
		{"algorithm with no digest", "bdn:weights/llama@b3:", "no digest after"},
		{"unknown algorithm", "bdn:weights/llama@sha256:a1b2c3", `must begin "b3:"`},
		{"digest not hex", "bdn:weights/llama@b3:xyz", "is not hex"},
		// The first @ starts the digest and takes the rest of the segment with
		// it, so a second colon after one is digest material rather than a tag.
		{"digest with colon", "bdn:weights/llama@b3:abc:def", "is not hex"},
		// And @ wins over an earlier colon, leaving the colon inside the
		// volume name where the name rule refuses it.
		{"tag before digest", "bdn:weights/llama:prod@b3:abc", "must begin with a letter"},
		{"digest too long", "bdn:weights/llama@b3:" + strings.Repeat("a", 65), "longer than 64 hex characters"},

		{"dot segment", "bdn:weights/llama/./x", `path segment "." is not allowed`},
		{"dotdot segment", "bdn:weights/llama/../x", `path segment ".." is not allowed`},
		{"encoded dotdot segment", "bdn:weights/llama/%2e%2e/x", `path segment ".." is not allowed`},
		{"empty path segment", "bdn:weights/llama/a//b", "empty segment"},
		{"bad percent encoding", "bdn:weights/llama/a%zz", "invalid URL escape"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseVolumeRef(test.input)
			require.Error(t, err)
			require.Contains(t, err.Error(), test.contains)
		})
	}
}

func TestVolumeRefLevelString(t *testing.T) {
	require.Equal(t, "namespace", VolumeRefLevelNamespace.String())
	require.Equal(t, "volume", VolumeRefLevelVolume.String())
	require.Equal(t, "point", VolumeRefLevelPoint.String())
	require.Equal(t, "path", VolumeRefLevelPath.String())
	require.Equal(t, "VolumeRefLevel(0)", VolumeRefLevel(0).String())
}
