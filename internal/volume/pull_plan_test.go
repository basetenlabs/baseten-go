//go:build linux || darwin

package volume

import (
	"fmt"
	"testing"
)

func TestPullPlanCachesChunkmapsAndBoundsGraph(t *testing.T) {
	makeChunkmap := func(marker byte) ([]byte, Digest) {
		chunkDigest := testDigest(marker)
		body := encodeChunkmap(1, []chunkEntry{{
			Digest: chunkDigest,
			Length: 1,
			Target: targetForDigest(chunkDigest),
		}})
		return body, testFixtureDigest(body)
	}
	t.Run("cache", func(t *testing.T) {
		client := newTestVolumeClient(t)
		body, digest := makeChunkmap(0x51)
		reader := &memoryObjectReader{objects: map[Digest]storedObject{
			digest: {body: body, kind: ObjectKindChunkmap, encoding: ObjectEncodingIdentity},
		}}
		manifest := validatedManifest{
			Files: []manifestFile{
				{Kind: fileKindChunkmap, Digest: digest, Mode: 0o600, Path: "one", Size: 1, Target: targetForDigest(digest)},
				{Kind: fileKindChunkmap, Digest: digest, Mode: 0o600, Path: "two", Size: 1, Target: targetForDigest(digest)},
			},
			TotalSize: 2,
		}
		plan, err := client.buildPullPlan(
			t.Context(),
			reader,
			manifest,
			newProgressReporter(OperationPull, nil, nil),
			newByteGate(client.maxBytesInFlight),
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		if plan.chunkCount != 2 || objectReadCount(reader, digest) != 1 {
			t.Fatalf("plan chunks = %d, chunkmap reads = %d", plan.chunkCount, objectReadCount(reader, digest))
		}
	})

	t.Run("aggregate fanout", func(t *testing.T) {
		client := newTestVolumeClient(t)
		body, digest := makeChunkmap(0x52)
		reader := &memoryObjectReader{objects: map[Digest]storedObject{
			digest: {body: body, kind: ObjectKindChunkmap, encoding: ObjectEncodingIdentity},
		}}
		client.maxManifestBytes = uint64(len(body) + 1)
		maxGraphChunks := max(client.maxManifestBytes/contentGraphChunkBudgetBytes, uint64(1))
		files := make([]manifestFile, maxGraphChunks+1)
		for index := range files {
			files[index] = manifestFile{
				Kind: fileKindChunkmap, Digest: digest, Mode: 0o600,
				Path: fmt.Sprintf("file-%04d", index), Size: 1, Target: targetForDigest(digest),
			}
		}
		_, err := client.buildPullPlan(
			t.Context(),
			reader,
			validatedManifest{Files: files, TotalSize: uint64(len(files))},
			newProgressReporter(OperationPull, nil, nil),
			newByteGate(client.maxBytesInFlight),
			nil,
		)
		if err == nil || !IsCode(err, ErrorPreconditionFailed) {
			t.Fatalf("fanout error = %v, want %s", err, ErrorPreconditionFailed)
		}
	})
}

func objectReadCount(reader *memoryObjectReader, digest Digest) int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	var count int
	for _, request := range reader.reads {
		if request.Digest == digest {
			count++
		}
	}
	return count
}
