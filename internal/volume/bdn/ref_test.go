package bdn

import (
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
)

func TestParseRef(t *testing.T) {
	tests := []struct {
		in     string
		want   Ref
		render string
	}{
		{"ns/vol", Ref{Namespace: "ns", Volume: "vol"}, "bdn://ns/vol"},
		{"bdn://ns/vol", Ref{Namespace: "ns", Volume: "vol"}, "bdn://ns/vol"},
		{"ns/vol:prod", Ref{Namespace: "ns", Volume: "vol", Tag: "prod"}, "bdn://ns/vol:prod"},
		{
			"bdn://ns/vol@b3:abc123abc123",
			Ref{Namespace: "ns", Volume: "vol", Digest: "b3:abc123abc123"},
			"bdn://ns/vol@b3:abc123abc123",
		},
		// Namespace and volume are lowercased at every boundary the service
		// has, so a ref that differs only in case names the same volume.
		{"NS/Vol", Ref{Namespace: "ns", Volume: "vol"}, "bdn://ns/vol"},
		// Tags are case-sensitive, and are left alone.
		{"ns/vol:Prod", Ref{Namespace: "ns", Volume: "vol", Tag: "Prod"}, "bdn://ns/vol:Prod"},
		{"  ns/vol  ", Ref{Namespace: "ns", Volume: "vol"}, "bdn://ns/vol"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseRef(tc.in)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.render, got.String())
		})
	}
}

func TestParseRefRejectsMalformed(t *testing.T) {
	tests := map[string]string{
		"empty":            "",
		"no volume":        "ns",
		"no namespace":     "/vol",
		"empty volume":     "ns/",
		"too many parts":   "ns/vol/extra",
		"tag with nothing": "ns/vol:",
		"pin with nothing": "ns/vol@",
	}
	for name, ref := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseRef(ref)
			require.Error(t, err)
		})
	}
}

// TestPinnedDropsTheTag covers what makes a download repeatable: the tag that
// selected a version is replaced by the version itself, so resolving again
// cannot land somewhere else.
func TestPinnedDropsTheTag(t *testing.T) {
	ref, err := ParseRef("ns/vol:prod")
	require.NoError(t, err)

	pinned := ref.Pinned("b3:deadbeef")
	require.Equal(t, "", pinned.Tag)
	require.Equal(t, "bdn://ns/vol@b3:deadbeef", pinned.String())
}
