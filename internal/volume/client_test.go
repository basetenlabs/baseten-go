package volume

import (
	"context"
	"io"
	"testing"
)

type copyDecoder struct{}

func (copyDecoder) Decode(
	_ context.Context,
	dst io.Writer,
	src io.Reader,
	_ DecodeLimits,
) error {
	_, err := io.Copy(dst, src)
	return err
}

func newTestVolumeClient(t *testing.T) *Client {
	t.Helper()
	client, err := New(Options{
		NewHasher:        newTestHasher,
		Decoder:          copyDecoder{},
		MaxConcurrency:   4,
		MaxManifestBytes: 8 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.destinationReserveBytes = 0
	return client
}
