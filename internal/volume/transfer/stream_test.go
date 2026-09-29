package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/volume"
)

func testChunkStream(t *testing.T, count, size, window int, budget int64, hook func(context.Context, int) error) *chunkStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	p := &puller{origin: &origin{}, limits: volume.Concurrency{ChunkOperations: window}, limiter: volume.NewSemaphoreLimiter(window), bytes: volume.NewByteGate(budget)}
	p.opts.NewHasher = stubHasher
	p.opts.DownloadObject = func(ctx context.Context, req volume.ObjectDownload) (*volume.ObjectResult, error) {
		index := int(req.Key[len(req.Key)-1] - 'a')
		if hook != nil {
			if err := hook(ctx, index); err != nil {
				return nil, err
			}
		}
		body := bytes.Repeat([]byte{byte('a' + index)}, size)
		return &volume.ObjectResult{Body: io.NopCloser(bytes.NewReader(body)), Size: int64(size)}, nil
	}
	s := &chunkStream{ctx: ctx, puller: p}
	for i := 0; i < count; i++ {
		body := bytes.Repeat([]byte{byte('a' + i)}, size)
		digest, err := volume.HashBytes(stubHasher, body)
		if err != nil {
			t.Fatal(err)
		}
		s.chunks = append(s.chunks, volume.ChunkRef{Digest: digest, Length: uint64(size), Offset: uint64(i * size), Target: volume.Target{RelativeKey: string(rune('a' + i))}})
	}
	t.Cleanup(s.close)
	return s
}

func assertStreamBudgetReleased(t *testing.T, s *chunkStream, budget int64) {
	t.Helper()
	s.close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.puller.bytes.Acquire(ctx, budget); err != nil {
		t.Fatalf("byte budget leaked: %v", err)
	}
	s.puller.bytes.Release(budget)
}

func TestChunkStreamPrefetchesAndOrders(t *testing.T) {
	later := make(chan struct{})
	s := testChunkStream(t, 3, 16, 2, volume.ChunkSize, func(ctx context.Context, i int) error {
		if i == 1 {
			close(later)
		}
		if i == 0 {
			select {
			case <-later:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("a", 16) + strings.Repeat("b", 16) + strings.Repeat("c", 16)
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	assertStreamBudgetReleased(t, s, volume.ChunkSize)
}

func TestChunkStreamBoundsWindow(t *testing.T) {
	var calls atomic.Int32
	second := make(chan struct{})
	s := testChunkStream(t, 8, 16, 2, volume.ChunkSize, func(_ context.Context, i int) error {
		calls.Add(1)
		if i == 1 {
			close(second)
		}
		return nil
	})
	if _, err := s.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-second:
	case <-s.ctx.Done():
		t.Fatal(s.ctx.Err())
	}
	// Neither buffered chunk has been consumed, so no third slot is available.
	assertStreamBudgetReleased(t, s, volume.ChunkSize)
	if n := calls.Load(); n != 2 {
		t.Fatalf("fetched %d chunks with a two-chunk window", n)
	}
}

func TestChunkStreamOneChunkBudget(t *testing.T) {
	s := testChunkStream(t, 3, volume.ChunkSize, 3, volume.ChunkSize, nil)
	n, err := io.Copy(io.Discard, s)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3*volume.ChunkSize {
		t.Fatalf("read %d bytes", n)
	}
	assertStreamBudgetReleased(t, s, volume.ChunkSize)
}

func TestChunkStreamEarlyCloseCancelsDownloads(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	s := testChunkStream(t, 3, 16, 2, volume.ChunkSize, func(ctx context.Context, i int) error {
		if i == 1 {
			close(started)
			<-ctx.Done()
			close(stopped)
			return ctx.Err()
		}
		return nil
	})
	if _, err := s.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-s.ctx.Done():
		t.Fatal(s.ctx.Err())
	}
	assertStreamBudgetReleased(t, s, volume.ChunkSize)
	select {
	case <-stopped:
	default:
		t.Fatal("close returned before download stopped")
	}
}

func TestChunkStreamRejectsCorruptionAndLatchesError(t *testing.T) {
	s := testChunkStream(t, 3, 16, 3, volume.ChunkSize, nil)
	s.chunks[1].Digest = s.chunks[0].Digest
	got, err := io.ReadAll(s)
	if err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("expected digest error, got %v", err)
	}
	if string(got) != strings.Repeat("a", 16) {
		t.Fatalf("delivered corrupt bytes: %q", got)
	}
	if n, again := s.Read(make([]byte, 1)); n != 0 || again != err {
		t.Fatalf("error not latched: %d, %v", n, again)
	}
	assertStreamBudgetReleased(t, s, volume.ChunkSize)
}

func TestChunkStreamCancellation(t *testing.T) {
	s := testChunkStream(t, 3, 16, 2, volume.ChunkSize, func(ctx context.Context, _ int) error { <-ctx.Done(); return ctx.Err() })
	ctx, cancel := context.WithCancel(s.ctx)
	s.ctx = ctx
	s.start()
	cancel()
	if _, err := io.ReadAll(s); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	assertStreamBudgetReleased(t, s, volume.ChunkSize)
}
