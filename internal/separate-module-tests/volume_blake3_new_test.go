package separatemoduletests_test

import (
	"context"
	"github.com/basetenlabs/baseten-go/internal/volume"
	"io"
	"testing"
)

type separateModuleCopyDecoder struct{}

func (separateModuleCopyDecoder) Decode(
	_ context.Context,
	dst io.Writer,
	src io.Reader,
	_ volume.DecodeLimits,
) error {
	_, err := io.Copy(dst, src)
	return err
}

func newZeeboVolumeClient(t *testing.T) *volume.Client {
	t.Helper()
	client, err := volume.New(volume.Options{
		NewHasher:      newZeeboBLAKE3,
		Decoder:        separateModuleCopyDecoder{},
		MaxConcurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestVolumeNewAcceptsZeeboBLAKE3(t *testing.T) {
	newZeeboVolumeClient(t)
}
