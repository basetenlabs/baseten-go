package separatemoduletests_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/volume/transfer"
	"github.com/klauspost/compress/zstd"
)

// substitute replaces a stored object's bytes while leaving it under the
// digest it was stored as — an object that is not what it claims to be, which
// is the only shape these checks exist to catch.
func substitute(t *testing.T, fake *fakeService, digest string, body []byte) {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	object, ok := fake.objects[digest]
	if !ok {
		t.Fatalf("no stored object %s", digest)
	}
	object.body = body
	fake.objects[digest] = object
}

// TestPullRefusesASubstitutedManifest covers the root of the trust chain.
// Every chunk a download verifies is verified against a digest that came out
// of the manifest, so a manifest nobody checked makes all of that verification
// authenticate the leaves against a root taken on faith.
func TestPullRefusesASubstitutedManifest(t *testing.T) {
	_, fake := pushFixture(t)
	dest := filepath.Join(t.TempDir(), "downloaded")

	// The substitute is compressed the same way the real object is, so the
	// download reaches the digest check rather than failing on the encoding
	// first — the point is to prove the digest is what refuses it.
	fake.mu.Lock()
	digest := fake.commits[len(fake.commits)-1].manifestDigest
	fake.mu.Unlock()
	encoder, encErr := zstd.NewWriter(nil)
	if encErr != nil {
		t.Fatal(encErr)
	}
	substitute(t, fake, digest, encoder.EncodeAll([]byte("{\"_type\":\"manifest_header\",\"entry_count\":0}\n"), nil))

	_, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake))
	if err == nil {
		t.Fatal("a manifest that is not what it claims to be should not be accepted")
	}
	if !strings.Contains(err.Error(), "manifest hashes to") {
		t.Errorf("expected a digest mismatch, got %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused download published its destination")
	}
}

// TestPullAcceptsAnIntactManifest is the other half of the pair: the check
// must pass what it should, or a test that only ever sees failure proves
// nothing about the honest path.
func TestPullAcceptsAnIntactManifest(t *testing.T) {
	_, fake := pushFixture(t)
	dest := filepath.Join(t.TempDir(), "downloaded")
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "assets"), 0o755) })

	if _, err := transfer.Pull(context.Background(), fake.client(t), pullOptions(dest, fake)); err != nil {
		t.Fatalf("an intact manifest should download: %v", err)
	}
}
