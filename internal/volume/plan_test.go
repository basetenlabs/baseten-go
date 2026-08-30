package volume

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
)

// sampleManifest describes a tree with nesting, a name that is a prefix of
// another, and one of each entry kind.
func sampleManifest() *Manifest {
	digest := testDigest(0x11)
	file := func(path string, size uint64) FileEntry {
		return FileEntry{Path: path, Mode: 0o644, Kind: FileKindChunk, Size: size,
			Chunk: ChunkRef{Digest: digest, Length: size, Target: TargetForDigest(digest)}}
	}
	return &Manifest{
		Directories: []DirectoryEntry{
			{Path: "model", Mode: 0o755},
			{Path: "model/weights", Mode: 0o555},
			{Path: "models", Mode: 0o755},
			{Path: "empty", Mode: 0o700},
			{Path: "model/cache", Mode: 0o700},
		},
		Files: []FileEntry{
			file("model/config.json", 10),
			file("model/weights/w0.bin", 20),
			file("model/weights/w1.bin", 30),
			file("models/other.txt", 40),
			file("top.txt", 50),
		},
		Symlinks: []SymlinkEntry{{Path: "model/latest", Target: "weights/w1.bin", Mode: SymlinkMode}},
	}
}

func TestSelectEntries(t *testing.T) {
	tests := []struct {
		name      string
		include   []string
		wantFiles string
		wantDirs  string
		wantLinks string
	}{
		{
			name:      "one file",
			include:   []string{"top.txt"},
			wantFiles: "top.txt",
			wantDirs:  "",
		},
		{
			// A selector matches on slash boundaries, so "model" does not
			// reach into "models" despite being a prefix of the string.
			name:      "directory does not match a longer name",
			include:   []string{"model"},
			wantFiles: "model/config.json,model/weights/w0.bin,model/weights/w1.bin",
			wantDirs:  "model,model/cache,model/weights",
			wantLinks: "model/latest",
		},
		{
			name:      "nested directory brings its ancestors",
			include:   []string{"model/weights"},
			wantFiles: "model/weights/w0.bin,model/weights/w1.bin",
			wantDirs:  "model,model/weights",
		},
		{
			name:      "several selectors are a union",
			include:   []string{"model/weights", "models/other.txt"},
			wantFiles: "model/weights/w0.bin,model/weights/w1.bin,models/other.txt",
			wantDirs:  "model,model/weights,models",
		},
		{
			// Overlapping selectors name each entry once, not twice.
			name:      "overlapping selectors",
			include:   []string{"model", "model/weights", "model/config.json"},
			wantFiles: "model/config.json,model/weights/w0.bin,model/weights/w1.bin",
			wantDirs:  "model,model/cache,model/weights",
			wantLinks: "model/latest",
		},
		{
			name:     "an empty directory on its own",
			include:  []string{"empty"},
			wantDirs: "empty",
		},
		{
			// Nothing inside it can pull its parent into the plan, so the
			// directory has to do that itself — otherwise "model" is created
			// by MkdirAll with whatever the umask allows and nothing records
			// the mode the manifest gave it.
			name:     "a nested empty directory brings its ancestors",
			include:  []string{"model/cache"},
			wantDirs: "model,model/cache",
		},
		{
			// A selected directory brings its ancestors, so their recorded
			// modes survive into the plan rather than being left to the umask.
			name:      "a nested directory selected on its own",
			include:   []string{"model/weights"},
			wantDirs:  "model,model/weights",
			wantFiles: "model/weights/w0.bin,model/weights/w1.bin",
		},
		{
			name:      "leading and trailing slashes are trimmed",
			include:   []string{"/model/weights/"},
			wantFiles: "model/weights/w0.bin,model/weights/w1.bin",
			wantDirs:  "model,model/weights",
		},
		{
			name:      "a symlink by name",
			include:   []string{"model/latest"},
			wantDirs:  "model",
			wantLinks: "model/latest",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectEntries(sampleManifest(), tc.include)
			require.NoError(t, err)
			require.Equal(t, tc.wantFiles, joinPaths(got.Files, func(f FileEntry) string { return f.Path }))
			require.Equal(t, sortedCSV(tc.wantDirs), joinPaths(got.Directories, func(d DirectoryEntry) string { return d.Path }))
			require.Equal(t, tc.wantLinks, joinPaths(got.Symlinks, func(s SymlinkEntry) string { return s.Path }))
		})
	}
}

// TestSelectEntriesWithNoSelectors returns the manifest untouched, in its own
// order rather than a rebuilt one.
func TestSelectEntriesWithNoSelectors(t *testing.T) {
	manifest := sampleManifest()
	got, err := SelectEntries(manifest, nil)
	require.NoError(t, err)
	require.Equal(t, manifest, got)
}

// TestSelectEntriesKeepsRecordedModes checks that narrowing a manifest does
// not lose the modes of the directories it keeps.
func TestSelectEntriesKeepsRecordedModes(t *testing.T) {
	got, err := SelectEntries(sampleManifest(), []string{"model/weights"})
	require.NoError(t, err)
	require.Len(t, got.Directories, 2)
	require.Equal(t, uint16(0o755), got.Directories[0].Mode)
	require.Equal(t, uint16(0o555), got.Directories[1].Mode)
}

func TestSelectEntriesRejectsBadSelectors(t *testing.T) {
	tests := map[string]string{
		"no match":         "missing",
		"partial name":     "mode",
		"below a file":     "top.txt/nested",
		"escaping":         "../outside",
		"selects the root": "/",
		"only slashes":     "///",
	}
	for name, selector := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := SelectEntries(sampleManifest(), []string{selector})
			require.Error(t, err)
		})
	}

	// The error names the selector that missed, not just that something did.
	_, err := SelectEntries(sampleManifest(), []string{"model", "nowhere"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "nowhere")
}

func TestCheckPlan(t *testing.T) {
	require.NoError(t, CheckPlan(sampleManifest(), false))

	t.Run("slabmap", func(t *testing.T) {
		m := sampleManifest()
		m.Files[0].Kind = FileKindSlabmap
		err := CheckPlan(m, false)
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrUnsupportedEntry), "got %v", err)
		require.Contains(t, err.Error(), "model/config.json")
	})

	t.Run("unknown kind", func(t *testing.T) {
		m := sampleManifest()
		m.Files[0].Kind = "tarball"
		require.True(t, errors.Is(CheckPlan(m, false), ErrUnsupportedEntry), "unknown kinds should be refused")
	})

	t.Run("escaping path", func(t *testing.T) {
		m := sampleManifest()
		m.Files[0].Path = "../outside"
		require.Error(t, CheckPlan(m, false))
	})

	t.Run("case collision", func(t *testing.T) {
		m := sampleManifest()
		m.Files[0].Path = "TOP.txt"
		err := CheckPlan(m, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "differ only in case")

		// The same tree is perfectly materializable where the filesystem
		// tells the two names apart, and a volume pushed from Linux is
		// entitled to come back on Linux.
		require.NoError(t, CheckPlan(m, true))
	})

	t.Run("exact duplicate is refused either way", func(t *testing.T) {
		m := sampleManifest()
		m.Files[0].Path = "top.txt"
		require.Error(t, CheckPlan(m, false))
		require.Error(t, CheckPlan(m, true))
	})

	t.Run("a file and a directory of the same name", func(t *testing.T) {
		m := sampleManifest()
		m.Files[0].Path = "models"
		require.Error(t, CheckPlan(m, false))
	})

	t.Run("empty symlink target", func(t *testing.T) {
		m := sampleManifest()
		m.Symlinks[0].Target = ""
		require.Error(t, CheckPlan(m, false))
	})
}

func joinPaths[T any](entries []T, path func(T) string) string {
	paths := make([]string, len(entries))
	for i, entry := range entries {
		paths[i] = path(entry)
	}
	return strings.Join(paths, ",")
}

// sortedCSV puts an expectation in the order SelectEntries returns
// directories, which is by path.
func sortedCSV(csv string) string {
	if csv == "" {
		return ""
	}
	parts := strings.Split(csv, ",")
	slices.Sort(parts)
	return strings.Join(parts, ",")
}
