package modelarchive_test

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/client/modelarchive"
	"github.com/basetenlabs/baseten-go/internal/require"
)

type tarEntry struct {
	name string
	data string
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
		entries = append(entries, tarEntry{name: hdr.Name, data: string(buf)})
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

	rc, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{
		Dir:                 dir,
		ExternalPackageDirs: []string{"../does_not_exist"},
		BundledPackagesDir:  "packages",
	})
	require.NoError(t, err)
	_, err = io.ReadAll(rc)
	rc.Close()
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

	rc, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{
		Dir:                 trussDir,
		ExternalPackageDirs: []string{"../shared_pkg"},
		BundledPackagesDir:  "packages",
	})
	require.NoError(t, err)
	_, err = io.ReadAll(rc)
	rc.Close()
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate archive entry")
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

func TestBuildModelArchiveSymlinkNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require admin on windows")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "model_name: x\n")
	writeFile(t, filepath.Join(dir, "real.txt"), "real")
	require.NoError(t, os.Symlink("real.txt", filepath.Join(dir, "link.txt")))

	rc, err := modelarchive.BuildModelArchive(context.Background(), modelarchive.BuildModelArchiveOptions{Dir: dir})
	require.NoError(t, err)
	names := entryNames(readArchive(t, rc))
	require.Equal(t, "config.yaml,real.txt", strings.Join(names, ","))
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

	rc, err := modelarchive.BuildModelArchive(ctx, modelarchive.BuildModelArchiveOptions{Dir: dir})
	require.NoError(t, err)
	_, err = io.ReadAll(rc)
	rc.Close()
	require.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got %v", err)
}
