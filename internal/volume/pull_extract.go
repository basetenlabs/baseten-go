package volume

import (
	"context"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type pullChunkWork struct {
	filePath string
	chunk    chunkEntry
}

func (c *Client) extractFiles(
	ctx context.Context,
	reader ObjectReader,
	root *rootedDirectory,
	directories map[string]rootedFileStat,
	plan pullPlan,
	resume *pullResume,
	progress *progressReporter,
	memory *byteGate,
) (uint64, uint64, error) {
	totalChunks := plan.chunkCount
	downloadProgress := newPullDownloadProgress(plan, progress)
	if totalChunks == 0 {
		progress.emit(ProgressEvent{
			Phase:      ProgressVerify,
			TotalItems: totalPointer(uint64(len(plan.files))),
			TotalBytes: totalPointer(plan.totalSize),
		})
		return 0, 0, nil
	}

	workerContext, cancel := context.WithCancel(ctx)
	defer cancel()
	var downloaded atomic.Uint64
	var reused atomic.Uint64
	var firstError error
	var errorMu sync.Mutex
	requests := newDataPathRequestGate(c.maxConcurrency)
	workers := min(c.maxConcurrency, totalChunks)
	work := make(chan pullChunkWork, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			for item := range work {
				if err := c.extractChunk(
					workerContext,
					reader,
					root,
					directories,
					item,
					resume,
					requests,
					memory,
					&downloaded,
					&reused,
					downloadProgress,
				); err != nil {
					errorMu.Lock()
					if firstError == nil {
						firstError = err
						cancel()
					}
					errorMu.Unlock()
					return
				}
			}
		}()
	}
sendWork:
	for _, file := range plan.files {
		for _, chunk := range file.chunks {
			select {
			case work <- pullChunkWork{filePath: file.path, chunk: chunk}:
			case <-workerContext.Done():
				break sendWork
			}
		}
	}
	close(work)
	wait.Wait()
	if firstError != nil {
		return 0, 0, firstError
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, canceledError("extract files", err)
	}
	progress.emit(ProgressEvent{
		Phase:      ProgressVerify,
		TotalItems: totalPointer(uint64(len(plan.files))),
		TotalBytes: totalPointer(plan.totalSize),
	})
	return downloaded.Load(), reused.Load(), nil
}

func (c *Client) extractChunk(
	ctx context.Context,
	reader ObjectReader,
	root *rootedDirectory,
	directories map[string]rootedFileStat,
	work pullChunkWork,
	resume *pullResume,
	requests *adaptiveRequestGate,
	memory *byteGate,
	downloaded *atomic.Uint64,
	reused *atomic.Uint64,
	progress *pullDownloadProgress,
) error {
	if work.chunk.Offset > math.MaxInt64 ||
		work.chunk.Length > math.MaxInt64 ||
		work.chunk.Length > uint64(^uint(0)>>1) {
		return preconditionError("extract file", "chunk is too large for this platform")
	}
	output, opened, err := root.openRegular(work.filePath, os.O_RDWR, 0, directories)
	if err != nil {
		return filesystemError("extract file", "staged file cannot be opened safely")
	}
	if validateOwnedRegular(opened, 0o600) != nil {
		output.Close()
		return integrityError(
			"extract file",
			"staged file has an unsafe owner, mode, or hardlink alias",
		)
	}

	completed := completedPullChunk{
		Path: work.filePath, Offset: work.chunk.Offset, Length: work.chunk.Length, Digest: work.chunk.Digest,
	}
	if resume.contains(completed) {
		memoryPermit, err := memory.acquire(ctx, work.chunk.Length)
		if err != nil {
			output.Close()
			return byteGateError("verify staged chunk", err)
		}
		reusable, verifyErr := c.stagedChunkMatches(output, work.chunk)
		memoryPermit.release()
		if verifyErr != nil {
			output.Close()
			return verifyErr
		}
		if reusable {
			if err := output.Close(); err != nil {
				return filesystemError("verify staged chunk", "staged file cannot be closed")
			}
			reused.Add(work.chunk.Length)
			return progress.complete(work.filePath, work.chunk.Length)
		}
	}

	requestPermit, err := requests.acquire(ctx)
	if err != nil {
		output.Close()
		return canceledError("download chunk", err)
	}
	requestOutcome := transferNeutral
	var requestLatency *time.Duration
	defer func() {
		if requestLatency == nil {
			requestPermit.complete(requestOutcome)
		} else {
			requestPermit.completeWithLatency(requestOutcome, *requestLatency)
		}
	}()
	length := work.chunk.Length
	body, attempt, err := c.readVerifiedObjectObserved(
		ctx,
		reader,
		work.chunk.Digest,
		expectedObject{
			kind:              ObjectKindChunk,
			maxDecodedBytes:   length,
			exactDecodedBytes: &length,
		},
		memory,
	)
	if err != nil {
		requestOutcome = classifyTransferOutcome(err, attempt.observation, false)
		output.Close()
		return err
	}
	defer body.release()
	if length > 0 {
		requestLatency = &body.transferLatency
	}
	if err := ctx.Err(); err != nil {
		output.Close()
		return canceledError("write staged chunk", err)
	}
	if len(body.data) > 0 {
		written, err := output.WriteAt(body.data, int64(work.chunk.Offset))
		if err != nil || written != len(body.data) {
			output.Close()
			return filesystemError("extract file", "verified chunk cannot be written")
		}
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return filesystemError("extract file", "staged chunk cannot be synchronized")
	}
	if err := output.Close(); err != nil {
		return filesystemError("extract file", "staged chunk cannot be closed")
	}
	if err := resume.markCompleted(completed); err != nil {
		return err
	}
	downloaded.Add(work.chunk.Length)
	requestOutcome = classifyTransferOutcome(
		nil,
		body.observation,
		work.chunk.Length > 0,
	)
	return progress.complete(work.filePath, work.chunk.Length)
}
