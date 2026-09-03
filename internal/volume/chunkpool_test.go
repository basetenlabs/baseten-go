package volume

import "testing"

// drainChunkBuffers empties the pool so a test starts from a known state.
func drainChunkBuffers() {
	for {
		select {
		case <-chunkBuffers:
		default:
			return
		}
	}
}

// TestChunkBufferPoolBoundsIdleMemory pins the pool's bound: releases past
// maxIdleChunkBuffers drop their buffers for the collector rather than
// growing what sits idle. The bound is what makes "in-flight bytes cap
// resident chunk memory" true — an unbounded pool would quietly hold a whole
// wave's worth of buffers outside the byte gate's accounting.
func TestChunkBufferPoolBoundsIdleMemory(t *testing.T) {
	drainChunkBuffers()
	t.Cleanup(drainChunkBuffers)

	buffers := make([]*[]byte, 2*maxIdleChunkBuffers)
	for i := range buffers {
		buffers[i] = AcquireChunkBuffer()
	}
	for _, buf := range buffers {
		ReleaseChunkBuffer(buf)
	}
	if got := len(chunkBuffers); got != maxIdleChunkBuffers {
		t.Errorf("after releasing %d buffers the pool holds %d, want the bound %d",
			len(buffers), got, maxIdleChunkBuffers)
	}

	// The buffers that come back out are pooled ones, empty and full-size,
	// and the drop was the overflow rather than the returns: every buffer
	// now acquired up to the bound is one of the released set.
	seen := map[*[]byte]bool{}
	for _, buf := range buffers {
		seen[buf] = true
	}
	for i := 0; i < maxIdleChunkBuffers; i++ {
		buf := AcquireChunkBuffer()
		if !seen[buf] {
			t.Fatalf("acquire %d returned a buffer that was never released into the pool", i)
		}
		if len(*buf) != 0 || cap(*buf) != ChunkSize+1 {
			t.Fatalf("pooled buffer has len %d cap %d, want len 0 cap %d", len(*buf), cap(*buf), ChunkSize+1)
		}
	}
}
