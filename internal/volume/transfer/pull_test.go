package transfer

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
	"github.com/basetenlabs/baseten-go/internal/volume"
)

// TestPullOptionsRejectRestartWithOverwrite pins the combination that would
// have deleted the caller's own directory. Overwrite promises to leave files
// the volume does not describe alone, and Restart means "discard what is
// there" — together they contradicted that promise, and bought nothing, since
// writing in place already refetches whatever fails to verify.
func TestPullOptionsRejectRestartWithOverwrite(t *testing.T) {
	valid := PullOptions{
		Ref: "ns/vol", DestDir: "/tmp/out", NewHasher: stubHasher,
		Decompress:     func(io.Reader) (io.ReadCloser, error) { return nil, nil },
		DownloadObject: func(context.Context, volume.ObjectDownload) (*volume.ObjectResult, error) { return nil, nil },
	}
	require.NoError(t, valid.Validate())

	overwrite := valid
	overwrite.Overwrite = true
	require.NoError(t, overwrite.Validate())

	restart := valid
	restart.Restart = true
	require.NoError(t, restart.Validate())

	both := valid
	both.Overwrite, both.Restart = true, true
	err := both.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "staged downloads")
}

// TestPulledModesKeepTheSpecialBits is the secondary evidence for the mode
// translation: the primary is the pure helper's own test, which no filesystem
// can skip. This one exercises the helper through a real chmod, so it is
// guarded — some mounts have opinions about these bits. It calls the
// translation directly, so it does not cover the download's call sites; that
// whole path, from a recorded mode to a file on disk, is what the nested
// test module's TestPullRestoresTheSpecialBits guards.
func TestPulledModesKeepTheSpecialBits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "binary")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	const recorded uint16 = 0o4755
	if err := root.Chmod("binary", volume.ModeFromManifest(recorded)); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeSetuid == 0 {
		t.Skip("this filesystem does not keep the setuid bit")
	}
	// Round-trip: what the scanner would record for the file now on disk must
	// be the mode the manifest carried.
	if got := uint16(info.Mode().Perm()) | specialBitsOf(info.Mode()); got != recorded {
		t.Errorf("recorded %04o, on disk it reads back as %04o", recorded, got)
	}
}

// specialBitsOf mirrors what the scanner records, so the assertion above is a
// round trip rather than a restatement of the helper under test.
func specialBitsOf(mode fs.FileMode) uint16 {
	var bits uint16
	if mode&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}
