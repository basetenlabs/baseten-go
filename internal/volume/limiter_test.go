package volume

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
)

func TestSemaphoreLimiterCapsConcurrency(t *testing.T) {
	const capacity = 3
	limiter := NewSemaphoreLimiter(capacity)

	var live, peak atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			permit, err := limiter.Acquire(context.Background())
			require.NoError(t, err)
			defer permit.Complete(Success)

			now := live.Add(1)
			for {
				high := peak.Load()
				if now <= high || peak.CompareAndSwap(high, now) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			live.Add(-1)
		}()
	}
	wg.Wait()

	require.True(t, peak.Load() <= capacity, "peak concurrency %d exceeded the limit of %d", peak.Load(), capacity)
	require.True(t, peak.Load() > 1, "the limiter never ran anything concurrently")
}

func TestSemaphoreLimiterStopsOnCancellation(t *testing.T) {
	limiter := NewSemaphoreLimiter(1)
	held, err := limiter.Acquire(context.Background())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = limiter.Acquire(ctx)
	require.True(t, errors.Is(err, context.Canceled), "expected a cancellation, got %v", err)

	held.Complete(Success)
}

// TestPermitCompletesOnce guards the invariant the limiter depends on: a slot
// released twice would let the limit drift upward over a transfer.
func TestPermitCompletesOnce(t *testing.T) {
	var released atomic.Int64
	permit := NewPermit(func(Outcome, time.Duration) { released.Add(1) })

	permit.Complete(Success)
	permit.Complete(Stall)
	permit.CompleteUntimed(Neutral)
	require.Equal(t, int64(1), released.Load())
}

// TestPermitReportsWhatItSaw checks that the outcome and the latency reach the
// limiter, and that an untimed completion contributes no sample.
func TestPermitReportsWhatItSaw(t *testing.T) {
	var gotOutcome Outcome
	var gotElapsed time.Duration
	permit := NewPermit(func(outcome Outcome, elapsed time.Duration) {
		gotOutcome, gotElapsed = outcome, elapsed
	})
	// Completing immediately can land inside a single clock tick, which would
	// report zero and look exactly like an untimed completion. The wait is
	// what makes the distinction this test is checking observable at all;
	// relaxing the assertion to allow zero would erase it.
	const held = 2 * time.Millisecond
	time.Sleep(held)
	permit.Complete(Stall)
	require.Equal(t, Stall, gotOutcome)
	require.True(t, gotElapsed >= held, "a timed completion should carry the time it was held, got %v", gotElapsed)

	permit = NewPermit(func(outcome Outcome, elapsed time.Duration) {
		gotOutcome, gotElapsed = outcome, elapsed
	})
	permit.CompleteUntimed(Neutral)
	require.Equal(t, Neutral, gotOutcome)
	require.Equal(t, time.Duration(0), gotElapsed)
}

func TestByteGateCapsBytesInFlight(t *testing.T) {
	const limit = 4 * ChunkSize
	gate := NewByteGate(limit)
	ctx := context.Background()

	var inFlight, peak atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, gate.Acquire(ctx, ChunkSize))
			defer gate.Release(ChunkSize)

			now := inFlight.Add(ChunkSize)
			for {
				high := peak.Load()
				if now <= high || peak.CompareAndSwap(high, now) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inFlight.Add(-ChunkSize)
		}()
	}
	wg.Wait()

	require.True(t, peak.Load() <= limit, "peak %d bytes exceeded the %d byte budget", peak.Load(), limit)
}

// TestByteGateBlocksUntilReleased checks that the budget is a wait rather than
// a failure: a chunk that does not fit yet waits for one that does.
func TestByteGateBlocksUntilReleased(t *testing.T) {
	gate := NewByteGate(ChunkSize)
	ctx := context.Background()
	require.NoError(t, gate.Acquire(ctx, ChunkSize))

	admitted := make(chan struct{})
	go func() {
		require.NoError(t, gate.Acquire(ctx, ChunkSize))
		close(admitted)
	}()

	select {
	case <-admitted:
		t.Fatal("the gate admitted a chunk the budget had no room for")
	case <-time.After(20 * time.Millisecond):
	}

	gate.Release(ChunkSize)
	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("the gate never admitted the waiting chunk")
	}
}

// TestByteGateRefusesTheImpossible covers a request larger than the whole
// budget, which no amount of waiting would ever satisfy.
func TestByteGateRefusesTheImpossible(t *testing.T) {
	gate := NewByteGate(2 * ChunkSize)
	err := gate.Acquire(context.Background(), 3*ChunkSize)
	require.True(t, errors.Is(err, ErrByteBudget), "expected a budget error, got %v", err)
}

// TestByteGateAlwaysFitsOneChunk covers a budget configured below a single
// chunk, which would otherwise deadlock every transfer.
func TestByteGateAlwaysFitsOneChunk(t *testing.T) {
	gate := NewByteGate(1024)
	require.NoError(t, gate.Acquire(context.Background(), ChunkSize))
}

func TestByteGateStopsOnCancellation(t *testing.T) {
	gate := NewByteGate(ChunkSize)
	require.NoError(t, gate.Acquire(context.Background(), ChunkSize))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := gate.Acquire(ctx, ChunkSize)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "expected a deadline, got %v", err)
}

func TestConcurrencyDefaults(t *testing.T) {
	got := Concurrency{}.WithDefaults()
	require.Equal(t, DefaultFileJobs, got.FileJobs)
	require.Equal(t, int64(DefaultMaxBytesInFlight), got.MaxBytesInFlight)
	// Deliberately not defaulted: zero is the signal to adapt the limit to the
	// origin, so filling it in here would silently pin every transfer that
	// never asked to be pinned.
	require.Equal(t, 0, got.ChunkOperations)

	set := Concurrency{FileJobs: 2, ChunkOperations: 3, MaxBytesInFlight: 4}.WithDefaults()
	require.Equal(t, 2, set.FileJobs)
	require.Equal(t, 3, set.ChunkOperations)
	require.Equal(t, int64(4), set.MaxBytesInFlight)
}
