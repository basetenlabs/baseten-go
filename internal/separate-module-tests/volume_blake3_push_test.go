package separatemoduletests_test

import (
	"context"
	"errors"
	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/zeebo/blake3"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type goldenUploadSession struct {
	mu           sync.Mutex
	chunkDigests []volume.Digest
	manifests    [][]byte
}

func (*goldenUploadSession) MissingObjects(
	_ context.Context,
	digests []volume.Digest,
) ([]volume.Digest, error) {
	return append([]volume.Digest(nil), digests...), nil
}

func (session *goldenUploadSession) UploadObject(
	_ context.Context,
	object volume.UploadObject,
) (volume.UploadObjectResult, error) {
	body, err := io.ReadAll(object.Body)
	if err != nil {
		return volume.UploadObjectResult{}, err
	}
	if uint64(len(body)) != object.Size {
		return volume.UploadObjectResult{}, errors.New("upload body size does not match metadata")
	}
	if got := volume.Digest(blake3.Sum256(body)); got != object.Digest {
		return volume.UploadObjectResult{}, errors.New("SDK object digest does not match Zeebo BLAKE3")
	}
	if object.Kind == volume.ObjectKindChunk {
		session.mu.Lock()
		session.chunkDigests = append(session.chunkDigests, object.Digest)
		session.mu.Unlock()
	} else if object.Kind == volume.ObjectKindManifest {
		session.mu.Lock()
		session.manifests = append(session.manifests, append([]byte(nil), body...))
		session.mu.Unlock()
	}
	return volume.UploadObjectResult{Created: true}, nil
}

func (*goldenUploadSession) Publish(
	context.Context,
	volume.Digest,
) (volume.PublishResult, error) {
	return volume.PublishResult{Outcome: volume.PublishOutcomePublished}, nil
}

func TestVolumeZeeboBLAKE3ChunkMatchesReferenceEncoderGolden(t *testing.T) {
	chunk := make([]byte, 102_400)
	for index := range chunk {
		chunk[index] = byte(index % 251)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "chunk.bin"), chunk, 0o600); err != nil {
		t.Fatal(err)
	}

	session := &goldenUploadSession{}
	if _, err := newZeeboVolumeClient(t).Push(t.Context(), volume.PushOptions{
		Path:     source,
		Session:  session,
		Uploader: session,
	}); err != nil {
		t.Fatal(err)
	}

	want, err := volume.ParseDigest(
		"b3:bc3e3d41a1146b069abffad3c0d44860cf664390afce4d9661f7902e7943e085",
	)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.chunkDigests) != 1 || session.chunkDigests[0] != want {
		t.Fatalf("chunk digests = %v, want [%s]", session.chunkDigests, want)
	}
}

func TestVolumeZeeboBLAKE3EmptyFileUsesImplicitCanonicalDigest(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	session := &goldenUploadSession{}
	result, err := newZeeboVolumeClient(t).Push(t.Context(), volume.PushOptions{
		Path:     source,
		Session:  session,
		Uploader: session,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LogicalBytes != 0 || result.UploadedBytes != 0 || result.FileCount != 1 {
		t.Fatalf("empty push result = %+v", result)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.chunkDigests) != 0 {
		t.Fatalf("empty push uploaded chunk digests %v", session.chunkDigests)
	}
	if len(session.manifests) != 1 ||
		!strings.Contains(
			string(session.manifests[0]),
			`"digest":"b3:af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262"`,
		) {
		t.Fatalf("empty manifest does not contain canonical BLAKE3 digest: %q", session.manifests)
	}
}
