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
		stream.close()
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
// A bounded window fetches chunks concurrently and delivers them in order.
// Byte reservations are made in file order so later chunks cannot starve the
// next chunk the reader needs. Reservations last until consumption or cleanup.
type chunkStream struct {
	ctx     context.Context
	puller  *puller
	chunks  []volume.ChunkRef
	cancel  context.CancelFunc
	pending chan *streamChunk
	slots   chan struct{}
	done    chan struct{}
	current *streamChunk

	// err latches the first failure, including EOF.
	err error
}

// streamChunk owns one download and its buffer until the reader consumes it.
// Closing ready transfers ownership from the fetch goroutine to the reader.
type streamChunk struct {
	ctx    context.Context
	puller *puller
	ready  chan struct{}

	// pooled is the buffer backing buf when the current chunk is full-size and
	// nil otherwise, buf is what remains undelivered of that chunk, and held
	// is the share of the in-flight byte budget those bytes occupy. All three
	// are owned together and handed back together by release.
	pooled *[]byte
	buf    []byte
	held   int64

	err error
}

// start is lazy: handlers that ignore an entry do not download its contents.
func (s *chunkStream) start() {
	s.ctx, s.cancel = context.WithCancel(s.ctx)
	window := s.puller.limits.ChunkOperations
	if window <= 0 {
		window = volume.DefaultFileJobs
	}
	s.pending = make(chan *streamChunk, window)
	s.slots = make(chan struct{}, window)
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		defer close(s.pending)
		for _, chunk := range s.chunks {
			if chunk.Length == 0 {
				continue
			}
			select {
			case s.slots <- struct{}{}:
			case <-s.ctx.Done():
				return
			}
			if err := s.ctx.Err(); err != nil {
				return
			}
			if err := s.puller.bytes.Acquire(s.ctx, int64(chunk.Length)); err != nil {
				part := &streamChunk{err: err, ready: make(chan struct{})}
				close(part.ready)
				s.pending <- part
				return
			}
			part := &streamChunk{ctx: s.ctx, puller: s.puller, ready: make(chan struct{})}
			// The slot guarantees room in pending, even during cleanup.
			s.pending <- part
			go func() {
				part.err = part.fetch(chunk)
				close(part.ready)
			}()
		}
	}()
}

func (s *chunkStream) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if s.cancel == nil {
		s.start()
	}
	for s.current == nil || len(s.current.buf) == 0 {
		if s.current != nil {
			s.current.release()
			s.current = nil
			<-s.slots
		}
		part, ok := <-s.pending
		if !ok {
			s.err = s.ctx.Err()
			if s.err == nil {
				s.err = io.EOF
			}
			return 0, s.err
		}
		s.current = part
		<-part.ready
		if part.err != nil {
			s.err = part.err
			s.close()
			return 0, s.err
		}
	}
	n := copy(p, s.current.buf)
	s.current.buf = s.current.buf[n:]
	return n, nil
}

// close cancels outstanding work and waits for every buffer to be returned.
func (s *chunkStream) close() {
	if s.cancel == nil {
		return
	}
	s.cancel()
	if s.current != nil {
		s.current.release()
		s.current = nil
	}
	for part := range s.pending {
		<-part.ready
		part.release()
	}
	<-s.done
}

// fetch owns an already reserved byte budget, and downloads and verifies one chunk.
func (s *streamChunk) fetch(chunk volume.ChunkRef) error {
	permit, err := s.puller.limiter.Acquire(s.ctx)
	if err != nil {
		s.puller.bytes.Release(int64(chunk.Length))
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

	body, err := s.puller.downloadVerifiedChunk(s.ctx, chunk, buffer, permit)
	if err != nil {
		return err
	}

	s.pooled, s.buf, s.held = pooled, body, int64(chunk.Length)
	delivered = true
	s.puller.stats.fetched.Add(1)
	return nil
}

// release hands back the chunk's buffer and byte reservation. It is safe to
// call again during cleanup after the reader has consumed the chunk.
func (s *streamChunk) release() {
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
