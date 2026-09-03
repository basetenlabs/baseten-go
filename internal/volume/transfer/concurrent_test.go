package transfer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
)

func TestForEachRunsEverythingBounded(t *testing.T) {
	items := make([]int, 50)
	for i := range items {
		items[i] = i
	}
	var live, peak, done atomic.Int64

	err := forEach(context.Background(), 4, items, func(_ context.Context, i int, item int) error {
		require.Equal(t, i, item)
		n := live.Add(1)
		for {
			high := peak.Load()
			if n <= high || peak.CompareAndSwap(high, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		live.Add(-1)
		done.Add(1)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, int64(50), done.Load())
	require.True(t, peak.Load() <= 4, "ran %d at once, limit was 4", peak.Load())
	require.True(t, peak.Load() > 1, "never ran anything concurrently")
}

// TestForEachReportsTheOriginalCause pins the property both engines depend on:
// the first real failure is what surfaces, not the cancellation it triggers in
// its siblings.
func TestForEachReportsTheOriginalCause(t *testing.T) {
	cause := errors.New("the real problem")
	items := make([]int, 100)
	var started atomic.Int64

	err := forEach(context.Background(), 4, items, func(ctx context.Context, i int, _ int) error {
		started.Add(1)
		if i == 1 {
			return cause
		}
		// Everything else reports the cancellation, so a loop that surfaced
		// the wrong error would have plenty of chances to. They time out
		// rather than waiting forever: with every slot held by a task waiting
		// to be cancelled, the failing task could never be reached.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return errors.New("never cancelled")
		}
	})
	require.True(t, errors.Is(err, cause), "got %v, want the original cause", err)
	require.True(t, started.Load() < int64(len(items)), "the loop kept starting work after failing")
}

// TestForEachStopsOnAnAlreadyCancelledContext covers the caller giving up
// before the loop begins.
func TestForEachStopsOnAnAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var ran atomic.Int64
	err := forEach(ctx, 4, make([]int, 10), func(context.Context, int, int) error {
		ran.Add(1)
		return nil
	})
	require.True(t, errors.Is(err, context.Canceled), "got %v", err)
	require.Equal(t, int64(0), ran.Load())
}

func TestForEachHandlesEmptyAndUnitCases(t *testing.T) {
	require.NoError(t, forEach(context.Background(), 4, []int(nil), func(context.Context, int, int) error {
		t.Fatal("nothing to do, but something ran")
		return nil
	}))

	// A nonsensical limit still makes progress rather than deadlocking.
	var ran atomic.Int64
	require.NoError(t, forEach(context.Background(), 0, make([]int, 3), func(context.Context, int, int) error {
		ran.Add(1)
		return nil
	}))
	require.Equal(t, int64(3), ran.Load())
}
