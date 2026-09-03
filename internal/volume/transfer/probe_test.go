package transfer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
)

// TestCaseSensitiveFilesystemProbe checks the probe against the filesystem it
// is actually running on, by asking that filesystem the same question
// directly. The answer varies by machine — macOS is usually case-insensitive,
// Linux usually not — so the test compares the probe against reality rather
// than asserting a fixed result.
func TestCaseSensitiveFilesystemProbe(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe-truth"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := os.Lstat(filepath.Join(dir, "PROBE-TRUTH"))
	wantSensitive := os.IsNotExist(err)

	require.Equal(t, wantSensitive, caseSensitiveFilesystem(dir))

	// A directory nothing can be written into answers false, which refuses a
	// tree it might have written rather than overwriting a file it should not.
	require.False(t, caseSensitiveFilesystem(filepath.Join(dir, "does-not-exist")),
		"an unusable directory should answer conservatively")
}

func TestProbeDirectory(t *testing.T) {
	parent := t.TempDir()
	existing := filepath.Join(parent, "dest")
	require.NoError(t, os.Mkdir(existing, 0o755))

	// Writing in place puts everything inside the destination, so that is the
	// filesystem whose answer matters.
	require.Equal(t, existing, probeDirectory(existing, true))

	// A staged download writes to a sibling of the destination, so it is the
	// parent that answers — even when the destination already exists, since an
	// empty destination can be a mount point of its own.
	require.Equal(t, parent, probeDirectory(existing, false))
	require.Equal(t, parent, probeDirectory(filepath.Join(parent, "missing"), false))

	// With nothing to write into yet, the parent answers either way.
	require.Equal(t, parent, probeDirectory(filepath.Join(parent, "missing"), true))

	// A file is not a directory to probe in.
	file := filepath.Join(parent, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	require.Equal(t, parent, probeDirectory(file, true))
}
