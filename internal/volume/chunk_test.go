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
			got := ChunkRanges(tc.size)
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
