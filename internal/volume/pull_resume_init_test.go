//go:build linux || darwin

package volume

import (
	"path/filepath"
	"testing"
)

func TestPullResumeLockContentionFailsLoud(t *testing.T) {
	fixture := newPullFixture(t)
	parent := t.TempDir()
	destination := filepath.Join(parent, "output")
	identity, err := newPullIdentity(fixture.manifestDigest, destination, nil)
	if err != nil {
		t.Fatal(err)
	}
	chunk := completedPullChunk{
		Path: "a.txt", Length: 5, Digest: fixture.directDigest,
	}
	expected := map[completedPullChunk]struct{}{chunk: {}}
	first, err := fixture.client.openPullResume(parent, identity, expected, false)
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	second, err := fixture.client.openPullResume(parent, identity, expected, false)
	if second != nil {
		second.close()
	}
	if err == nil || !IsCode(err, ErrorPreconditionFailed) {
		t.Fatalf("lock contention error = %v", err)
	}
}
