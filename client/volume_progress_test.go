package client

import (
	internal "github.com/basetenlabs/baseten-go/internal/volume"
	"testing"
)

func TestVolumeProgressAdapterTransfersTraffic(t *testing.T) {
	var got VolumeProgress
	callback := volumeProgressAdapter(func(p VolumeProgress) { got = p })
	callback(internal.Progress{Phase: internal.PhaseUpload, Files: 1, TotalFiles: 2, Bytes: 10, TotalBytes: 20, TransferredBytes: 7})
	want := VolumeProgress{Phase: VolumePhaseUpload, Files: 1, TotalFiles: 2, Bytes: 10, TotalBytes: 20, TransferredBytes: 7}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
