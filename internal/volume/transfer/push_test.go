package transfer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
	"github.com/basetenlabs/baseten-go/internal/volume"
)

// stubHasher stands in for a real one where only the presence of the option
// matters. Options validation runs before the hasher is checked against the
// BLAKE3 vectors, which is what these cases exercise.
func stubHasher() hash.Hash { return sha256.New() }

func TestPushOptionsValidate(t *testing.T) {
	valid := PushOptions{Namespace: "ns", Volume: "vol", SourceDir: "/tmp/tree", NewHasher: stubHasher}
	require.NoError(t, valid.Validate())

	tests := map[string]func(*PushOptions){
		"no namespace":   func(o *PushOptions) { o.Namespace = "" },
		"no volume":      func(o *PushOptions) { o.Volume = "" },
		"no source":      func(o *PushOptions) { o.SourceDir = "" },
		"no hasher":      func(o *PushOptions) { o.NewHasher = nil },
		"empty tag":      func(o *PushOptions) { o.Tags = []string{""} },
		"reserved tag":   func(o *PushOptions) { o.Tags = []string{"head"} },
		"reserved among": func(o *PushOptions) { o.Tags = []string{"prod", "head"} },
	}
	for name, break_ := range tests {
		t.Run(name, func(t *testing.T) {
			opts := valid
			break_(&opts)
			require.Error(t, opts.Validate())
		})
	}
}

func TestPriorAtMatchesOnlyTheSameSpan(t *testing.T) {
	prior := []volume.ChunkRef{
		{Offset: 0, Length: volume.ChunkSize, Digest: volume.Digest{1}},
		{Offset: volume.ChunkSize, Length: 100, Digest: volume.Digest{2}},
	}

	got := priorAt(prior, 1, volume.ChunkRange{Offset: volume.ChunkSize, Length: 100})
	require.NotNil(t, got)
	require.Equal(t, volume.Digest{2}, got.Digest)

	tests := map[string]struct {
		index int
		span  volume.ChunkRange
	}{
		"past the end":     {2, volume.ChunkRange{Offset: 2 * volume.ChunkSize, Length: 1}},
		"different length": {1, volume.ChunkRange{Offset: volume.ChunkSize, Length: 200}},
		"shifted offset":   {1, volume.ChunkRange{Offset: volume.ChunkSize + 1, Length: 100}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Nil(t, priorAt(prior, tc.index, tc.span))
		})
	}
}

// TestFailureOutcome pins that a cancelled sibling is not read as the origin
// pushing back. When one chunk fails, the transfer cancels the rest; counting
// each of those as a stall would make one failure look like a wall of them.
func TestFailureOutcome(t *testing.T) {
	live := context.Background()
	require.Equal(t, volume.Stall, failureOutcome(live, errors.New("connection refused")))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.Equal(t, volume.Neutral, failureOutcome(cancelled, errors.New("connection refused")))
	require.Equal(t, volume.Neutral, failureOutcome(live, context.Canceled))
	require.Equal(t, volume.Neutral, failureOutcome(live, fmt.Errorf("read chunk: %w", context.Canceled)))
	require.Equal(t, volume.Neutral, failureOutcome(live, context.DeadlineExceeded))
}

func TestAllReused(t *testing.T) {
	prior := []volume.ChunkRef{{Length: 1}, {Length: 2}}
	reused := []pushedChunk{{fromPrior: true}, {fromPrior: true}}

	require.True(t, allReused(reused, prior), "two reused chunks against two prior ones")
	require.False(t, allReused([]pushedChunk{{fromPrior: true}, {}}, prior), "one chunk was sent")
	require.False(t, allReused(reused[:1], prior), "the file lost a chunk")
	require.False(t, allReused(reused, prior[:1]), "the file gained a chunk")
	require.False(t, allReused(nil, nil), "a file with no chunks has nothing to reuse")
}
