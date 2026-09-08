package transfer

import (
	"context"
	"fmt"
	"io"

	"github.com/basetenlabs/baseten-go/internal/volume"
)

// streamEntries hands every selected entry to the caller's handler instead of
// writing anything to disk.
//
// One at a time, in canonical path order: a handler reproducing the tree
// somewhere that is not a filesystem needs a parent before what is under it,
// and needs a stable order. Fanning out across files would take that away,
// and a caller whose sink tolerates concurrency can fan out inside the
// handler, where it knows whether that is safe.
func (p *puller) streamEntries(ctx context.Context, manifest *volume.Manifest) error {
	// Entries carry what describes an entry, not how its bytes are stored, so
	// the chunk-bearing record has to be found again for a file.
	files := make(map[string]volume.FileEntry, len(manifest.Files))
	for _, file := range manifest.Files {
		files[file.Path] = file
	}

	for _, entry := range manifest.Entries() {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Nil chunks for a directory or a symlink, which makes a stream that
		// reports EOF on its first read.
		var chunks []volume.ChunkRef
		if entry.Kind == volume.EntryKindFile {
			var err error
			if chunks, err = p.chunksOf(ctx, files[entry.Path]); err != nil {
				return fmt.Errorf("read %s: %w", entry.Path, err)
			}
		}

		stream := &chunkStream{ctx: ctx, puller: p, chunks: chunks}
		err := p.opts.EntryHandler(ctx, entry, stream)
		// Released whatever the handler did, including returning without
		// draining the stream, which is what a handler wanting only a file's
		// first bytes does, and what every directory does.
		stream.release()
		if err != nil {
			return fmt.Errorf("read %s: %w", entry.Path, err)
		}
		if entry.Kind == volume.EntryKindFile {
			p.progress.Add(1, int64(entry.Size))
		}
	}
	return nil
}

// chunkStream reads one file's chunks in order, verifying each against its
// recorded digest before any of its bytes are handed out. So what a handler
// reads is always a verified prefix of the file: a corrupted or truncated
// chunk fails the Read it would have been part of rather than being delivered.
//
// Chunks are fetched one at a time, on demand. A file written to disk instead
// has its chunks fetched in parallel and placed at their offsets, which a
// sequential reader cannot do, since there is nowhere to put a chunk that
// arrives before the one in front of it.
type chunkStream struct {
	ctx    context.Context
	puller *puller
	chunks []volume.ChunkRef
	next   int

	// pooled is the buffer backing buf when the current chunk is full-size and
	// nil otherwise, buf is what remains undelivered of that chunk, and held
	// is the share of the in-flight byte budget those bytes occupy. All three
	// are owned together and handed back together by release.
	pooled *[]byte
	buf    []byte
	held   int64

	// err latches the first failure. A reader that returned an error once must
	// not resume delivering bytes if it is read again.
	err error
}

func (s *chunkStream) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	for len(s.buf) == 0 {
		s.release()
		if s.next == len(s.chunks) {
			return 0, io.EOF
		}
		chunk := s.chunks[s.next]
		s.next++
		// The empty file's chunk carries no bytes. Sizing a file on disk
		// produces it for free, so the write path skips it as neither fetched
		// nor reused; there is nothing to skip here but the fetch itself.
		if chunk.Length == 0 {
			continue
		}
		if s.err = s.fetch(chunk); s.err != nil {
			return 0, s.err
		}
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// fetch downloads one chunk and verifies it, leaving its bytes in buf.
func (s *chunkStream) fetch(chunk volume.ChunkRef) error {
	permit, err := s.puller.limiter.Acquire(s.ctx)
	if err != nil {
		return err
	}
	if err := s.puller.bytes.Acquire(s.ctx, int64(chunk.Length)); err != nil {
		permit.CompleteUntimed(volume.Neutral)
		return err
	}

	var pooled *[]byte
	var buffer []byte
	if chunk.Length == volume.ChunkSize {
		pooled = volume.AcquireChunkBuffer()
		buffer = (*pooled)[:0]
	} else {
		// One spare byte, as the write path takes, so a body of exactly the
		// expected length does not trip the read's growth fallback.
		buffer = make([]byte, 0, chunk.Length+1)
	}

	// The budget is held until the bytes have been delivered, since it is what
	// bounds resident chunk data and they are resident until then. So it is
	// handed to the stream on success and given back on every other path,
	// along with the buffer.
	delivered := false
	defer func() {
		if delivered {
			return
		}
		if pooled != nil {
			volume.ReleaseChunkBuffer(pooled)
		}
		s.puller.bytes.Release(int64(chunk.Length))
	}()

	req, err := s.puller.origin.request(s.ctx, chunk.Target, int64(chunk.Length))
	if err != nil {
		permit.CompleteUntimed(volume.Neutral)
		return err
	}
	body, err := volume.FetchObjectInto(
		s.ctx, s.puller.opts.DownloadObject, s.puller.opts.Decompress, req, int64(chunk.Length), buffer)
	if err != nil {
		permit.CompleteUntimed(failureOutcome(s.ctx, err))
		return err
	}
	permit.Complete(volume.Success)

	if uint64(len(body)) != chunk.Length {
		return fmt.Errorf("chunk %s is %d bytes, the manifest says %d", chunk.Digest, len(body), chunk.Length)
	}
	digest, err := volume.HashBytes(s.puller.opts.NewHasher, body)
	if err != nil {
		return err
	}
	if digest != chunk.Digest {
		return fmt.Errorf("chunk at offset %d hashes to %s, the manifest says %s", chunk.Offset, digest, chunk.Digest)
	}

	s.pooled, s.buf, s.held = pooled, body, int64(chunk.Length)
	delivered = true
	s.puller.stats.fetched.Add(1)
	return nil
}

// release hands back the current chunk's buffer and its share of the byte
// budget. Idempotent, which is what lets Read call it between chunks and the
// caller call it again once the handler has returned.
func (s *chunkStream) release() {
	if s.pooled != nil {
		volume.ReleaseChunkBuffer(s.pooled)
		s.pooled = nil
	}
	if s.held > 0 {
		s.puller.bytes.Release(s.held)
		s.held = 0
	}
	s.buf = nil
}
