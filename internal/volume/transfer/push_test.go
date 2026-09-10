package transfer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
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

func TestUploadOnceSingleFlightsConcurrentCallers(t *testing.T) {
	p := &pusher{}
	key := uploadKey{kind: objectChunk, digest: volume.Digest{1}}
	target := volume.TargetForDigest(key.digest)

	const callers = 32
	start := make(chan struct{})
	release := make(chan struct{})
	ownerStarted := make(chan struct{})
	var calls atomic.Int64
	type outcome struct {
		result *bdn.UploadResult
		reused bool
		err    error
	}
	results := make([]outcome, callers)

	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			result, reused, _, err := p.uploadOnce(context.Background(), key, func() (*bdn.UploadResult, error) {
				if calls.Add(1) == 1 {
					close(ownerStarted)
				}
				<-release
				return &bdn.UploadResult{Digest: key.digest, Target: target, Created: true}, nil
			})
			results[i] = outcome{result: result, reused: reused, err: err}
		}(i)
	}

	close(start)
	<-ownerStarted
	close(release)
	wg.Wait()

	require.Equal(t, int64(1), calls.Load())
	owners := 0
	for _, got := range results {
		require.NoError(t, got.err)
		require.NotNil(t, got.result)
		require.Equal(t, target, got.result.Target)
		if !got.reused {
			owners++
		}
	}
	require.Equal(t, 1, owners)
}

func TestUploadOnceDoesNotCacheFailure(t *testing.T) {
	p := &pusher{}
	key := uploadKey{kind: objectChunk, digest: volume.Digest{2}}
	failed := errors.New("upload failed")
	var calls atomic.Int64

	_, reused, waited, err := p.uploadOnce(context.Background(), key, func() (*bdn.UploadResult, error) {
		calls.Add(1)
		return nil, failed
	})
	require.Error(t, err)
	require.False(t, reused, "a failed first call was not reuse")
	require.False(t, waited, "the first owner did not wait")

	target := volume.TargetForDigest(key.digest)
	result, reused, waited, err := p.uploadOnce(context.Background(), key, func() (*bdn.UploadResult, error) {
		calls.Add(1)
		return &bdn.UploadResult{Digest: key.digest, Target: target}, nil
	})
	require.NoError(t, err)
	require.False(t, reused, "the retry sent the request")
	require.False(t, waited, "a sequential retry had no flight to wait on")
	require.Equal(t, target, result.Target)
	require.Equal(t, int64(2), calls.Load())
}

func TestUploadOnceCancelledWaiterDoesNotCancelOwner(t *testing.T) {
	p := &pusher{}
	key := uploadKey{kind: objectChunk, digest: volume.Digest{3}}
	target := volume.TargetForDigest(key.digest)
	ownerStarted := make(chan struct{})
	release := make(chan struct{})
	ownerDone := make(chan error, 1)
	var calls atomic.Int64

	go func() {
		_, _, _, err := p.uploadOnce(context.Background(), key, func() (*bdn.UploadResult, error) {
			calls.Add(1)
			close(ownerStarted)
			<-release
			return &bdn.UploadResult{Digest: key.digest, Target: target}, nil
		})
		ownerDone <- err
	}()
	<-ownerStarted

	waiterCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, waited, err := p.uploadOnce(waiterCtx, key, func() (*bdn.UploadResult, error) {
		calls.Add(1)
		return nil, errors.New("cancelled waiter became owner")
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "waiter should return its context error")
	require.True(t, waited, "the cancelled call found the owner's flight")

	close(release)
	require.NoError(t, <-ownerDone)

	result, reused, _, err := p.uploadOnce(context.Background(), key, func() (*bdn.UploadResult, error) {
		calls.Add(1)
		return nil, errors.New("completed result was not reused")
	})
	require.NoError(t, err)
	require.True(t, reused, "the owner's completed result should stay cached")
	require.Equal(t, target, result.Target)
	require.Equal(t, int64(1), calls.Load())
}

func TestUploadOnceScopesKeysByKindAndDigest(t *testing.T) {
	p := &pusher{}
	keys := []uploadKey{
		{kind: objectChunk, digest: volume.Digest{4}},
		{kind: objectChunkmap, digest: volume.Digest{4}},
		{kind: objectChunk, digest: volume.Digest{5}},
	}
	var calls atomic.Int64

	for _, key := range keys {
		result, reused, _, err := p.uploadOnce(context.Background(), key, func() (*bdn.UploadResult, error) {
			calls.Add(1)
			return &bdn.UploadResult{Digest: key.digest, Target: volume.TargetForDigest(key.digest)}, nil
		})
		require.NoError(t, err)
		require.NotNil(t, result)
		require.False(t, reused, "each distinct key should own its first upload")
	}
	require.Equal(t, int64(len(keys)), calls.Load())
}
