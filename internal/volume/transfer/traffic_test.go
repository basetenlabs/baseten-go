package transfer

import (
	"errors"
	"io"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/volume"
)

type partialTrafficBody struct{}

func (partialTrafficBody) Read(p []byte) (int, error) { return copy(p, "partial"), io.ErrUnexpectedEOF }
func (partialTrafficBody) Close() error               { return nil }

func TestTrafficCountsPartialFailureWithoutLogicalProgress(t *testing.T) {
	var last volume.Progress
	p := &puller{progress: volume.NewProgressReporter(func(update volume.Progress) { last = update })}
	p.progress.SetPhase(volume.PhaseDownload, 1, 100)
	body := &trafficReader{ReadCloser: partialTrafficBody{}, onRead: p.addTransferred}
	_, err := io.ReadAll(body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("unexpected error: %v", err)
	}
	if last.TransferredBytes != 7 || last.Bytes != 0 || last.Files != 0 {
		t.Fatalf("unexpected progress: %+v", last)
	}
	p.progress.SetPhase(volume.PhasePublish, 0, 0)
	if last.TransferredBytes != 0 || p.transferred.Load() != 7 {
		t.Fatalf("phase reset lost transfer total: progress=%+v total=%d", last, p.transferred.Load())
	}
}
