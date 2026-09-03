package volume

import "sync"

// chunkBuffers pools FULL-SIZE chunk buffers and nothing smaller. The churn
// worth pooling exists only at full size — every chunk of a large file but
// its last is exactly ChunkSize — while pooling short chunks at this size
// would break the byte gate's accounting: a transfer acquires a span's own
// length from the gate but would hold an 8 MiB buffer, and at the shipped
// 256-file default that is 256 x 8 MiB = 2 GiB, the entire default byte
// budget held in pooled capacity the gate never sees. Short chunks keep the
// exact-size allocation and never touch this pool, by construction at every
// call site rather than by discipline.
//
// Buffers carry ChunkSize+1 capacity. The spare byte keeps every
// exactly-full-size body off readAllSizedInto's io.ReadAll fallback branch;
// overrun detection does not depend on it — the fallback plus the caller's
// size check still name an overrun — the +1 buys the common path only.
var chunkBuffers = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, ChunkSize+1)
		return &buf
	},
}

// AcquireChunkBuffer takes a full-size chunk buffer from the pool. The
// caller owns it until ReleaseChunkBuffer, and the fetch seam FILLS a
// caller's buffer rather than returning one of its own, because the
// ownership has to be structural: see FetchObjectInto for the one case
// where returned bytes and pooled array are different objects.
func AcquireChunkBuffer() *[]byte {
	return chunkBuffers.Get().(*[]byte)
}

// ReleaseChunkBuffer returns a buffer to the pool. It must be called only
// once nothing references the bytes — after hashing and the write for a
// pull, after the upload call has returned (its request body fully consumed)
// for a push — and on EVERY exit path, which the call sites get structurally
// from defer.
func ReleaseChunkBuffer(buf *[]byte) {
	*buf = (*buf)[:0]
	chunkBuffers.Put(buf)
}
