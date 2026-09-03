package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
)

// TestResumedShortChunkRefetchReusesItsBuffer pins the allocation shape of
// the one path that used to allocate twice: a resumable SHORT chunk whose
// on-disk bytes fail the resume check. The exact-size buffer the check read
// into is the same storage the refetch fills, so the path costs one
// chunk-length allocation, not two — measured in bytes, because the second
// allocation was the point: a tail chunk can be nearly 8 MiB.
//
// The hasher is any 32-byte hash — the digest only has to agree with itself
// here — and the downloader serves from memory, so what the measurement sees
// is the path's own storage plus small constant noise, against a threshold
// with half a chunk-length of slack.
func TestResumedShortChunkRefetchReusesItsBuffer(t *testing.T) {
	newHasher := func() hash.Hash { return sha256.New() }
	const length = 1 << 20 // short by construction: well under ChunkSize
	content := bytes.Repeat([]byte{0x5A}, length)
	stale := bytes.Repeat([]byte{0xC3}, length)
	digest, err := volume.HashBytes(newHasher, content)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "resumed.bin")
	handle, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	p := &puller{
		opts: PullOptions{
			NewHasher: newHasher,
			DownloadObject: func(_ context.Context, _ volume.ObjectDownload) (*volume.ObjectResult, error) {
				return &volume.ObjectResult{
					Body:        io.NopCloser(bytes.NewReader(content)),
					ContentType: bdn.ContentTypeChunk,
					Size:        int64(length),
				}, nil
			},
		},
		origin: newOrigin(nil, bdn.Ref{Namespace: "ns"}, "org", bdn.Origin{}),
	}
	limiter := volume.NewSemaphoreLimiter(1)
	chunk := volume.ChunkRef{
		Digest: digest,
		Length: length,
		Target: volume.Target{RelativeKey: "objects/b3/aa/bb/x"},
	}
	ctx := context.Background()

	run := func() {
		// The right length with the wrong bytes: the resume check reads it,
		// misses, and the refetch has to happen.
		if _, err := handle.WriteAt(stale, 0); err != nil {
			t.Fatal(err)
		}
		permit, err := limiter.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.writeChunk(ctx, handle, chunk, true, permit); err != nil {
			t.Fatal(err)
		}
	}
	run() // warm everything the first pass allocates once

	const rounds = 8
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < rounds; i++ {
		run()
	}
	runtime.ReadMemStats(&after)

	perRound := (after.TotalAlloc - before.TotalAlloc) / rounds
	if limit := uint64(length * 3 / 2); perRound > limit {
		t.Errorf("the resumed-short-chunk mismatch path allocates %d bytes per chunk, want at most %d — the refetch is not reusing the resume check's buffer", perRound, limit)
	}
}
