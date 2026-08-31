package volume

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
// objects two clients can share, and the plain split is the one the service.s
// existing objects were built with — a client that folded the tail would
// share no large-file chunks with any of them.
//
// An empty file gets one zero-length chunk rather than none, because a file
// entry always names a chunk and the digest of no bytes is a real object.
func ChunkRanges(size uint64) []ChunkRange {
	if size == 0 {
		return []ChunkRange{{}}
	}
	ranges := make([]ChunkRange, 0, (size+ChunkSize-1)/ChunkSize)
	for offset := uint64(0); offset < size; offset += ChunkSize {
		length := uint64(ChunkSize)
		if remaining := size - offset; remaining < length {
			length = remaining
		}
		ranges = append(ranges, ChunkRange{Offset: offset, Length: length})
	}
	return ranges
}
