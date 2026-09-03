package volume

import (
	"runtime"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
)

func TestValidatePath(t *testing.T) {
	valid := []string{
		"file",
		"dir/file",
		"a/b/c/d.txt",
		"weird name with spaces",
		"unicode/héllo",
		"z<file>&",
		"...",
		"..hidden",
		"a..b",
	}
	for _, path := range valid {
		t.Run("valid/"+path, func(t *testing.T) {
			require.NoError(t, ValidatePath(path))
		})
	}

	invalid := map[string]string{
		"empty":            "",
		"absolute":         "/etc/passwd",
		"trailing slash":   "dir/",
		"dot segment":      "a/./b",
		"parent segment":   "a/../b",
		"leading parent":   "../escape",
		"bare parent":      "..",
		"bare dot":         ".",
		"empty segment":    "a//b",
		"backslash":        `a\b`,
		"windows absolute": `C:\tree\file`,
		"nul byte":         "a\x00b",
		"invalid utf-8":    "a\xffb",
	}
	for name, path := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			require.Error(t, ValidatePath(path))
		})
	}
}

// TestValidateSourceURI covers the third piece of text that reaches the
// encoder, after entry paths and symlink targets. All three are inside the
// digest, so all three are held to the same standard.
func TestValidateSourceURI(t *testing.T) {
	for _, uri := range []string{
		"file:///home/user/models",
		"file:///C:/models",
		"file:///tmp/mødels",
		"s3://bucket/prefix",
	} {
		t.Run("valid/"+uri, func(t *testing.T) {
			require.NoError(t, ValidateSourceURI(uri))
		})
	}

	invalid := map[string]string{
		"empty":         "",
		"invalid utf-8": "file:///tmp/a\xffb",
		"nul byte":      "file:///tmp/a\x00b",
	}
	for name, uri := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			require.Error(t, ValidateSourceURI(uri))
		})
	}
}

func TestNormalizeSymlinkTarget(t *testing.T) {
	// Targets are stored verbatim, absolute ones included: the format allows
	// them, and a target is whatever readlink returned. That includes names
	// that merely look windows-ish — "c:data" is a legal unix filename with a
	// colon in it, and "//usr/lib/x" is an absolute target with a doubled
	// slash — because only a backslash marks a target as windows-shaped.
	for _, target := range []string{
		"sibling", "../up/two", "/absolute/target", "./here",
		"c:data", "D:/data", "//usr/lib/x", "//server/share/file",
	} {
		t.Run("kept/"+target, func(t *testing.T) {
			got, err := NormalizeSymlinkTarget(target)
			require.NoError(t, err)
			require.Equal(t, target, got)
		})
	}

	rejected := map[string]string{
		"empty":           "",
		"drive letter":    `C:\Windows`,
		"unc":             `\\server\share\file`,
		"mixed unc":       `//server\share`,
		"extended length": `\\?\C:\long`,
		"nul byte":        "a\x00b",
		"invalid utf-8":   "a\xffb",
	}
	for name, target := range rejected {
		t.Run("rejected/"+name, func(t *testing.T) {
			_, err := NormalizeSymlinkTarget(target)
			require.Error(t, err)
		})
	}

	// The mixed spelling — a doubled forward slash with a backslash later —
	// is UNC in windows's other spelling, and the refusal must say so.
	_, err := NormalizeSymlinkTarget(`//server\share`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "UNC")
}

// TestNormalizeSymlinkTargetSeparators pins the one platform-dependent piece
// of normalization: a backslash is a separator on Windows and an ordinary
// character in a filename everywhere else.
func TestNormalizeSymlinkTargetSeparators(t *testing.T) {
	got, err := NormalizeSymlinkTarget(`up\one`)
	require.NoError(t, err)
	if runtime.GOOS == "windows" {
		require.Equal(t, "up/one", got)
	} else {
		require.Equal(t, `up\one`, got)
	}
}

func TestPathIndexRejectsCollisions(t *testing.T) {
	index := newPathIndex(false)
	require.NoError(t, index.add("dir/File.txt"))
	require.NoError(t, index.add("dir/other"))

	err := index.add("dir/File.txt")
	require.Error(t, err)
	require.Contains(t, err.Error(), "appears twice")

	err = index.add("dir/file.txt")
	require.Error(t, err)
	require.Contains(t, err.Error(), "differ only in case")
}

// TestPathIndexOnACaseSensitiveFilesystem covers the destination that really
// can hold both names. An exact duplicate is still refused: no filesystem
// holds one path twice.
func TestPathIndexOnACaseSensitiveFilesystem(t *testing.T) {
	index := newPathIndex(true)
	require.NoError(t, index.add("dir/File.txt"))
	require.NoError(t, index.add("dir/file.txt"))

	err := index.add("dir/file.txt")
	require.Error(t, err)
	require.Contains(t, err.Error(), "appears twice")
}

func TestParentPaths(t *testing.T) {
	tests := map[string][]string{
		"file":        nil,
		"a/b":         {"a"},
		"a/b/c":       {"a/b", "a"},
		"a/b/c/d.txt": {"a/b/c", "a/b", "a"},
	}
	for path, want := range tests {
		t.Run(path, func(t *testing.T) {
			got := parentPaths(path)
			require.Equal(t, strings.Join(want, ","), strings.Join(got, ","))
		})
	}
}

// TestWindowsLookingTargetsRoundTrip pins that targets the format accepts —
// a colon-bearing relative name, a doubled-slash absolute — survive encode
// and decode verbatim. Only a backslash marks a target as windows-shaped;
// these are ordinary unix bytes.
func TestWindowsLookingTargetsRoundTrip(t *testing.T) {
	m := &Manifest{Symlinks: []SymlinkEntry{
		{Path: "colon", Target: "c:data", Mode: SymlinkMode},
		{Path: "doubled", Target: "//usr/lib/x", Mode: SymlinkMode},
	}}
	decoded, err := DecodeManifest(EncodeManifest(m))
	require.NoError(t, err)
	require.Len(t, decoded.Symlinks, 2)
	require.Equal(t, "c:data", decoded.Symlinks[0].Target)
	require.Equal(t, "//usr/lib/x", decoded.Symlinks[1].Target)
}
