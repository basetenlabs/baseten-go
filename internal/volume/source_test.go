package volume

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
// not reproduce. Rewriting the backslash to a slash would publish a tree that
// does not match the source, so the scan refuses instead.
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
	dirs := map[string]DirectoryEntry{}
	addAncestors(dirs, "a/b/c/file.txt")
	require.Equal(t, 3, len(dirs))
	for _, path := range []string{"a", "a/b", "a/b/c"} {
		require.Equal(t, DirectoryEntry{Path: path, Mode: DefaultDirMode}, dirs[path])
		// The synthesized guard entry carries no time: the encoder omits the
		// key rather than asserting a time nothing measured.
		require.True(t, dirs[path].MTime.IsZero(), "synthesized ancestor %s gained a time", path)
	}

	// A directory the walk already recorded keeps its real mode and time.
	recorded := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	dirs = map[string]DirectoryEntry{"a": {Path: "a", Mode: 0o700, MTime: recorded}}
	addAncestors(dirs, "a/b/file.txt")
	require.Equal(t, DirectoryEntry{Path: "a", Mode: 0o700, MTime: recorded}, dirs["a"])
	require.Equal(t, DirectoryEntry{Path: "a/b", Mode: DefaultDirMode}, dirs["a/b"])
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

// TestNewManifestOwnsItsSlices pins that the manifest copies what it is
// given, so a caller reusing or reordering its own slice afterwards cannot
// change what a later encode produces.
func TestNewManifestOwnsItsSlices(t *testing.T) {
	src := &Source{
		Directories: []DirectoryEntry{{Path: "d", Mode: 0o755}},
		Symlinks:    []SymlinkEntry{{Path: "l", Target: "d"}},
	}
	files := []FileEntry{
		{Path: "b", Mode: 0o644, Kind: FileKindChunk},
		{Path: "a", Mode: 0o644, Kind: FileKindChunk},
	}
	m := NewManifest(src, "file:///s", files)
	before := string(EncodeManifest(m))

	// Everything the caller still holds is rearranged and rewritten.
	files[0], files[1] = files[1], files[0]
	files[0].Path = "clobbered"
	src.Directories[0].Path = "clobbered"
	src.Symlinks[0].Path = "clobbered"

	require.Equal(t, before, string(EncodeManifest(m)))
}

// TestFileURIIsUnchangedForUnixPaths is the byte-identity gate on the URI
// construction. The URI is inside the digest, so any change to what a
// slash-rooted path produces would invalidate every manifest already made
// from one. The helper is pure and takes an already-slashed path, so this
// holds on every platform rather than only where the test happens to run.
func TestFileURIIsUnchangedForUnixPaths(t *testing.T) {
	for _, path := range []string{"/", "/tmp/x", "/tmp/bdn-volume-fixture/tree", "/a b/c#d"} {
		require.Equal(t, "file://"+path, fileURI(path))
	}
	// The windows shape is the one that changes, and the drive moves out of
	// the authority where it never belonged.
	require.Equal(t, "file:///C:/data/v", fileURI("C:/data/v"))
}

// TestModeFromManifestRoundTripsTheSpecialBits is the primary evidence for
// the translation, and it is deliberately a pure one: it depends on no
// filesystem, so it cannot be skipped or flake on a mount with opinions.
//
// The asymmetry it covers is why the bug it guards was invisible: the encode
// side had a test pinning all three bits, and the decode side had none.
func TestModeFromManifestRoundTripsTheSpecialBits(t *testing.T) {
	for _, tc := range []struct {
		recorded uint16
		want     fs.FileMode
	}{
		{0o644, 0o644},
		{0o755, 0o755},
		{0o4755, 0o755 | fs.ModeSetuid},
		{0o2755, 0o755 | fs.ModeSetgid},
		{0o1777, 0o777 | fs.ModeSticky},
		{0o7000, fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky},
	} {
		require.Equal(t, tc.want, ModeFromManifest(tc.recorded))
	}

	// Every mode the scanner records must survive the trip back unchanged,
	// which is the property the two halves together are supposed to have.
	for _, mode := range []fs.FileMode{
		0o644, 0o755, 0o600, 0o444,
		0o755 | fs.ModeSetuid, 0o755 | fs.ModeSetgid, 0o777 | fs.ModeSticky,
		0o770 | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky,
	} {
		recorded := uint16(mode.Perm()) | specialBits(mode)
		require.Equal(t, mode, ModeFromManifest(recorded))
	}

	// Anything above the recorded set is dropped, so a mode can never become
	// a file type.
	require.Equal(t, fs.FileMode(0o755), ModeFromManifest(0o40755))
}

// TestScanSourceRecordsModificationTimes pins where each entry's time comes
// from: the walk's own lstat, so a symlink carries the link's time and never
// the target's.
func TestScanSourceRecordsModificationTimes(t *testing.T) {
	root := writeTree(t, map[string]string{
		"dir/file.txt": "content",
		"link":         "->dir/file.txt",
	})
	fileTime := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	dirTime := time.Date(2023, 1, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, os.Chtimes(filepath.Join(root, "dir", "file.txt"), fileTime, fileTime))
	require.NoError(t, os.Chtimes(filepath.Join(root, "dir"), dirTime, dirTime))

	src, err := ScanSource(root)
	require.NoError(t, err)

	got := fileByPath(t, src, "dir/file.txt").MTime
	require.True(t, got.Equal(fileTime), "file mtime %v, want %v", got, fileTime)
	got = dirByPath(t, src, "dir").MTime
	require.True(t, got.Equal(dirTime), "dir mtime %v, want %v", got, dirTime)

	linkInfo, err := os.Lstat(filepath.Join(root, "link"))
	require.NoError(t, err)
	require.Len(t, src.Symlinks, 1)
	require.False(t, src.Symlinks[0].MTime.IsZero(), "the link's own time was not recorded")
	require.True(t, src.Symlinks[0].MTime.Equal(clampMTime(linkInfo.ModTime())),
		"link mtime %v, want the link's own lstat time %v", src.Symlinks[0].MTime, linkInfo.ModTime())
}

// TestScanIsStableAcrossReads pins that two scans of an untouched tree record
// identical modification times — the property a time-bearing manifest digest
// stands on. The tree's times are natural, deliberately: the instability this
// guards against was two scans reading different times for files nothing
// touched, from the directory enumeration's lazily-updated metadata copy on
// Windows, and a test that set its own times would never look at the cache
// this test exists to bypass. On Windows this is a CANARY, not a proof: it
// passes deterministically with the fix, while a revert to the cached read
// fails only when the lazy update happens to land between its scans.
func TestScanIsStableAcrossReads(t *testing.T) {
	root := writeTree(t, map[string]string{
		"dir/file.txt": "content",
		"dir/sub/a":    "aaa",
		"link":         "->dir/file.txt",
	})

	first, err := ScanSource(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ScanSource(root)
	if err != nil {
		t.Fatal(err)
	}

	for i := range first.Files {
		if !first.Files[i].MTime.Equal(second.Files[i].MTime) {
			t.Errorf("file %s recorded %v then %v across two scans of an untouched tree",
				first.Files[i].Path, first.Files[i].MTime, second.Files[i].MTime)
		}
	}
	for i := range first.Directories {
		if !first.Directories[i].MTime.Equal(second.Directories[i].MTime) {
			t.Errorf("directory %s recorded %v then %v across two scans of an untouched tree",
				first.Directories[i].Path, first.Directories[i].MTime, second.Directories[i].MTime)
		}
	}
	for i := range first.Symlinks {
		if !first.Symlinks[i].MTime.Equal(second.Symlinks[i].MTime) {
			t.Errorf("symlink %s recorded %v then %v across two scans of an untouched tree",
				first.Symlinks[i].Path, first.Symlinks[i].MTime, second.Symlinks[i].MTime)
		}
	}
	// The encoded form is the property callers actually depend on.
	m1 := NewManifest(first, "file:///pin", nil)
	m2 := NewManifest(second, "file:///pin", nil)
	if string(EncodeManifest(m1)) != string(EncodeManifest(m2)) {
		t.Error("two scans of an untouched tree encoded to different bytes")
	}
}

// TestScanRootReadsThroughTheHandle pins what ScanRoot exists for: every read
// goes through the retained handle, so a scan started on one tree keeps
// describing that tree even when the path it was opened by has come to name a
// different one. A pathname-based scan here would describe the impostor, and
// a push holding the original handle would then read files the scan never
// looked at.
func TestScanRootReadsThroughTheHandle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a directory with an open handle cannot be renamed on Windows")
	}
	base := t.TempDir()
	original := filepath.Join(base, "tree")
	require.NoError(t, os.MkdirAll(filepath.Join(original, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(original, "sub", "a.txt"), []byte("original"), 0o644))

	root, err := os.OpenRoot(original)
	require.NoError(t, err)
	defer root.Close()

	// The swap: the opened tree moves aside and a different one takes its
	// pathname, the shape of the race between opening a source and scanning
	// it.
	require.NoError(t, os.Rename(original, filepath.Join(base, "moved")))
	impostor := filepath.Join(base, "tree")
	require.NoError(t, os.MkdirAll(impostor, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(impostor, "impostor.txt"), []byte("other bytes"), 0o644))

	src, err := ScanRoot(root)
	require.NoError(t, err)
	require.Equal(t, "sub/a.txt", joinFiles(src.Files))
	require.Equal(t, "sub", joinDirs(src.Directories))
	require.Equal(t, uint64(8), src.TotalBytes)
}

// TestScanRecordsFileIdentity pins that the identity baseline comes from the
// scan itself — the same Lstat that records the time — and matches what a
// direct Lstat of the file reports.
func TestScanRecordsFileIdentity(t *testing.T) {
	root := writeTree(t, map[string]string{"dir/file.txt": "content"})
	src, err := ScanSource(root)
	if err != nil {
		t.Fatal(err)
	}
	file := fileByPath(t, src, "dir/file.txt")

	info, err := os.Lstat(filepath.Join(root, "dir", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	dev, ino, ok := FileIdentity(info)
	if !ok {
		if file.Dev != 0 || file.Ino != 0 {
			t.Fatalf("no identity on this platform, yet the scan recorded dev=%d ino=%d", file.Dev, file.Ino)
		}
		t.Skip("platform exposes no inode identity; the size re-check alone holds here")
	}
	if file.Dev != dev || file.Ino != ino {
		t.Errorf("scan recorded dev=%d ino=%d, a direct Lstat reports dev=%d ino=%d", file.Dev, file.Ino, dev, ino)
	}
	if file.Ino == 0 {
		t.Error("the recorded inode is zero on a platform that has one")
	}
}
