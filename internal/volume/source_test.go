package volume

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
)

// writeTree materializes a description of a tree under a fresh temporary
// directory. Keys are slash-separated relative paths; a value ending in "/"
// makes a directory, "->" makes a symlink, and anything else is file content.
func writeTree(t *testing.T, spec map[string]string) string {
	t.Helper()
	root := t.TempDir()
	// Directories first, so a file's parent always exists by the time it is
	// written whatever order the map ranges in.
	for path, content := range spec {
		if !strings.HasPrefix(content, "->") {
			require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.FromSlash(path), ".."), 0o755))
		}
	}
	for path, content := range spec {
		abs := filepath.Join(root, filepath.FromSlash(path))
		switch {
		case strings.HasPrefix(content, "->"):
			require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
			if err := os.Symlink(strings.TrimPrefix(content, "->"), abs); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		case content == "/":
			require.NoError(t, os.MkdirAll(abs, 0o755))
		default:
			require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
		}
	}
	return root
}

func TestScanSource(t *testing.T) {
	root := writeTree(t, map[string]string{
		"top.txt":         "hello",
		"empty.txt":       "",
		"nested/deep/a":   "aaa",
		"nested/b":        "bb",
		"nested/link":     "->../top.txt",
		"assets/readonly": "/",
	})
	require.NoError(t, os.Chmod(filepath.Join(root, "assets", "readonly"), 0o555))
	require.NoError(t, os.Chmod(filepath.Join(root, "top.txt"), 0o600))
	t.Cleanup(func() {
		// A 0555 directory would otherwise defeat the temp-dir cleanup.
		_ = os.Chmod(filepath.Join(root, "assets", "readonly"), 0o755)
	})

	src, err := ScanSource(root)
	require.NoError(t, err)

	require.Equal(t, "assets,assets/readonly,nested,nested/deep", joinDirs(src.Directories))
	require.Equal(t, "empty.txt,nested/b,nested/deep/a,top.txt", joinFiles(src.Files))
	require.Len(t, src.Symlinks, 1)
	require.Equal(t, "nested/link", src.Symlinks[0].Path)
	require.Equal(t, "../top.txt", src.Symlinks[0].Target)
	require.Equal(t, SymlinkMode, src.Symlinks[0].Mode)
	require.Equal(t, uint64(10), src.TotalBytes)

	require.Equal(t, uint64(5), fileByPath(t, src, "top.txt").Size)
	require.Equal(t, uint64(0), fileByPath(t, src, "empty.txt").Size)
	if runtime.GOOS != "windows" {
		require.Equal(t, uint16(0o600), fileByPath(t, src, "top.txt").Mode)
		require.Equal(t, uint16(0o555), dirByPath(t, src, "assets/readonly").Mode)
	}
}

// TestScanSourceRecordsSpecialModeBits checks that setuid, setgid, and sticky
// survive the scan. They live outside Go's portable permission bits, so a mode
// read as Perm() alone would drop them and publish a tree that no longer
// behaves like the source.
func TestScanSourceRecordsSpecialModeBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no setuid, setgid, or sticky bits on Windows")
	}
	root := writeTree(t, map[string]string{"tool": "binary", "shared": "data"})
	// Go spells these as its own portable flags, not as the octal bits the
	// manifest records; converting between the two is what entryMode does.
	require.NoError(t, os.Chmod(filepath.Join(root, "tool"), 0o755|fs.ModeSetuid))
	require.NoError(t, os.Chmod(filepath.Join(root, "shared"), 0o644|fs.ModeSetgid|fs.ModeSticky))

	src, err := ScanSource(root)
	require.NoError(t, err)
	require.Equal(t, uint16(0o4755), fileByPath(t, src, "tool").Mode)
	require.Equal(t, uint16(0o3644), fileByPath(t, src, "shared").Mode)
}

// TestScanSourceFollowsRootSymlink covers a source directory reached through a
// link, which a plain walk would report as a single non-directory entry and
// push as nothing at all.
func TestScanSourceFollowsRootSymlink(t *testing.T) {
	real := writeTree(t, map[string]string{"a.txt": "content"})
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	src, err := ScanSource(link)
	require.NoError(t, err)
	require.Len(t, src.Files, 1)
	require.Equal(t, "a.txt", src.Files[0].Path)
}

// TestScanSourceDoesNotFollowSymlinks checks that a link to a directory is
// recorded as a link rather than walked into, which would duplicate the whole
// subtree and, for a link pointing at an ancestor, never terminate.
func TestScanSourceDoesNotFollowSymlinks(t *testing.T) {
	root := writeTree(t, map[string]string{
		"real/inner.txt": "content",
		"alias":          "->real",
	})

	src, err := ScanSource(root)
	require.NoError(t, err)
	require.Equal(t, "real/inner.txt", joinFiles(src.Files))
	require.Equal(t, "real", joinDirs(src.Directories))
	require.Len(t, src.Symlinks, 1)
	require.Equal(t, "real", src.Symlinks[0].Target)
}

func TestScanSourceRejectsCaseCollision(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "Readme.md"), []byte("a"), 0o644))
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("b"), 0o644); err != nil {
		t.Skipf("cannot create the second file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Readme.md")); err != nil {
		t.Skip("filesystem is case-insensitive, so only one file exists")
	}
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	if len(entries) != 2 {
		t.Skip("filesystem is case-insensitive, so only one file exists")
	}

	_, err = ScanSource(root)
	require.Error(t, err)
	require.Contains(t, err.Error(), "differ only in case")
}

func TestScanSourceRejectsUnsupportedEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mkfifo on Windows")
	}
	// A unix socket is the one special file a test can create without
	// privileges or a build-tagged syscall; the scan rejects it for the same
	// reason it rejects a fifo or a device node.
	root, err := os.MkdirTemp("", "vol")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	listener, err := net.Listen("unix", filepath.Join(root, "s"))
	if err != nil {
		t.Skipf("cannot create a unix socket: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	_, err = ScanSource(root)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot describe")
}

func TestScanSourceRejectsNonDirectoryRoot(t *testing.T) {
	root := writeTree(t, map[string]string{"a.txt": "content"})

	_, err := ScanSource(filepath.Join(root, "a.txt"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a directory")

	_, err = ScanSource(filepath.Join(root, "missing"))
	require.Error(t, err)
}

// TestScanSourceRejectsBackslashName covers a filename a Windows puller could
// not reproduce. The reference client rewrites the backslash to a slash and
// publishes a tree that does not match the source; this one refuses.
func TestScanSourceRejectsBackslashName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a backslash is a separator, not a filename character")
	}
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, `od\d`), []byte("x"), 0o644))

	_, err := ScanSource(root)
	require.Error(t, err)
	require.Contains(t, err.Error(), "backslash")
}

func TestScanSourceSynthesizesMissingAncestors(t *testing.T) {
	dirs := map[string]uint16{}
	addAncestors(dirs, "a/b/c/file.txt")
	require.Equal(t, 3, len(dirs))
	for _, path := range []string{"a", "a/b", "a/b/c"} {
		require.MapEqual(t, dirs, path, DefaultDirMode)
	}

	// A directory the walk already recorded keeps its real mode.
	dirs = map[string]uint16{"a": 0o700}
	addAncestors(dirs, "a/b/file.txt")
	require.MapEqual(t, dirs, "a", uint16(0o700))
	require.MapEqual(t, dirs, "a/b", DefaultDirMode)
}

func TestSourceURIForDir(t *testing.T) {
	uri, err := SourceURIForDir(t.TempDir())
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(uri, "file://"), "uri %q should start with file://", uri)
	require.False(t, strings.Contains(uri, `\`), "uri %q should be slash-separated", uri)
}

func TestNewManifest(t *testing.T) {
	src := &Source{
		Directories: []DirectoryEntry{{Path: "dir", Mode: 0o755}},
		Symlinks:    []SymlinkEntry{{Path: "link", Target: "dir", Mode: SymlinkMode}},
	}
	files := []FileEntry{{Path: "dir/f", Mode: 0o644, Kind: FileKindChunk}}

	m := NewManifest(src, "file:///tmp/tree", files)
	require.Equal(t, ProvenanceFingerprint, m.Provenance.SourceFingerprint)
	require.Equal(t, ProvenanceFingerprintType, m.Provenance.SourceFingerprintType)
	require.Equal(t, "file:///tmp/tree", m.Provenance.SourceURI)
	require.Equal(t, uint64(3), m.EntryCount())

	// The manifest owns its entry slices, so the encoder's in-place sort
	// cannot reorder the scan a caller is still holding.
	m.Directories[0].Path = "other"
	require.Equal(t, "dir", src.Directories[0].Path)
}

func joinDirs(dirs []DirectoryEntry) string {
	paths := make([]string, len(dirs))
	for i, d := range dirs {
		paths[i] = d.Path
	}
	return strings.Join(paths, ",")
}

func joinFiles(files []SourceFile) string {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	return strings.Join(paths, ",")
}

func fileByPath(t *testing.T, src *Source, path string) SourceFile {
	t.Helper()
	for _, f := range src.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no file %q in %s", path, joinFiles(src.Files))
	return SourceFile{}
}

func dirByPath(t *testing.T, src *Source, path string) DirectoryEntry {
	t.Helper()
	for _, d := range src.Directories {
		if d.Path == path {
			return d
		}
	}
	t.Fatalf("no directory %q in %s", path, joinDirs(src.Directories))
	return DirectoryEntry{}
}
