package transfer

import (
	"context"
	"io"
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
