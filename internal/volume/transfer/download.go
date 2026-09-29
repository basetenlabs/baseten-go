package transfer

import (
	"context"
	"fmt"

	"github.com/basetenlabs/baseten-go/internal/volume"
)

// downloadVerifiedChunk downloads into caller-owned storage and verifies the
// decompressed length and digest before returning bytes. It completes the
// supplied operation permit on every return path. The caller owns the buffer
// and byte reservation, retaining both until the bytes are written or consumed.
// Disk resume checks and destination-specific statistics remain with callers.
func (p *puller) downloadVerifiedChunk(ctx context.Context, chunk volume.ChunkRef, buffer []byte, permit *volume.Permit) ([]byte, error) {
	req, err := p.origin.request(ctx, chunk.Target, int64(chunk.Length))
	if err != nil {
		permit.CompleteUntimed(volume.Neutral)
		return nil, err
	}
	body, err := volume.FetchObjectInto(ctx, p.opts.DownloadObject, p.opts.Decompress, req, int64(chunk.Length), buffer)
	if err != nil {
		permit.CompleteUntimed(failureOutcome(ctx, err))
		return nil, err
	}
	permit.Complete(volume.Success)

	// Verify before either caller can publish bytes to its destination.
	if uint64(len(body)) != chunk.Length {
		return nil, fmt.Errorf("chunk %s is %d bytes, the manifest says %d", chunk.Digest, len(body), chunk.Length)
	}
	digest, err := volume.HashBytes(p.opts.NewHasher, body)
	if err != nil {
		return nil, err
	}
	if digest != chunk.Digest {
		return nil, fmt.Errorf("chunk at offset %d hashes to %s, the manifest says %s", chunk.Offset, digest, chunk.Digest)
	}
	return body, nil
}
