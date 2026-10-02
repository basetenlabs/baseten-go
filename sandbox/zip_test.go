package sandbox

import (
	"archive/zip"
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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

func TestFilesZipEntries(t *testing.T) {
	entries, err := filesZipEntries(map[string][]byte{
		"Dockerfile":   []byte("FROM debian\n"),
		"app/main.sh":  []byte("echo hi\n"),
		"app/empty.md": nil,
	})
	require.NoError(t, err)
	archive, err := writeZip(entries)
	require.NoError(t, err)
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
		require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "file-link")))
		require.NoError(t, os.Symlink(outside, filepath.Join(root, "dir-link")))
		require.NoError(t, os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "broken-link")))
	}

	entries, err := directoryZipEntries(root)
	require.NoError(t, err)
	archive, err := writeZip(entries)
	require.NoError(t, err)
	contents, modes := readZip(t, archive)
	require.MapEqual(t, contents, "Dockerfile", "FROM debian\n")
	require.MapEqual(t, contents, "bin/", "<dir>")
	require.MapEqual(t, contents, "bin/run", "#!/bin/sh\n")
	if runtime.GOOS != "windows" {
		// Windows files have no executable bit to keep.
		require.Equal(t, fs.FileMode(0o755), modes["bin/run"])
	}
	if links {
		// A file link stores the file, a directory link an empty directory,
		// and a broken link nothing.
		require.MapEqual(t, contents, "file-link", "linked")
		require.MapEqual(t, contents, "dir-link/", "<dir>")
		_, hasSecret := contents["dir-link/secret"]
		require.False(t, hasSecret, "directory links must not be followed")
		_, hasBroken := contents["broken-link"]
		require.False(t, hasBroken, "broken links are left out")
	}

	t.Run("RequiresDockerfile", func(t *testing.T) {
		_, err := directoryZipEntries(t.TempDir())
		require.Error(t, err)
		require.Contains(t, err.Error(), "no Dockerfile")
	})
}

func TestCheckZipDockerfile(t *testing.T) {
	archive, err := writeZip([]zipEntry{{path: "sub/Dockerfile", data: []byte("FROM x")}})
	require.NoError(t, err)
	require.Error(t, checkZipDockerfile(archive))
	require.Error(t, checkZipDockerfile([]byte("not a zip")))
}
