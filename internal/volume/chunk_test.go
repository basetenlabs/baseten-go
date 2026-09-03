package volume

import (
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
)

func TestChunkRanges(t *testing.T) {
	tests := []struct {
		name string
		size uint64
		want []ChunkRange
	}{
		{"empty", 0, []ChunkRange{{Offset: 0, Length: 0}}},
		{"one byte", 1, []ChunkRange{{Offset: 0, Length: 1}}},
		{"exactly one chunk", ChunkSize, []ChunkRange{{Offset: 0, Length: ChunkSize}}},
		{
			"one chunk and a byte", ChunkSize + 1,
			[]ChunkRange{{Offset: 0, Length: ChunkSize}, {Offset: ChunkSize, Length: 1}},
		},
		{
			"one byte short of two chunks", 2*ChunkSize - 1,
			[]ChunkRange{{Offset: 0, Length: ChunkSize}, {Offset: ChunkSize, Length: ChunkSize - 1}},
		},
		{
			"exactly three chunks", 3 * ChunkSize,
			[]ChunkRange{
				{Offset: 0, Length: ChunkSize},
				{Offset: ChunkSize, Length: ChunkSize},
				{Offset: 2 * ChunkSize, Length: ChunkSize},
			},
		},
		{
			// A tail this small is where a coalescing writer profile would
			// fold it into the previous chunk. This one must not.
			"tiny tail", 2*ChunkSize + 7,
			[]ChunkRange{
				{Offset: 0, Length: ChunkSize},
				{Offset: ChunkSize, Length: ChunkSize},
				{Offset: 2 * ChunkSize, Length: 7},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []ChunkRange
			for i, r := range ChunkRanges(tc.size) {
				// The ordinal is the chunk's position in the chunkmap, so it
				// must count from zero without gaps.
				require.Equal(t, len(got), i)
				got = append(got, r)
			}
			require.Len(t, got, len(tc.want))
			for i := range tc.want {
				require.Equal(t, tc.want[i], got[i])
			}

			// Whatever the split, the chunks must tile the file exactly, which
			// is the invariant the server re-checks at commit.
			var total uint64
			for i, r := range got {
				if i > 0 {
					require.Equal(t, got[i-1].Offset+got[i-1].Length, r.Offset)
				}
				require.True(t, r.Length <= ChunkSize, "chunk %d is %d bytes, over the limit", i, r.Length)
				total += r.Length
			}
			require.Equal(t, tc.size, total)
		})
	}
}

// TestChunkCountAgreesWithTheSplit pins the two statements of the same rule
// against each other. ChunkCount exists so a caller can size a slice without
// draining the sequence, which makes it a second source of truth; this is
// what keeps the two from drifting apart.
func TestChunkCountAgreesWithTheSplit(t *testing.T) {
	for _, size := range []uint64{
		0, 1, ChunkSize - 1, ChunkSize, ChunkSize + 1, 2*ChunkSize - 1,
		2 * ChunkSize, 3 * ChunkSize, 3*ChunkSize + 7,
	} {
		var counted uint64
		for range ChunkRanges(size) {
			counted++
		}
		require.Equal(t, counted, ChunkCount(size))
	}
}

// TestChunkRangesStopsWhenTheCallerBreaks covers the half of the iterator
// contract a range loop hides: a caller that stops early must not keep the
// sequence running.
func TestChunkRangesStopsWhenTheCallerBreaks(t *testing.T) {
	seen := 0
	for range ChunkRanges(10 * ChunkSize) {
		seen++
		if seen == 3 {
			break
		}
	}
	require.Equal(t, 3, seen)
}
