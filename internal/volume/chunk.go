package volume

import "iter"

// ChunkSize is the fixed size a file is split into. The server rejects a
// larger chunk body outright, so this is a protocol constant rather than a
// tunable.
const ChunkSize = 8 << 20

// ChunkRange is a byte span of a file.
type ChunkRange struct {
	Offset uint64
	Length uint64
}

// ChunkRanges splits a file of the given size into the spans a push uploads:
// fixed ChunkSize chunks from offset zero, with the remainder as a shorter
// final chunk.
//
// The format also describes a profile that folds a small tail into the
// previous chunk. Not doing that is deliberate: chunk boundaries decide which
// objects two clients can share, and the plain split is the one the service's
// existing objects were built with — a client that folded the tail would
// share no large-file chunks with any of them.
//
// An empty file gets one zero-length chunk rather than none, because a file
// entry always names a chunk and the digest of no bytes is a real object.
// Each span is yielded with its ordinal. That number is part of the format
// rather than a convenience for the caller: it is the chunk's position in the
// chunkmap, and the chunkmap is ordered.
func ChunkRanges(size uint64) iter.Seq2[int, ChunkRange] {
	return func(yield func(int, ChunkRange) bool) {
		if size == 0 {
			yield(0, ChunkRange{})
			return
		}
		index := 0
		for offset := uint64(0); offset < size; offset += ChunkSize {
			length := uint64(ChunkSize)
			if remaining := size - offset; remaining < length {
				length = remaining
			}
			if !yield(index, ChunkRange{Offset: offset, Length: length}) {
				return
			}
			index++
		}
	}
}

// ChunkCount is how many spans ChunkRanges yields for a file of this size.
//
// It exists so a caller can size a slice without draining the sequence to
// find out how long it is. That makes it a second statement of the same rule,
// which is a thing worth being uneasy about: a caller that trusts this number
// and iterates fewer times would leave zero-valued entries behind, and a
// zero-valued chunk entry is a real digest of no bytes rather than an obvious
// blank. Callers sizing from it are expected to check the two agree.
func ChunkCount(size uint64) uint64 {
	if size == 0 {
		return 1
	}
	return (size + ChunkSize - 1) / ChunkSize
}
