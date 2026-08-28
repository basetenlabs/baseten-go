package modelarchive_test

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/basetenlabs/baseten-go/client/modelarchive"
	"github.com/basetenlabs/baseten-go/internal/require"
)

type tarEntry struct {
	name     string
	data     string
	typeflag byte
	linkname string
}

func readArchive(t *testing.T, rc io.ReadCloser) []tarEntry {
	t.Helper()
	defer rc.Close()
	tr := tar.NewReader(rc)
	var entries []tarEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		buf, err := io.ReadAll(tr)
		require.NoError(t, err)
		entries = append(entries, tarEntry{
			name:     hdr.Name,
			data:     string(buf),
			typeflag: hdr.Typeflag,
			linkname: hdr.Linkname,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries
}

func entryNames(entries []tarEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.name
	}
	return names
}

func entryByName(t *testing.T, entries []tarEntry, name string) tarEntry {
	t.Helper()
	for _, e := range entries {
		if e.name == name {
			return e
		}
	}
	t.Fatalf("no entry named %q in %v", name, entryNames(entries))
	return tarEntry{}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
}

func TestBuildModelArchiveBasicWalk(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: foo\n")
	writeFile(t, filepath.Join(dir, "model", "model.py"), "print('hi')\n")
	writeFile(t, filepath.Join(dir, "data", "weights.bin"), "WEIGHTS")

	rc, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{Dir: dir})
	require.NoError(t, err)
	entries := readArchive(t, rc)

	require.Equal(t, 3, len(entries))
	require.Equal(t, "config.yaml", entries[0].name)
	require.Equal(t, "model_name: foo\n", entries[0].data)
	require.Equal(t, "data/weights.bin", entries[1].name)
	require.Equal(t, "WEIGHTS", entries[1].data)
	require.Equal(t, "model/model.py", entries[2].name)
}

func TestBuildModelArchiveConfigOverride(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: on_disk\n")
	writeFile(t, filepath.Join(dir, "model", "model.py"), "x\n")

	rc, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{
		Dir:                dir,
		ConfigYAMLOverride: []byte("model_name: in_memory\n"),
	})
	require.NoError(t, err)
	entries := readArchive(t, rc)

	require.Equal(t, 2, len(entries))
	require.Equal(t, "config.yaml", entries[0].name)
	require.Equal(t, "model_name: in_memory\n", entries[0].data)
}

func TestBuildModelArchiveOverrideWhenConfigMissingOnDisk(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "model", "model.py"), "x\n")

	rc, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{
		Dir:                dir,
		ConfigYAMLOverride: []byte("model_name: synth\n"),
	})
	require.NoError(t, err)
	entries := readArchive(t, rc)

	require.Equal(t, 2, len(entries))
	require.Equal(t, "config.yaml", entries[0].name)
	require.Equal(t, "model_name: synth\n", entries[0].data)
	require.Equal(t, "model/model.py", entries[1].name)
}

func TestBuildModelArchiveNoConfigOnDiskIsFine(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "model", "model.py"), "x\n")

	rc, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{Dir: dir})
	require.NoError(t, err)
	names := entryNames(readArchive(t, rc))
	require.Equal(t, "model/model.py", strings.Join(names, ","))
}

func TestBuildModelArchiveExternalPackageDirs(t *testing.T) {
	root := t.TempDir()
	trussDir := filepath.Join(root, "truss")
	extDir := filepath.Join(root, "shared_pkg")

	writeFile(t, filepath.Join(trussDir, "config.yaml"), "model_name: ext\n")
	writeFile(t, filepath.Join(trussDir, "model", "model.py"), "M\n")
	writeFile(t, filepath.Join(extDir, "shared.py"), "S\n")
	writeFile(t, filepath.Join(extDir, "sub", "x.py"), "X\n")

	rc, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{
		Dir:                 trussDir,
		ExternalPackageDirs: []string{"../shared_pkg"},
		BundledPackagesDir:  "packages",
	})
	require.NoError(t, err)
	names := entryNames(readArchive(t, rc))

	// External package contents land under packages/ directly (basename of
	// external dir is NOT preserved).
	want := []string{"config.yaml", "model/model.py", "packages/shared.py", "packages/sub/x.py"}
	require.Equal(t, strings.Join(want, ","), strings.Join(names, ","))
}

func TestBuildModelArchiveCustomBundledPackagesDir(t *testing.T) {
	root := t.TempDir()
	trussDir := filepath.Join(root, "truss")
	extDir := filepath.Join(root, "shared_pkg")

	writeFile(t, filepath.Join(trussDir, "config.yaml"), "model_name: ext\n")
	writeFile(t, filepath.Join(extDir, "shared.py"), "S\n")

	rc, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{
		Dir:                 trussDir,
		ExternalPackageDirs: []string{"../shared_pkg"},
		BundledPackagesDir:  "vendored",
	})
	require.NoError(t, err)
	names := entryNames(readArchive(t, rc))
	require.Equal(t, "config.yaml,vendored/shared.py", strings.Join(names, ","))
}

func TestBuildModelArchiveExternalDirWithoutBundledDirErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: x\n")

	_, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{
		Dir:                 dir,
		ExternalPackageDirs: []string{"../nope"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "BundledPackagesDir")
}

func TestBuildModelArchiveMissingExternalPackageDirErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: ext\n")

	// A missing external package dir is a precondition, so it fails before any
	// of the archive is produced.
	_, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{
		Dir:                 dir,
		ExternalPackageDirs: []string{"../does_not_exist"},
		BundledPackagesDir:  "packages",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "external package dir")
}

func TestBuildModelArchiveDuplicateArchivePathErrors(t *testing.T) {
	root := t.TempDir()
	trussDir := filepath.Join(root, "truss")
	extDir := filepath.Join(root, "shared_pkg")

	writeFile(t, filepath.Join(trussDir, "config.yaml"), "model_name: dup\n")
	// truss-side file at packages/conflict.py collides with ext-side
	// shared.py landing at packages/conflict.py.
	writeFile(t, filepath.Join(trussDir, "packages", "conflict.py"), "T\n")
	writeFile(t, filepath.Join(extDir, "conflict.py"), "E\n")

	// The collision is found while enumerating, so it fails the build outright
	// rather than surfacing partway through a read.
	_, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{
		Dir:                 trussDir,
		ExternalPackageDirs: []string{"../shared_pkg"},
		BundledPackagesDir:  "packages",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate archive entry")
	// The message names both colliding source files and the remediation.
	require.Contains(t, err.Error(), filepath.Join(trussDir, "packages", "conflict.py"))
	require.Contains(t, err.Error(), filepath.Join(extDir, "conflict.py"))
	require.Contains(t, err.Error(), "Rename or remove one")
}

func TestBuildModelArchiveDefaultIgnoreApplied(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: x\n")
	writeFile(t, filepath.Join(dir, "model", "model.py"), "ok\n")
	writeFile(t, filepath.Join(dir, "model", "model.pyc"), "binary")
	writeFile(t, filepath.Join(dir, "__pycache__", "cached.pyc"), "binary")
	writeFile(t, filepath.Join(dir, ".DS_Store"), "junk")
	writeFile(t, filepath.Join(dir, ".hypothesis", "db.sqlite3"), "junk")
	writeFile(t, filepath.Join(dir, "docs", "_build", "html", "index.html"), "junk")
	writeFile(t, filepath.Join(dir, "share", "python-wheels", "x.whl"), "junk")
	// Path-anchored patterns must not match outside their parent: a top-level
	// _build/ or python-wheels/ should still ship.
	writeFile(t, filepath.Join(dir, "_build", "keep.txt"), "keep")
	writeFile(t, filepath.Join(dir, "python-wheels", "keep.txt"), "keep")

	rc, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{Dir: dir})
	require.NoError(t, err)
	names := entryNames(readArchive(t, rc))
	require.Equal(t, "_build/keep.txt,config.yaml,model/model.py,python-wheels/keep.txt", strings.Join(names, ","))
}

func TestBuildModelArchiveIgnoreFileOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: x\n")
	writeFile(t, filepath.Join(dir, "keep.pyc"), "kept-because-defaults-skipped")
	writeFile(t, filepath.Join(dir, "drop.txt"), "dropped-by-processor")
	writeFile(t, filepath.Join(dir, ".truss_ignore"), "drop.txt\n")

	var processorCalled bool
	opts := modelarchive.BuildModelArchiveOptions{Dir: dir}
	opts.IgnoreFileProcessor = func(_ context.Context, ipo modelarchive.IgnoreFileProcessorOptions) (modelarchive.IgnoreFileFunc, error) {
		processorCalled = true
		patterns := strings.Split(strings.TrimSpace(string(ipo.Contents)), "\n")
		return func(_ context.Context, opts modelarchive.IgnoreFileOptions) (bool, error) {
			for _, p := range patterns {
				if p == opts.RelPath {
					return true, nil
				}
			}
			return false, nil
		}, nil
	}

	rc, err := modelarchive.BuildModelArchive(context.Background(), opts)
	require.NoError(t, err)
	names := entryNames(readArchive(t, rc))
	require.True(t, processorCalled, "IgnoreFileProcessor should have been invoked")
	// keep.pyc would normally be ignored by DefaultIgnoreFile but the custom
	// processor replaces it entirely; .truss_ignore itself is ignored by the
	// processor's pattern (not listed → not ignored).
	want := []string{".truss_ignore", "config.yaml", "keep.pyc"}
	require.Equal(t, strings.Join(want, ","), strings.Join(names, ","))
}

func TestBuildModelArchiveMissingProcessorErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: x\n")
	writeFile(t, filepath.Join(dir, ".truss_ignore"), "*.log\n")

	_, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{Dir: dir})
	require.Error(t, err)
	require.Contains(t, err.Error(), "IgnoreFileProcessor is nil")
}

// symlinksSupported reports whether this process can create a symlink at all.
// On Windows that depends on the account's privileges and on developer mode
// rather than on the OS alone, so it is probed once rather than assumed.
var symlinksSupported = sync.OnceValue(func() bool {
	dir, err := os.MkdirTemp("", "symlink-probe")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	return os.Symlink("target", filepath.Join(dir, "probe")) == nil
})

func skipWithoutSymlinks(t *testing.T) {
	t.Helper()
	if !symlinksSupported() {
		t.Skip("this process cannot create symlinks")
	}
}

func TestBuildModelArchiveSymlinkStoredNotFollowed(t *testing.T) {
	skipWithoutSymlinks(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: x\n")
	writeFile(t, filepath.Join(dir, "real.txt"), "real")
	writeFile(t, filepath.Join(dir, "model", "model.py"), "print('hi')\n")
	require.NoError(t, os.Symlink("real.txt", filepath.Join(dir, "link.txt")))
	// A target reached through ".." is fine as long as it lands back inside.
	require.NoError(t, os.Symlink(filepath.Join("..", "real.txt"), filepath.Join(dir, "model", "up.txt")))
	// A directory symlink is stored as a link, so its contents are not walked
	// and appear once, under their real path.
	require.NoError(t, os.Symlink("model", filepath.Join(dir, "model_link")))

	rc, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{Dir: dir})
	require.NoError(t, err)
	entries := readArchive(t, rc)

	require.Equal(t, "config.yaml,link.txt,model/model.py,model/up.txt,model_link,real.txt",
		strings.Join(entryNames(entries), ","))
	for _, tc := range []struct{ name, linkname string }{
		{"link.txt", "real.txt"},
		{"model/up.txt", filepath.Join("..", "real.txt")},
		{"model_link", "model"},
	} {
		entry := entryByName(t, entries, tc.name)
		require.Equal(t, byte(tar.TypeSymlink), entry.typeflag)
		require.Equal(t, tc.linkname, entry.linkname)
		require.Equal(t, "", entry.data)
	}
}

func TestBuildModelArchiveSymlinkOutsideArchiveRejected(t *testing.T) {
	skipWithoutSymlinks(t)
	root := t.TempDir()
	outside := filepath.Join(root, "outside.txt")
	writeFile(t, outside, "secret")

	for _, tc := range []struct {
		name   string
		at     string
		target string
	}{
		{"absolute", "link.txt", outside},
		{"escaping", "link.txt", filepath.Join("..", "outside.txt")},
		{"escaping from a subdirectory", "model/link.txt", filepath.Join("..", "..", "outside.txt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "truss")
			writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: x\n")
			linkPath := filepath.Join(dir, filepath.FromSlash(tc.at))
			require.NoError(t, os.MkdirAll(filepath.Dir(linkPath), 0o755))
			require.NoError(t, os.Symlink(tc.target, linkPath))

			// Enumeration happens up front, so this fails before a caller can
			// start uploading rather than partway through the stream.
			_, err := modelarchive.BuildModelArchive(context.Background(),
				modelarchive.BuildModelArchiveOptions{Dir: dir})
			require.Error(t, err)

			var invalid *modelarchive.InvalidSymlinkError
			require.True(t, errors.As(err, &invalid), "expected InvalidSymlinkError, got %v", err)
			require.Equal(t, tc.at, invalid.ArchivePath)
			require.Equal(t, tc.target, invalid.Target)
			require.Contains(t, err.Error(), ".truss_ignore")
		})
	}
}

func TestBuildModelArchiveIgnoredSymlinkNotRejected(t *testing.T) {
	skipWithoutSymlinks(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: x\n")
	writeFile(t, filepath.Join(dir, ".truss_ignore"), ".devenv/\n")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".devenv", "gc"), 0o755))
	require.NoError(t, os.Symlink("/nix/store/whatever", filepath.Join(dir, ".devenv", "gc", "shell")))

	// The ignore rules are applied before the entry is classified, so ignoring
	// an unarchivable symlink is a way out of the error.
	rc, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{
		Dir: dir,
		IgnoreFileProcessor: func(_ context.Context, opts modelarchive.IgnoreFileProcessorOptions) (modelarchive.IgnoreFileFunc, error) {
			return func(_ context.Context, e modelarchive.IgnoreFileOptions) (bool, error) {
				return strings.HasPrefix(e.RelPath, ".devenv"), nil
			}, nil
		},
	})
	require.NoError(t, err)
	require.Equal(t, ".truss_ignore,config.yaml", strings.Join(entryNames(readArchive(t, rc)), ","))
}

func TestBuildModelArchiveIrregularFileSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not an ordinary filesystem entry on windows")
	}
	// Not t.TempDir: a socket path has to fit in sun_path, roughly 100 bytes,
	// and the per-test temp dir alone is longer than that on macOS.
	dir, err := os.MkdirTemp("/tmp", "ma")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: x\n")
	socket, err := net.Listen("unix", filepath.Join(dir, "sock"))
	require.NoError(t, err)
	defer socket.Close()

	rc, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{Dir: dir})
	require.NoError(t, err)
	require.Equal(t, "config.yaml", strings.Join(entryNames(readArchive(t, rc)), ","))
}

func TestBuildModelArchiveMissingDirErrors(t *testing.T) {
	_, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{
		Dir: filepath.Join(t.TempDir(), "nope"),
	})
	require.Error(t, err)
}

func TestBuildModelArchiveContextCanceled(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: x\n")
	writeFile(t, filepath.Join(dir, "a.txt"), "a")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := modelarchive.BuildModelArchive(ctx, modelarchive.BuildModelArchiveOptions{Dir: dir})
	require.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got %v", err)
}

// walkFile is a File flattened for comparison: the contents are read through
// Open so a walk can be checked against what a build would archive.
type walkFile struct {
	archivePath string
	sourcePath  string
	size        int64
	data        string
	linkTarget  string
	isDir       bool
}

func walkFiles(t *testing.T, opts modelarchive.BuildModelArchiveOptions) []walkFile {
	t.Helper()
	var files []walkFile
	require.NoError(t, modelarchive.WalkModelArchive(context.Background(), opts, func(f modelarchive.File) error {
		rc, err := f.Open()
		require.NoError(t, err)
		defer rc.Close()
		data, err := io.ReadAll(rc)
		require.NoError(t, err)
		files = append(files, walkFile{
			archivePath: f.ArchivePath,
			sourcePath:  f.SourcePath,
			size:        f.Size,
			data:        string(data),
			linkTarget:  f.LinkTarget,
			isDir:       f.Info != nil && f.Info.IsDir(),
		})
		return nil
	}))
	return files
}

func walkPaths(files []walkFile) []string {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.archivePath
	}
	return paths
}

func TestWalkModelArchiveMatchesBuild(t *testing.T) {
	trussDir := filepath.Join(t.TempDir(), "truss")
	extDir := filepath.Join(t.TempDir(), "shared_pkg")
	writeFile(t, filepath.Join(trussDir, "config.yaml"), "model_name: walk\n")
	writeFile(t, filepath.Join(trussDir, "model", "model.py"), "M\n")
	writeFile(t, filepath.Join(trussDir, "__pycache__", "junk.pyc"), "IGNORED")
	writeFile(t, filepath.Join(extDir, "shared.py"), "S\n")
	opts := modelarchive.BuildModelArchiveOptions{
		Dir:                 trussDir,
		ConfigYAMLOverride:  []byte("model_name: override\n"),
		ExternalPackageDirs: []string{extDir},
		BundledPackagesDir:  "packages",
	}

	// The walk is what the build runs on, so the two must agree on every path
	// and every byte, including the synthesized config and the ignore rules.
	files := walkFiles(t, opts)
	var walked []tarEntry
	for _, f := range files {
		walked = append(walked, tarEntry{name: f.archivePath, data: f.data})
	}
	sort.Slice(walked, func(i, j int) bool { return walked[i].name < walked[j].name })

	rc, err := modelarchive.BuildModelArchive(context.Background(), opts)
	require.NoError(t, err)
	built := readArchive(t, rc)

	require.Equal(t, strings.Join(entryNames(built), ","), strings.Join(entryNames(walked), ","))
	require.Equal(t, "config.yaml,model/model.py,packages/shared.py", strings.Join(entryNames(walked), ","))
	require.Len(t, built, len(walked))
	for i := range built {
		require.Equal(t, built[i].data, walked[i].data)
	}
}

func TestWalkModelArchiveFileFields(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: fields\n")
	writeFile(t, filepath.Join(dir, "model.py"), "M\n")

	override := "model_name: override\n"
	files := walkFiles(t, modelarchive.BuildModelArchiveOptions{
		Dir:                dir,
		ConfigYAMLOverride: []byte(override),
	})
	require.Len(t, files, 2)

	// The override is synthesized, so it has no source path and no file info,
	// but still reports a size and opens like any other entry.
	require.Equal(t, "config.yaml", files[0].archivePath)
	require.Equal(t, "", files[0].sourcePath)
	require.Equal(t, int64(len(override)), files[0].size)
	require.Equal(t, override, files[0].data)

	require.Equal(t, "model.py", files[1].archivePath)
	require.Equal(t, filepath.Join(dir, "model.py"), files[1].sourcePath)
	require.Equal(t, int64(2), files[1].size)
}

func TestWalkModelArchiveErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: x\n")
	writeFile(t, filepath.Join(dir, "a.txt"), "a")

	sentinel := errors.New("stop walking")
	seen := 0
	err := modelarchive.WalkModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{Dir: dir},
		func(modelarchive.File) error {
			seen++
			return sentinel
		})
	require.True(t, errors.Is(err, sentinel), "expected the sentinel, got %v", err)
	require.Equal(t, 1, seen)
}

func TestWalkModelArchiveValidates(t *testing.T) {
	err := modelarchive.WalkModelArchive(context.Background(),
		modelarchive.BuildModelArchiveOptions{Dir: filepath.Join(t.TempDir(), "nope")},
		func(modelarchive.File) error { return nil })
	require.Error(t, err)
}

func TestWalkModelArchiveRejectsSymlinkOutsideArchive(t *testing.T) {
	skipWithoutSymlinks(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: x\n")
	require.NoError(t, os.Symlink("/etc/hosts", filepath.Join(dir, "link.txt")))

	// A walker gets the same rejection a build does, so walking a tree is also
	// a check that it can be pushed.
	seen := 0
	err := modelarchive.WalkModelArchive(context.Background(),
		modelarchive.BuildModelArchiveOptions{Dir: dir},
		func(modelarchive.File) error {
			seen++
			return nil
		})
	var invalid *modelarchive.InvalidSymlinkError
	require.True(t, errors.As(err, &invalid), "expected InvalidSymlinkError, got %v", err)
	require.Equal(t, "link.txt", invalid.ArchivePath)
	// The offending entry is never reported, only the config that preceded it.
	require.Equal(t, 1, seen)
}

func TestWalkModelArchiveSymlinkFields(t *testing.T) {
	skipWithoutSymlinks(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: x\n")
	writeFile(t, filepath.Join(dir, "real.txt"), "real")
	require.NoError(t, os.Symlink("real.txt", filepath.Join(dir, "link.txt")))

	files := walkFiles(t, modelarchive.BuildModelArchiveOptions{Dir: dir})
	require.Equal(t, "config.yaml,link.txt,real.txt", strings.Join(walkPaths(files), ","))

	// A symlink carries its validated target rather than the target's bytes,
	// so a consumer never has to re-read the link to learn where it points.
	link := files[1]
	require.Equal(t, "real.txt", link.linkTarget)
	require.Equal(t, int64(0), link.size)
	require.Equal(t, "", link.data)
	require.Equal(t, "", files[2].linkTarget)
}

func TestWalkModelArchiveSharedExternalDirs(t *testing.T) {
	root := t.TempDir()
	trussDir := filepath.Join(root, "truss")
	writeFile(t, filepath.Join(trussDir, "config.yaml"), "model_name: x\n")
	writeFile(t, filepath.Join(root, "a", "sub", "a.py"), "A\n")
	writeFile(t, filepath.Join(root, "b", "sub", "b.py"), "B\n")

	// Two external dirs contributing the same directory is normal, so it is
	// reported once rather than colliding.
	files := walkFiles(t, modelarchive.BuildModelArchiveOptions{
		Dir:                 trussDir,
		ExternalPackageDirs: []string{"../a", "../b"},
		BundledPackagesDir:  "packages",
		IncludeDirsInWalk:   true,
	})
	require.Equal(t, "config.yaml,packages/sub,packages/sub/a.py,packages/sub/b.py",
		strings.Join(walkPaths(files), ","))

	// A file and a directory landing on one path is still a collision.
	writeFile(t, filepath.Join(root, "c", "sub"), "not a dir\n")
	err := modelarchive.WalkModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{
		Dir:                 trussDir,
		ExternalPackageDirs: []string{"../a", "../c"},
		BundledPackagesDir:  "packages",
		IncludeDirsInWalk:   true,
	}, func(modelarchive.File) error { return nil })
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate archive entry")
}

func TestWalkModelArchiveIncludeDirsInWalk(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: x\n")
	writeFile(t, filepath.Join(dir, "model", "model.py"), "M\n")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "empty"), 0o755))

	// Without the option the walk is exactly the archive's members, which is
	// why an empty directory is invisible.
	files := walkFiles(t, modelarchive.BuildModelArchiveOptions{Dir: dir})
	require.Equal(t, "config.yaml,model/model.py", strings.Join(walkPaths(files), ","))

	files = walkFiles(t, modelarchive.BuildModelArchiveOptions{Dir: dir, IncludeDirsInWalk: true})
	require.Equal(t, "config.yaml,empty,model,model/model.py", strings.Join(walkPaths(files), ","))
	for _, f := range files {
		require.Equal(t, f.archivePath == "empty" || f.archivePath == "model", f.isDir)
	}

	// Directory entries never reach the archive itself.
	rc, err := modelarchive.BuildModelArchive(context.Background(),
		modelarchive.BuildModelArchiveOptions{Dir: dir, IncludeDirsInWalk: true})
	require.NoError(t, err)
	require.Equal(t, "config.yaml,model/model.py", strings.Join(entryNames(readArchive(t, rc)), ","))
}
