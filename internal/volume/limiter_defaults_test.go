package volume

import (
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
)

// TestConcurrencyDefaultsFollowThePinnedCeiling pins the file fan-out rule:
// unset, it takes the wide default so the object-operation limiter — not the
// outer file pool — governs the transfer; with ChunkOperations pinned it
// follows the pin, because files wider than the operations they can keep in
// flight only queue; set explicitly, it is honoured over both.
func TestConcurrencyDefaultsFollowThePinnedCeiling(t *testing.T) {
	// The structural floor, separate from the tuned value: the adaptive
	// limiter STARTS at up to 32 in-flight operations (its Initial is
	// GOMAXPROCS*2 capped at 32), so a file pool no wider than that starves
	// the limiter from its first sample and the starvation the default was
	// widened to fix comes straight back. The exact number may retune on new
	// measurements; this bound is the part any retuning must survive.
	require.True(t, DefaultFileJobs > 32,
		"DefaultFileJobs %d is not wider than the limiter's largest possible starting limit (32)", DefaultFileJobs)

	require.Equal(t, DefaultFileJobs, Concurrency{}.WithDefaults().FileJobs)
	require.Equal(t, 40, Concurrency{ChunkOperations: 40}.WithDefaults().FileJobs)
	require.Equal(t, 8, Concurrency{FileJobs: 8, ChunkOperations: 40}.WithDefaults().FileJobs)
	require.Equal(t, int64(DefaultMaxBytesInFlight), Concurrency{}.WithDefaults().MaxBytesInFlight)
	// Following the pin changes nothing about the pin itself: the operation
	// ceiling stays exactly what the caller set.
	require.Equal(t, 40, Concurrency{ChunkOperations: 40}.WithDefaults().ChunkOperations)
}
