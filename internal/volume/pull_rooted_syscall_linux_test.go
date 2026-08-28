//go:build linux

package volume

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxENOSYSFallbackResumesRestrictiveState(t *testing.T) {
	fixture := newPullFixture(t)
	destination := filepath.Join(t.TempDir(), "output")
	staging := interruptPullAfterFirstChunk(t, fixture, destination)
	for path, mode := range map[string]os.FileMode{
		filepath.Join(staging, pullDataName, "a.txt"):        0o400,
		filepath.Join(staging, pullDataName, "dir"):          0o500,
		filepath.Join(staging, pullDataName, "dir", "b.txt"): 0o400,
	} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	forceLinuxFchmodat2Unavailable(t)
	result, err := fixture.client.Pull(t.Context(), fixture.options(destination))
	if err != nil {
		t.Fatal(err)
	}
	if result.ReusedBytes != 5 || result.DownloadedBytes != 6 {
		t.Fatalf("restrictive resume result = %+v", result)
	}
}
