package transfer

import (
	"fmt"
	"hash"

	"github.com/basetenlabs/baseten-go/internal/volume"
)

// verifyBody checks a fetched object's content against the digest that named
// it. The digest covers the content, so the caller passes the decompressed
// body rather than the bytes the store held.
//
// The kind is only for the message: what the caller needs to know is which of
// the objects in flight failed, and they all arrive the same way.
func verifyBody(newHasher func() hash.Hash, body []byte, want volume.Digest, kind string) error {
	got, err := volume.HashBytes(newHasher, body)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s hashes to %s, but was fetched as %s", kind, got, want)
	}
	return nil
}
