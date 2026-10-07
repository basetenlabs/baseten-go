package sandbox

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
)

// readZip returns an archive's entries by name: file contents, or "<dir>"
// for a directory, and their modes.
func readZip(t *testing.T, archive []byte) (map[string]string, map[string]fs.FileMode) {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	contents := map[string]string{}
	modes := map[string]fs.FileMode{}
	for _, file := range reader.File {
		modes[file.Name] = file.Mode()
		if file.FileInfo().IsDir() {
			contents[file.Name] = "<dir>"
			continue
		}
		rc, err := file.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(rc)
		rc.Close()
		require.NoError(t, err)
		contents[file.Name] = string(data)
	}
	return contents, modes
}

// zipBytes zips entries in memory.
func zipBytes(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, writeZip(t.Context(), &buf, entries))
	return buf.Bytes()
}

// zipEntryContent returns a file entry's content, read from its local file
// if it has one.
func zipEntryContent(t *testing.T, entry zipEntry) string {
	t.Helper()
	if entry.sourcePath == "" {
		return string(entry.data)
	}
	data, err := os.ReadFile(entry.sourcePath)
	require.NoError(t, err)
	return string(data)
}

func TestFilesZipEntries(t *testing.T) {
	entries, err := filesZipEntries(map[string][]byte{
		"Dockerfile":   []byte("FROM debian\n"),
		"app/main.sh":  []byte("echo hi\n"),
		"app/empty.md": nil,
	})
	require.NoError(t, err)
	archive := zipBytes(t, entries)
	contents, modes := readZip(t, archive)
	require.MapEqual(t, contents, "Dockerfile", "FROM debian\n")
	require.MapEqual(t, contents, "app/main.sh", "echo hi\n")
	require.MapEqual(t, contents, "app/empty.md", "")
	require.Equal(t, fs.FileMode(0o644), modes["app/main.sh"])
	require.NoError(t, checkZipDockerfile(archive))

	t.Run("RequiresDockerfile", func(t *testing.T) {
		_, err := filesZipEntries(map[string][]byte{"main.sh": nil})
		require.Error(t, err)
	})

	t.Run("RejectsUnsafePaths", func(t *testing.T) {
		for _, path := range []string{"/abs", "../up", "a//b", `a\b`, "./a"} {
			_, err := filesZipEntries(map[string][]byte{"Dockerfile": nil, path: nil})
			require.Error(t, err)
		}
	})
}

func TestDirectoryZipEntries(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM debian\n"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(root, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "bin", "run"), []byte("#!/bin/sh\n"), 0o755))
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("linked"), 0o600))
	// Windows needs privileges to create links, so links are checked on
	// other platforms only.
	links := runtime.GOOS != "windows"
	if links {
		require.NoError(t, os.Symlink(filepath.Join(root, "bin", "run"), filepath.Join(root, "file-link")))
		require.NoError(t, os.Symlink(outside, filepath.Join(root, "dir-link")))
		require.NoError(t, os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "broken-link")))
	}

	entries, err := directoryZipEntries(t.Context(), root, nil)
	require.NoError(t, err)
	contents, modes := readZip(t, zipBytes(t, entries))
	require.MapEqual(t, contents, "Dockerfile", "FROM debian\n")
	require.MapEqual(t, contents, "bin/", "<dir>")
	require.MapEqual(t, contents, "bin/run", "#!/bin/sh\n")
	if runtime.GOOS != "windows" {
		// Windows files have no executable bit to keep.
		require.Equal(t, fs.FileMode(0o755), modes["bin/run"])
	}
	if links {
		// A file link within the directory stores a copy of the file, a
		// directory link an empty directory, and a broken link nothing.
		require.MapEqual(t, contents, "file-link", "#!/bin/sh\n")
		require.MapEqual(t, contents, "dir-link/", "<dir>")
		_, hasSecret := contents["dir-link/secret"]
		require.False(t, hasSecret, "directory links must not be followed")
		_, hasBroken := contents["broken-link"]
		require.False(t, hasBroken, "broken links are left out")
	}

	t.Run("FileLinkOutsideFails", func(t *testing.T) {
		if !links {
			t.Skip("Windows needs privileges to create links")
		}
		escaping := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(escaping, "Dockerfile"), []byte("FROM debian\n"), 0o644))
		require.NoError(t, os.Mkdir(filepath.Join(escaping, "sub"), 0o755))
		require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(escaping, "sub", "secret")))
		_, err := directoryZipEntries(t.Context(), escaping, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "only links to files within it can be pushed")
	})

	t.Run("RequiresDockerfile", func(t *testing.T) {
		_, err := directoryZipEntries(t.Context(), t.TempDir(), nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no Dockerfile")
	})

	t.Run("IgnoreLeavesOutPathsAndPrunesDirectories", func(t *testing.T) {
		dir := t.TempDir()
		for path, content := range map[string]string{
			"Dockerfile":        "FROM debian\n",
			".dockerignore":     "*\n",
			"keep.txt":          "keep",
			"skip.txt":          "skip",
			"skipdir/inner.txt": "inner",
			"sub/skip.txt":      "nested",
			"sub/keep.txt":      "nested keep",
		} {
			require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644))
		}
		var seen []string
		entries, err := directoryZipEntries(t.Context(), dir, func(_ context.Context, opts ImageIgnoreFileOptions) (bool, error) {
			seen = append(seen, opts.RelPath)
			return opts.Entry.Name() == "skip.txt" || opts.RelPath == "skipdir" ||
				opts.RelPath == "Dockerfile" || opts.RelPath == ".dockerignore", nil
		})
		require.NoError(t, err)
		contents, _ := readZip(t, zipBytes(t, entries))
		// The root Dockerfile and .dockerignore are kept without asking.
		require.MapEqual(t, contents, "Dockerfile", "FROM debian\n")
		require.MapEqual(t, contents, ".dockerignore", "*\n")
		require.MapEqual(t, contents, "keep.txt", "keep")
		require.MapEqual(t, contents, "sub/keep.txt", "nested keep")
		for _, name := range []string{"skip.txt", "skipdir/", "skipdir/inner.txt", "sub/skip.txt"} {
			_, ok := contents[name]
			require.False(t, ok, "%s was not left out", name)
		}
		require.Equal(t, "keep.txt,skip.txt,skipdir,sub,sub/keep.txt,sub/skip.txt", strings.Join(seen, ","))
	})

	t.Run("IgnoreErrorFails", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM debian\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), nil, 0o644))
		_, err := directoryZipEntries(t.Context(), dir, func(context.Context, ImageIgnoreFileOptions) (bool, error) {
			return false, errors.New("bad pattern")
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "bad pattern")
	})
}

func TestCheckZipDockerfile(t *testing.T) {
	archive := zipBytes(t, []zipEntry{{path: "sub/Dockerfile", data: []byte("FROM x")}})
	require.Error(t, checkZipDockerfile(archive))
	link := zipBytes(t, []zipEntry{{path: "Dockerfile", data: []byte("elsewhere/Dockerfile"), mode: fs.ModeSymlink | 0o777}})
	require.Error(t, checkZipDockerfile(link))
	require.Error(t, checkZipDockerfile([]byte("not a zip")))
}
