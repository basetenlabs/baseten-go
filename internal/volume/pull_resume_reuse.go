package volume

import (
	"context"
	"errors"
	"io"
	"os"
)

func (c *Client) reusableResumeBytes(
	ctx context.Context,
	root *rootedDirectory,
	directories map[string]rootedFileStat,
	plan pullPlan,
	resume *pullResume,
	memory *byteGate,
) (uint64, error) {
	if memory == nil {
		return 0, protocolError(
			"revalidate pull resume",
			"operation byte budget is not initialized",
		)
	}
	var reusableBytes uint64
	for _, file := range plan.files {
		if err := ctx.Err(); err != nil {
			return 0, canceledError("revalidate pull resume", err)
		}
		output, opened, err := root.openRegular(file.path, os.O_RDWR, 0, directories)
		if err != nil {
			return 0, filesystemError(
				"revalidate pull resume",
				"staged file cannot be opened safely",
			)
		}
		if validateOwnedRegular(opened, 0o600) != nil {
			output.Close()
			return 0, integrityError(
				"revalidate pull resume",
				"staged file has an unsafe owner, mode, or hardlink alias",
			)
		}
		for _, chunk := range file.chunks {
			if err := ctx.Err(); err != nil {
				output.Close()
				return 0, canceledError("revalidate pull resume", err)
			}
			completed := completedPullChunk{
				Path: file.path, Offset: chunk.Offset, Length: chunk.Length, Digest: chunk.Digest,
			}
			if !resume.contains(completed) {
				continue
			}
			memoryPermit, err := memory.acquire(ctx, chunk.Length)
			if err != nil {
				output.Close()
				return 0, byteGateError("revalidate pull resume", err)
			}
			matches, err := c.stagedChunkMatches(output, chunk)
			memoryPermit.release()
			if err != nil {
				output.Close()
				return 0, err
			}
			if !matches {
				continue
			}
			next, overflow := addUint64(reusableBytes, chunk.Length)
			if overflow || next > plan.totalSize {
				output.Close()
				return 0, integrityError(
					"revalidate pull resume",
					"reusable chunk bytes exceed the pull size",
				)
			}
			reusableBytes = next
		}
		if err := output.Close(); err != nil {
			return 0, filesystemError(
				"revalidate pull resume",
				"staged file cannot be closed",
			)
		}
	}
	return reusableBytes, nil
}

func (c *Client) stagedChunkMatches(output *os.File, chunk chunkEntry) (bool, error) {
	body := make([]byte, int(chunk.Length))
	var read int
	for read < len(body) {
		count, err := output.ReadAt(body[read:], int64(chunk.Offset)+int64(read))
		read += count
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, filesystemError("verify staged chunk", "staged chunk cannot be read")
		}
		if count == 0 {
			return false, nil
		}
	}
	digest, err := c.digest(body)
	clear(body)
	if err != nil {
		return false, err
	}
	return digest == chunk.Digest, nil
}
