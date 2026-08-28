//go:build linux || darwin

package volume

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
)

type storedObject struct {
	body     []byte
	kind     ObjectKind
	encoding ObjectEncoding
}

type observationOnCloseBody struct {
	io.Reader
	observation *TransferObservation
}

func (body *observationOnCloseBody) Close() error {
	body.observation.RetryCount = 1
	body.observation.StallObserved = true
	return nil
}

type errorObjectBody struct {
	closed      bool
	observation *TransferObservation
	closeErr    error
}

func (*errorObjectBody) Read([]byte) (int, error) {
	return 0, errors.New("error response body must not be read")
}

func (body *errorObjectBody) Close() error {
	body.closed = true
	body.observation.RetryCount = 1
	return body.closeErr
}

type memoryObjectReader struct {
	mu         sync.Mutex
	objects    map[Digest]storedObject
	reads      []ObjectRequest
	beforeRead func(context.Context, ObjectRequest) error
}

func (reader *memoryObjectReader) ReadObject(
	ctx context.Context,
	request ObjectRequest,
) (Object, error) {
	reader.mu.Lock()
	value, ok := reader.objects[request.Digest]
	beforeRead := reader.beforeRead
	reader.mu.Unlock()
	if !ok {
		return Object{}, os.ErrNotExist
	}
	if beforeRead != nil {
		if err := beforeRead(ctx, request); err != nil {
			return Object{}, err
		}
	}
	reader.mu.Lock()
	reader.reads = append(reader.reads, request)
	reader.mu.Unlock()
	body := append([]byte(nil), value.body...)
	return Object{
		Body:     io.NopCloser(bytes.NewReader(body)),
		Size:     int64(len(body)),
		Kind:     value.kind,
		Encoding: value.encoding,
	}, nil
}

type pullFixture struct {
	client         *Client
	reader         *memoryObjectReader
	manifestDigest Digest
	directDigest   Digest
	totalSize      uint64
}

func newPullFixture(t *testing.T) pullFixture {
	t.Helper()
	digest := func(body []byte) Digest {
		return testFixtureDigest(body)
	}
	directBody := []byte("hello")
	directDigest := digest(directBody)
	firstBody := []byte(" ")
	firstDigest := digest(firstBody)
	secondBody := []byte("world")
	secondDigest := digest(secondBody)
	chunkmapBody := encodeChunkmap(6, []chunkEntry{
		{Digest: firstDigest, Length: 1, Offset: 0, Target: targetForDigest(firstDigest)},
		{Digest: secondDigest, Length: 5, Offset: 1, Target: targetForDigest(secondDigest)},
	})
	chunkmapDigest := digest(chunkmapBody)
	manifestBody := encodeManifest(
		11,
		[]directoryEntry{{Mode: 0o750, Path: "dir"}},
		[]manifestFile{
			{
				Kind: fileKindChunk,
				Chunk: chunkEntry{
					Digest: directDigest, Length: 5, Target: targetForDigest(directDigest),
				},
				Mode: 0o444,
				Path: "a.txt",
				Size: 5,
			},
			{
				Kind:   fileKindChunkmap,
				Digest: chunkmapDigest,
				Mode:   0o555,
				Path:   "dir/b.txt",
				Size:   6,
				Target: targetForDigest(chunkmapDigest),
			},
		},
		[]symlinkEntry{{Mode: 0o777, Path: "link", Target: "a.txt"}},
		"local://fixture",
	)
	manifestDigest := digest(manifestBody)
	reader := &memoryObjectReader{objects: map[Digest]storedObject{
		manifestDigest: {
			body: manifestBody, kind: ObjectKindManifest, encoding: ObjectEncodingZstd,
		},
		chunkmapDigest: {
			body: chunkmapBody, kind: ObjectKindChunkmap, encoding: ObjectEncodingZstd,
		},
		directDigest: {
			body: directBody, kind: ObjectKindChunk, encoding: ObjectEncodingIdentity,
		},
		firstDigest: {
			body: firstBody, kind: ObjectKindChunk, encoding: ObjectEncodingZstd,
		},
		secondDigest: {
			body: secondBody, kind: ObjectKindChunk, encoding: ObjectEncodingIdentity,
		},
	}}
	return pullFixture{
		client:         newTestVolumeClient(t),
		reader:         reader,
		manifestDigest: manifestDigest,
		directDigest:   directDigest,
		totalSize:      11,
	}
}

func (fixture pullFixture) options(destination string) PullOptions {
	return PullOptions{
		ManifestDigest: fixture.manifestDigest,
		Objects:        fixture.reader,
		Destination:    destination,
	}
}
