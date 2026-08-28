//go:build linux || darwin

package volume

import (
	"path/filepath"
	"testing"
)

func newPullProgressFixture(t *testing.T) pullFixture {
	t.Helper()
	firstBody := []byte("alpha")
	firstDigest := testFixtureDigest(firstBody)
	secondBody := []byte("beta")
	secondDigest := testFixtureDigest(secondBody)
	manifestBody := encodeManifest(
		uint64(len(firstBody)+len(secondBody)),
		[]directoryEntry{
			{Mode: 0o755, Path: "empty-dir"},
			{Mode: 0o755, Path: "empty-files"},
		},
		[]manifestFile{
			{
				Kind: fileKindChunk,
				Chunk: chunkEntry{
					Digest: firstDigest,
					Length: uint64(len(firstBody)),
					Target: targetForDigest(firstDigest),
				},
				Mode: 0o644,
				Path: "a.txt",
				Size: uint64(len(firstBody)),
			},
			{
				Kind: fileKindChunk,
				Chunk: chunkEntry{
					Digest: secondDigest,
					Length: uint64(len(secondBody)),
					Target: targetForDigest(secondDigest),
				},
				Mode: 0o644,
				Path: "b.txt",
				Size: uint64(len(secondBody)),
			},
			{
				Kind: fileKindChunk,
				Chunk: chunkEntry{
					Digest: blake3EmptyDigest,
					Target: targetForDigest(blake3EmptyDigest),
				},
				Mode: 0o644,
				Path: "empty-files/one",
			},
			{
				Kind: fileKindChunk,
				Chunk: chunkEntry{
					Digest: blake3EmptyDigest,
					Target: targetForDigest(blake3EmptyDigest),
				},
				Mode: 0o644,
				Path: "empty-files/two",
			},
		},
		nil,
		"local://progress-fixture",
	)
	manifestDigest := testFixtureDigest(manifestBody)
	return pullFixture{
		client: newTestVolumeClient(t),
		reader: &memoryObjectReader{objects: map[Digest]storedObject{
			manifestDigest: {
				body: manifestBody, kind: ObjectKindManifest, encoding: ObjectEncodingIdentity,
			},
			firstDigest: {
				body: firstBody, kind: ObjectKindChunk, encoding: ObjectEncodingIdentity,
			},
			secondDigest: {
				body: secondBody, kind: ObjectKindChunk, encoding: ObjectEncodingIdentity,
			},
		}},
		manifestDigest: manifestDigest,
		directDigest:   firstDigest,
		totalSize:      uint64(len(firstBody) + len(secondBody)),
	}
}

func pullProgressEvents(
	t *testing.T,
	fixture pullFixture,
	destination string,
	configure func(*PullOptions),
) (*PullResult, []ProgressEvent) {
	t.Helper()
	var events []ProgressEvent
	options := fixture.options(destination)
	options.Progress = func(event ProgressEvent) {
		events = append(events, event)
	}
	if configure != nil {
		configure(&options)
	}
	result, err := fixture.client.Pull(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	return result, events
}

func assertFinalDownloadProgress(
	t *testing.T,
	events []ProgressEvent,
	wantItems uint64,
	wantBytes uint64,
) {
	t.Helper()
	assertPhaseLocalProgress(t, events, OperationPull)
	var final *ProgressEvent
	for index := range events {
		if events[index].Phase == ProgressDownload {
			final = &events[index]
		}
	}
	if final == nil || final.TotalItems == nil || final.TotalBytes == nil {
		t.Fatalf("download progress = %+v, want final totals", events)
	}
	if final.CompletedItems != *final.TotalItems ||
		final.CompletedBytes != *final.TotalBytes ||
		final.CompletedItems != wantItems ||
		final.CompletedBytes != wantBytes {
		t.Fatalf(
			"final download progress = %d/%d items, %d/%d bytes; want %d items and %d bytes",
			final.CompletedItems,
			*final.TotalItems,
			final.CompletedBytes,
			*final.TotalBytes,
			wantItems,
			wantBytes,
		)
	}
}

func TestPullDownloadProgressCompletesEmptyFiles(t *testing.T) {
	t.Run("mixed empty and nonempty files", func(t *testing.T) {
		fixture := newPullProgressFixture(t)
		result, events := pullProgressEvents(
			t,
			fixture,
			filepath.Join(t.TempDir(), "output"),
			nil,
		)
		if result.FileCount != 4 || result.LogicalBytes != fixture.totalSize {
			t.Fatalf("mixed pull result = %+v", result)
		}
		assertFinalDownloadProgress(t, events, 4, fixture.totalSize)
	})

	t.Run("all-empty selection", func(t *testing.T) {
		fixture := newPullProgressFixture(t)
		result, events := pullProgressEvents(
			t,
			fixture,
			filepath.Join(t.TempDir(), "output"),
			func(options *PullOptions) {
				options.Include = []string{"empty-files/"}
			},
		)
		if result.FileCount != 2 || result.LogicalBytes != 0 {
			t.Fatalf("all-empty pull result = %+v", result)
		}
		assertFinalDownloadProgress(t, events, 2, 0)
	})

	t.Run("subset-selected empty file", func(t *testing.T) {
		fixture := newPullProgressFixture(t)
		result, events := pullProgressEvents(
			t,
			fixture,
			filepath.Join(t.TempDir(), "output"),
			func(options *PullOptions) {
				options.Include = []string{"empty-files/one"}
			},
		)
		if result.FileCount != 1 || result.DirectoryCount != 1 || result.LogicalBytes != 0 {
			t.Fatalf("empty-file subset result = %+v", result)
		}
		assertFinalDownloadProgress(t, events, 1, 0)
	})

	t.Run("subset-selected empty directory", func(t *testing.T) {
		fixture := newPullProgressFixture(t)
		result, events := pullProgressEvents(
			t,
			fixture,
			filepath.Join(t.TempDir(), "output"),
			func(options *PullOptions) {
				options.Include = []string{"empty-dir/"}
			},
		)
		if result.FileCount != 0 || result.DirectoryCount != 1 || result.LogicalBytes != 0 {
			t.Fatalf("empty-directory subset result = %+v", result)
		}
		assertFinalDownloadProgress(t, events, 0, 0)
	})

	for _, restart := range []bool{false, true} {
		name := "resume"
		if restart {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newPullProgressFixture(t)
			destination := filepath.Join(t.TempDir(), "output")
			interruptPullAfterFirstChunk(t, fixture, destination)
			result, events := pullProgressEvents(
				t,
				fixture,
				destination,
				func(options *PullOptions) {
					options.Restart = restart
				},
			)
			if restart {
				if result.ReusedBytes != 0 || result.DownloadedBytes != fixture.totalSize {
					t.Fatalf("restart result = %+v", result)
				}
			} else if result.ReusedBytes != 5 || result.DownloadedBytes != 4 {
				t.Fatalf("resume result = %+v", result)
			}
			assertFinalDownloadProgress(t, events, 4, fixture.totalSize)
		})
	}
}
