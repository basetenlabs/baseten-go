//go:build linux || darwin

package volume

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestPullVerifiesAndAtomicallyPublishesGraph(t *testing.T) {
	fixture := newPullFixture(t)
	parent := t.TempDir()
	destination := filepath.Join(parent, "output")
	var progress []ProgressEvent
	options := fixture.options(destination)
	options.Progress = func(event ProgressEvent) {
		progress = append(progress, event)
	}
	result, err := fixture.client.Pull(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(destination, "a.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("a.txt = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(destination, "dir", "b.txt")); err != nil ||
		string(got) != " world" {
		t.Fatalf("dir/b.txt = %q, %v", got, err)
	}
	if target, err := os.Readlink(filepath.Join(destination, "link")); err != nil || target != "a.txt" {
		t.Fatalf("link = %q, %v", target, err)
	}
	assertMode(t, filepath.Join(destination, "a.txt"), 0o444)
	assertMode(t, filepath.Join(destination, "dir", "b.txt"), 0o555)
	assertMode(t, filepath.Join(destination, "dir"), 0o750)
	if result.ManifestDigest != fixture.manifestDigest ||
		result.LogicalBytes != fixture.totalSize ||
		result.DownloadedBytes != fixture.totalSize ||
		result.ReusedBytes != 0 ||
		result.FileCount != 2 ||
		result.DirectoryCount != 1 ||
		!result.ContentVerified ||
		result.PublicationOutcome != PullPublicationComplete {
		t.Fatalf("unexpected result: %+v", result)
	}
	assertPhaseLocalProgress(t, progress, OperationPull)
	sawIntermediateDownload := false
	for _, event := range progress {
		if event.Phase == ProgressDownload &&
			event.CompletedBytes > 0 &&
			event.CompletedBytes < fixture.totalSize {
			sawIntermediateDownload = true
		}
	}
	if !sawIntermediateDownload {
		t.Fatalf("download progress = %+v, want per-chunk event", progress)
	}
	for _, entry := range mustReadDir(t, parent) {
		if strings.Contains(entry.Name(), ".staging") {
			t.Fatalf("completed staging remains: %s", entry.Name())
		}
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}

func TestPullIncludeSelectsSubset(t *testing.T) {
	fixture := newPullFixture(t)
	destination := filepath.Join(t.TempDir(), "output")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	options := fixture.options(destination)
	options.Include = []string{"dir/"}
	result, err := fixture.client.Pull(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(destination, "dir", "b.txt")); err != nil ||
		string(got) != " world" {
		t.Fatalf("selected file = %q, %v", got, err)
	}
	for _, excluded := range []string{"a.txt", "link"} {
		if _, err := os.Lstat(filepath.Join(destination, excluded)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unselected path %q exists: %v", excluded, err)
		}
	}
	if result.LogicalBytes != 6 ||
		result.FileCount != 1 ||
		result.VolumeLogicalBytes == nil ||
		*result.VolumeLogicalBytes != 11 ||
		result.VolumeFileCount == nil ||
		*result.VolumeFileCount != 2 {
		t.Fatalf("unexpected subset result: %+v", result)
	}
}

func TestPullMaterializesEmptyFileWithoutObjectReadOrCheckpoint(t *testing.T) {
	manifestBody := encodeManifest(
		0,
		nil,
		[]manifestFile{{
			Kind: fileKindChunk,
			Chunk: chunkEntry{
				Digest: blake3EmptyDigest,
				Target: targetForDigest(blake3EmptyDigest),
			},
			Mode: 0o640,
			Path: "empty",
		}},
		nil,
		"local://fixture",
	)
	manifestDigest := testFixtureDigest(manifestBody)
	reader := &memoryObjectReader{objects: map[Digest]storedObject{
		manifestDigest: {
			body: manifestBody, kind: ObjectKindManifest, encoding: ObjectEncodingIdentity,
		},
	}}
	client := newTestVolumeClient(t)
	destination := filepath.Join(t.TempDir(), "output")
	result, err := client.Pull(t.Context(), PullOptions{
		ManifestDigest: manifestDigest,
		Objects:        reader,
		Destination:    destination,
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(destination, "empty"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 || info.Mode().Perm() != 0o640 {
		t.Fatalf("empty file metadata = size %d mode %04o", info.Size(), info.Mode().Perm())
	}
	if result.DownloadedBytes != 0 || result.ReusedBytes != 0 || result.FileCount != 1 {
		t.Fatalf("empty pull result = %+v", result)
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.reads) != 1 || reader.reads[0].Kind != ObjectKindManifest {
		t.Fatalf("empty pull object reads = %+v, want manifest only", reader.reads)
	}
}

func TestPullIncludeSelectsEmptyDirectoryAndAncestors(t *testing.T) {
	manifestBody := encodeManifest(
		0,
		[]directoryEntry{
			{Mode: 0o711, Path: "parent"},
			{Mode: 0o500, Path: "parent/empty"},
		},
		nil,
		nil,
		"local://fixture",
	)
	manifestDigest := testFixtureDigest(manifestBody)
	reader := &memoryObjectReader{objects: map[Digest]storedObject{
		manifestDigest: {
			body: manifestBody, kind: ObjectKindManifest, encoding: ObjectEncodingIdentity,
		},
	}}
	destination := filepath.Join(t.TempDir(), "output")
	result, err := newTestVolumeClient(t).Pull(t.Context(), PullOptions{
		ManifestDigest: manifestDigest,
		Objects:        reader,
		Destination:    destination,
		Include:        []string{"parent/empty/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Join(destination, "parent"), 0o711)
	assertMode(t, filepath.Join(destination, "parent", "empty"), 0o500)
	if result.FileCount != 0 || result.DirectoryCount != 2 || result.LogicalBytes != 0 {
		t.Fatalf("empty-directory subset result = %+v", result)
	}
}

func TestPullMasksPrivilegedWireModesAtMaterialization(t *testing.T) {
	content := []byte("x")
	chunkDigest := testFixtureDigest(content)
	manifestBody := encodeManifest(
		1,
		[]directoryEntry{{Mode: 0o1777, Path: "directory"}},
		[]manifestFile{{
			Kind: fileKindChunk,
			Chunk: chunkEntry{
				Digest: chunkDigest,
				Length: 1,
				Target: targetForDigest(chunkDigest),
			},
			Mode: 0o4755,
			Path: "directory/file",
			Size: 1,
		}},
		nil,
		"local://fixture",
	)
	manifestDigest := testFixtureDigest(manifestBody)
	reader := &memoryObjectReader{objects: map[Digest]storedObject{
		manifestDigest: {
			body: manifestBody, kind: ObjectKindManifest, encoding: ObjectEncodingIdentity,
		},
		chunkDigest: {
			body: content, kind: ObjectKindChunk, encoding: ObjectEncodingIdentity,
		},
	}}
	destination := filepath.Join(t.TempDir(), "output")
	if _, err := newTestVolumeClient(t).Pull(t.Context(), PullOptions{
		ManifestDigest: manifestDigest,
		Objects:        reader,
		Destination:    destination,
	}); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Join(destination, "directory"), 0o777)
	assertMode(t, filepath.Join(destination, "directory", "file"), 0o755)
}

func TestPullFinalVerificationSupportsUnreadableFileMode(t *testing.T) {
	content := []byte("verified")
	chunkDigest := testFixtureDigest(content)
	manifestBody := encodeManifest(
		uint64(len(content)),
		nil,
		[]manifestFile{{
			Kind: fileKindChunk,
			Chunk: chunkEntry{
				Digest: chunkDigest,
				Length: uint64(len(content)),
				Target: targetForDigest(chunkDigest),
			},
			Mode: 0,
			Path: "sealed",
			Size: uint64(len(content)),
		}},
		nil,
		"local://fixture",
	)
	manifestDigest := testFixtureDigest(manifestBody)
	reader := &memoryObjectReader{objects: map[Digest]storedObject{
		manifestDigest: {
			body: manifestBody, kind: ObjectKindManifest, encoding: ObjectEncodingIdentity,
		},
		chunkDigest: {
			body: content, kind: ObjectKindChunk, encoding: ObjectEncodingIdentity,
		},
	}}
	destination := filepath.Join(t.TempDir(), "output")
	if _, err := newTestVolumeClient(t).Pull(t.Context(), PullOptions{
		ManifestDigest: manifestDigest,
		Objects:        reader,
		Destination:    destination,
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(destination, "sealed")
	assertMode(t, path, 0)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(path); err != nil || !bytes.Equal(body, content) {
		t.Fatalf("sealed file = %q, %v", body, err)
	}
}

func TestPullRejectsCrossKindDigestAliasesBeforeConflictingRead(t *testing.T) {
	contentDigest := testDigest(0x73)
	chunkmapBody := encodeChunkmap(1, []chunkEntry{{
		Digest: contentDigest,
		Length: 1,
		Target: targetForDigest(contentDigest),
	}})
	aliasedDigest := testFixtureDigest(chunkmapBody)
	manifestBody := encodeManifest(
		uint64(len(chunkmapBody))+1,
		nil,
		[]manifestFile{
			{
				Kind: fileKindChunk,
				Chunk: chunkEntry{
					Digest: aliasedDigest,
					Length: uint64(len(chunkmapBody)),
					Target: targetForDigest(aliasedDigest),
				},
				Mode: 0o600,
				Path: "raw-chunkmap.jsonl",
				Size: uint64(len(chunkmapBody)),
			},
			{
				Kind:   fileKindChunkmap,
				Digest: aliasedDigest,
				Mode:   0o600,
				Path:   "structured",
				Size:   1,
				Target: targetForDigest(aliasedDigest),
			},
		},
		nil,
		"local://fixture",
	)
	manifestDigest := testFixtureDigest(manifestBody)
	reader := &memoryObjectReader{objects: map[Digest]storedObject{
		manifestDigest: {
			body: manifestBody, kind: ObjectKindManifest, encoding: ObjectEncodingIdentity,
		},
		aliasedDigest: {
			body: chunkmapBody, kind: ObjectKindChunkmap, encoding: ObjectEncodingIdentity,
		},
	}}
	_, err := newTestVolumeClient(t).Pull(t.Context(), PullOptions{
		ManifestDigest: manifestDigest,
		Objects:        reader,
		Destination:    filepath.Join(t.TempDir(), "output"),
	})
	if err == nil || !IsCode(err, ErrorPreconditionFailed) {
		t.Fatalf("cross-kind pull error = %v, want %s", err, ErrorPreconditionFailed)
	}
	if got := objectReadCount(reader, aliasedDigest); got != 0 {
		t.Fatalf("conflicting object reads = %d, want 0", got)
	}
}

func TestPullTransferPhasesRevalidateSymlinkGraph(t *testing.T) {
	client := newTestVolumeClient(t)
	plan := pullPlan{symlinks: []symlinkEntry{
		{Mode: 0o777, Path: "a", Target: "b"},
		{Mode: 0o777, Path: "b", Target: "a"},
	}}
	_, _, err := client.extractPull(
		t.Context(),
		nil,
		nil,
		plan,
		newProgressReporter(OperationPull, nil, nil),
		newByteGate(client.maxBytesInFlight),
	)
	if err == nil || !IsCode(err, ErrorProtocol) {
		t.Fatalf("extraction graph error = %v, want %s", err, ErrorProtocol)
	}
	if _, err := client.publishStaging(
		t.Context(),
		&pullResume{},
		destinationPreflight{},
		plan,
	); err == nil || !IsCode(err, ErrorProtocol) {
		t.Fatalf("publication graph error = %v, want %s", err, ErrorProtocol)
	}
}

func TestPullCorruptionLeavesDestinationUntouched(t *testing.T) {
	fixture := newPullFixture(t)
	fixture.reader.mu.Lock()
	original := fixture.reader.objects[fixture.directDigest]
	fixture.reader.objects[fixture.directDigest] = storedObject{
		body: []byte("wrong"), kind: ObjectKindChunk, encoding: ObjectEncodingIdentity,
	}
	fixture.reader.mu.Unlock()
	destination := filepath.Join(t.TempDir(), "output")
	_, err := fixture.client.Pull(t.Context(), fixture.options(destination))
	if err == nil || !IsCode(err, ErrorIntegrity) {
		t.Fatalf("corrupt pull error = %v, want %s", err, ErrorIntegrity)
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("destination changed after corrupt pull: %v", statErr)
	}
	fixture.reader.mu.Lock()
	fixture.reader.objects[fixture.directDigest] = original
	fixture.reader.mu.Unlock()
}

func TestPullPreflightsDestinationBeforeObjectReads(t *testing.T) {
	fixture := newPullFixture(t)
	destination := filepath.Join(t.TempDir(), "output")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(destination, "existing")
	if err := os.WriteFile(existing, []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.client.Pull(t.Context(), fixture.options(destination))
	if err == nil || !IsCode(err, ErrorPreconditionFailed) {
		t.Fatalf("nonempty destination error = %v", err)
	}
	fixture.reader.mu.Lock()
	reads := len(fixture.reader.reads)
	fixture.reader.mu.Unlock()
	if reads != 0 {
		t.Fatalf("object reads = %d, want 0", reads)
	}
	if got, readErr := os.ReadFile(existing); readErr != nil || string(got) != "unchanged" {
		t.Fatalf("existing file = %q, %v", got, readErr)
	}
}

func interruptPullAfterFirstChunk(
	t *testing.T,
	fixture pullFixture,
	destination string,
) string {
	t.Helper()
	fixture.client.maxConcurrency = 1
	blocked := make(chan struct{})
	var blockedOnce sync.Once
	fixture.reader.mu.Lock()
	fixture.reader.beforeRead = func(ctx context.Context, request ObjectRequest) error {
		if request.Kind != ObjectKindChunk || request.Digest == fixture.directDigest {
			return nil
		}
		blockedOnce.Do(func() {
			close(blocked)
		})
		<-ctx.Done()
		return ctx.Err()
	}
	fixture.reader.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := fixture.client.Pull(ctx, fixture.options(destination))
		done <- err
	}()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("pull did not checkpoint its first chunk")
	}
	cancel()
	err := <-done
	if err == nil || !IsCode(err, ErrorCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted pull error = %v", err)
	}
	fixture.reader.mu.Lock()
	fixture.reader.beforeRead = nil
	fixture.reader.mu.Unlock()
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted pull published destination: %v", err)
	}
	staging := onlyPullStaging(t, filepath.Dir(destination))
	for _, internal := range []string{staging, filepath.Join(staging, pullDataName)} {
		assertMode(t, internal, 0o700)
	}
	for _, internal := range []string{pullCheckpointName, pullJournalName, pullLockName} {
		assertMode(t, filepath.Join(staging, internal), 0o600)
	}
	return staging
}

func TestPullInterruptionResumesRevalidatedChunks(t *testing.T) {
	fixture := newPullFixture(t)
	destination := filepath.Join(t.TempDir(), "output")
	interruptPullAfterFirstChunk(t, fixture, destination)
	if got := objectReadCount(fixture.reader, fixture.directDigest); got != 1 {
		t.Fatalf("direct chunk reads before resume = %d, want 1", got)
	}
	result, err := fixture.client.Pull(t.Context(), fixture.options(destination))
	if err != nil {
		t.Fatal(err)
	}
	if result.ReusedBytes != 5 || result.DownloadedBytes != 6 {
		t.Fatalf("resume result = %+v", result)
	}
	if got := objectReadCount(fixture.reader, fixture.directDigest); got != 1 {
		t.Fatalf("direct chunk reads after resume = %d, want 1", got)
	}
}

func TestPullResumesAfterRestrictiveModesWereFinalized(t *testing.T) {
	fixture := newPullFixture(t)
	destination := filepath.Join(t.TempDir(), "output")
	fixture.client.filesystemHooks = &filesystemTestHooks{
		afterPullPublishVerify: func(*pullResume) error {
			return errors.New("injected crash after mode finalization")
		},
	}
	_, err := fixture.client.Pull(t.Context(), fixture.options(destination))
	if err == nil || !IsCode(err, ErrorFilesystem) {
		t.Fatalf("post-verification interruption = %v, want %s", err, ErrorFilesystem)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("canceled pull published destination: %v", err)
	}
	fixture.client.filesystemHooks = nil
	result, err := fixture.client.Pull(t.Context(), fixture.options(destination))
	if err != nil {
		t.Fatal(err)
	}
	if result.ReusedBytes != fixture.totalSize || result.DownloadedBytes != 0 {
		t.Fatalf("mode-finalized resume result = %+v", result)
	}
	assertMode(t, filepath.Join(destination, "a.txt"), 0o444)
	assertMode(t, filepath.Join(destination, "dir", "b.txt"), 0o555)
}

func TestPullResumesAfterRestrictiveDirectoryModeWasFinalized(t *testing.T) {
	manifestBody := encodeManifest(
		0,
		[]directoryEntry{{Mode: 0, Path: "sealed"}},
		nil,
		nil,
		"local://fixture",
	)
	manifestDigest := testFixtureDigest(manifestBody)
	reader := &memoryObjectReader{objects: map[Digest]storedObject{
		manifestDigest: {
			body: manifestBody, kind: ObjectKindManifest, encoding: ObjectEncodingIdentity,
		},
	}}
	client := newTestVolumeClient(t)
	destination := filepath.Join(t.TempDir(), "output")
	options := PullOptions{
		ManifestDigest: manifestDigest,
		Objects:        reader,
		Destination:    destination,
	}
	client.filesystemHooks = &filesystemTestHooks{
		afterPullPublishVerify: func(*pullResume) error {
			return errors.New("injected crash after mode finalization")
		},
	}
	if _, err := client.Pull(t.Context(), options); err == nil || !IsCode(err, ErrorFilesystem) {
		t.Fatalf("post-verification interruption = %v, want %s", err, ErrorFilesystem)
	}
	client.filesystemHooks = nil
	if _, err := client.Pull(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Join(destination, "sealed"), 0)
}

func TestPullRedownloadsCorruptOrTruncatedCheckpointedRanges(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"corrupt": func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte("xxxxx"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"truncated": func(t *testing.T, path string) {
			t.Helper()
			if err := os.Truncate(path, 2); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newPullFixture(t)
			destination := filepath.Join(t.TempDir(), "output")
			staging := interruptPullAfterFirstChunk(t, fixture, destination)
			mutate(t, filepath.Join(staging, pullDataName, "a.txt"))
			result, err := fixture.client.Pull(t.Context(), fixture.options(destination))
			if err != nil {
				t.Fatal(err)
			}
			if result.ReusedBytes != 0 || result.DownloadedBytes != fixture.totalSize {
				t.Fatalf("recovery result = %+v", result)
			}
			if got := objectReadCount(fixture.reader, fixture.directDigest); got != 2 {
				t.Fatalf("direct chunk reads = %d, want 2", got)
			}
		})
	}
}

func TestPullRestartAndMalformedCheckpointRecovery(t *testing.T) {
	t.Run("restart", func(t *testing.T) {
		fixture := newPullFixture(t)
		destination := filepath.Join(t.TempDir(), "output")
		staging := interruptPullAfterFirstChunk(t, fixture, destination)
		options := fixture.options(destination)
		options.Restart = true
		result, err := fixture.client.Pull(t.Context(), options)
		if err != nil {
			t.Fatal(err)
		}
		if result.ReusedBytes != 0 || result.DownloadedBytes != fixture.totalSize {
			t.Fatalf("restart result = %+v", result)
		}
		if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("matching restart state remains: %v", err)
		}
	})

	t.Run("malformed checkpoint", func(t *testing.T) {
		fixture := newPullFixture(t)
		destination := filepath.Join(t.TempDir(), "output")
		staging := interruptPullAfterFirstChunk(t, fixture, destination)
		if err := os.WriteFile(filepath.Join(staging, pullCheckpointName), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := fixture.client.Pull(t.Context(), fixture.options(destination))
		if err == nil || !IsCode(err, ErrorIntegrity) {
			t.Fatalf("malformed checkpoint error = %v, want %s", err, ErrorIntegrity)
		}
		options := fixture.options(destination)
		options.Restart = true
		if _, err := fixture.client.Pull(t.Context(), options); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPullDestinationCapacityUsesRevalidatedRemainingBytes(t *testing.T) {
	t.Run("new pull fails before chunks", func(t *testing.T) {
		fixture := newPullFixture(t)
		fixture.client.destinationReserveBytes = 5
		fixture.client.availableSpace = func(string) (uint64, error) {
			return 15, nil
		}
		parent := t.TempDir()
		_, err := fixture.client.Pull(
			t.Context(),
			fixture.options(filepath.Join(parent, "output")),
		)
		if err == nil || !IsCode(err, ErrorPreconditionFailed) {
			t.Fatalf("capacity error = %v, want %s", err, ErrorPreconditionFailed)
		}
		if got := chunkReadCount(fixture.reader); got != 0 {
			t.Fatalf("chunk reads before capacity error = %d, want 0", got)
		}
		if len(mustReadDir(t, parent)) != 0 {
			t.Fatal("capacity failure retained new staging")
		}
	})

	t.Run("resume uses remaining bytes", func(t *testing.T) {
		fixture := newPullFixture(t)
		destination := filepath.Join(t.TempDir(), "output")
		interruptPullAfterFirstChunk(t, fixture, destination)
		fixture.client.destinationReserveBytes = 2
		fixture.client.availableSpace = func(string) (uint64, error) {
			return 8, nil
		}
		result, err := fixture.client.Pull(t.Context(), fixture.options(destination))
		if err != nil {
			t.Fatal(err)
		}
		if result.ReusedBytes != 5 || result.DownloadedBytes != 6 {
			t.Fatalf("exact-space resume = %+v", result)
		}
	})

	t.Run("overflow", func(t *testing.T) {
		fixture := newPullFixture(t)
		fixture.client.destinationReserveBytes = math.MaxUint64
		fixture.client.availableSpace = func(string) (uint64, error) {
			panic("overflow must fail before inspection")
		}
		_, err := fixture.client.Pull(
			t.Context(),
			fixture.options(filepath.Join(t.TempDir(), "output")),
		)
		if err == nil || !IsCode(err, ErrorPreconditionFailed) {
			t.Fatalf("overflow error = %v, want %s", err, ErrorPreconditionFailed)
		}
	})
}

func chunkReadCount(reader *memoryObjectReader) int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	var count int
	for _, request := range reader.reads {
		if request.Kind == ObjectKindChunk {
			count++
		}
	}
	return count
}

func TestPublicationOutcomeLinearizesAtRenameAndCleanupRetries(t *testing.T) {
	parent := t.TempDir()
	destinationPath := filepath.Join(parent, "output")
	destination, err := preflightDestination(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	stagingRoot := filepath.Join(parent, ".publication-state")
	if err := os.Mkdir(stagingRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	dataRoot := filepath.Join(stagingRoot, pullDataName)
	if err := os.Mkdir(dataRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "published.txt"), []byte("published"), 0o600); err != nil {
		t.Fatal(err)
	}
	createStateFile := func(path string) {
		t.Helper()
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Chmod(0o600); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	checkpointPath := filepath.Join(stagingRoot, pullCheckpointName)
	journalPath := filepath.Join(stagingRoot, pullJournalName)
	lockPath := filepath.Join(stagingRoot, pullLockName)
	createStateFile(checkpointPath)
	createStateFile(journalPath)
	symlinkTarget := filepath.Join(parent, "not-private-state")
	if err := os.WriteFile(symlinkTarget, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(symlinkTarget, lockPath); err != nil {
		t.Fatal(err)
	}
	stagingDirectory, err := destination.parent.openDirectoryRoot(".publication-state", nil)
	if err != nil {
		t.Fatal(err)
	}
	stagingStat, err := stagingDirectory.currentStat()
	if err != nil {
		t.Fatal(err)
	}
	dataDirectory, err := stagingDirectory.openDirectoryRoot(pullDataName, nil)
	if err != nil {
		t.Fatal(err)
	}
	dataStat, err := dataDirectory.currentStat()
	if err != nil {
		t.Fatal(err)
	}
	resume := &pullResume{
		stagingRoot:    stagingRoot,
		dataRoot:       dataRoot,
		checkpointPath: checkpointPath,
		journalPath:    journalPath,
		lockPath:       lockPath,
		parent:         destination.parent,
		staging:        stagingDirectory,
		data:           dataDirectory,
		stagingName:    ".publication-state",
		stagingStat:    stagingStat,
		dataStat:       dataStat,
		directories:    map[string]rootedFileStat{"": dataStat},
	}
	client := newTestVolumeClient(t)
	content := []byte("published")
	contentDigest := testFixtureDigest(content)
	plan := pullPlan{
		files: []plannedFile{{
			mode: 0o600,
			path: "published.txt",
			size: uint64(len(content)),
			chunks: []chunkEntry{{
				Digest: contentDigest,
				Length: uint64(len(content)),
			}},
		}},
		totalSize: uint64(len(content)),
	}
	verifiedTree, err := client.verifyStagingAfterExtraction(
		t.Context(),
		dataDirectory,
		resume.directories,
		plan,
		newProgressReporter(OperationPull, nil, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	resume.verifiedTree = &verifiedTree
	published, publishErr := client.publishStaging(t.Context(), resume, destination, plan)
	if !published || publishErr == nil {
		t.Fatalf("publish outcome = (%t, %v), want published cleanup failure", published, publishErr)
	}
	if got, err := os.ReadFile(filepath.Join(destinationPath, "published.txt")); err != nil ||
		string(got) != "published" {
		t.Fatalf("published destination = %q, %v", got, err)
	}
	result := &PullResult{
		OutputDirectory: destinationPath, PublicationOutcome: PullPublicationIncomplete, ContentVerified: true,
	}
	publicationErr := newPullPublicationError(result, resume, destination, publishErr)
	if !IsCode(publicationErr, ErrorPublicationIncomplete) {
		t.Fatalf("typed publication error = %v", publicationErr)
	}
	if rendered := fmt.Sprintf("%#v", publicationErr); strings.Contains(rendered, destinationPath) {
		t.Fatalf("publication error formatting exposed local path: %s", rendered)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	createStateFile(lockPath)
	if err := publicationErr.RetryCleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if result.PublicationOutcome != PullPublicationComplete {
		t.Fatalf("cleanup outcome = %q, want %q", result.PublicationOutcome, PullPublicationComplete)
	}
	if err := publicationErr.RetryCleanup(t.Context()); err != nil {
		t.Fatalf("idempotent cleanup retry error = %v", err)
	}
	if _, err := os.Lstat(stagingRoot); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("publication state remains after cleanup: %v", err)
	}
}

func TestFinalizeDirectoriesUsesBoundedDescriptorsAndNoFollow(t *testing.T) {
	t.Run("bounded descriptors", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Chmod(root, 0o700); err != nil {
			t.Fatal(err)
		}
		const directoryCount = 256
		plan := pullPlan{directories: make([]directoryEntry, 0, directoryCount)}
		for index := range directoryCount {
			name := fmt.Sprintf("directory-%04d", index)
			plan.directories = append(plan.directories, directoryEntry{Mode: 0o500, Path: name})
		}
		rooted, err := openRootedDirectory(root)
		if err != nil {
			t.Fatal(err)
		}
		defer rooted.close()
		directories, err := prepareStagingDirectories(
			t.Context(),
			rooted,
			plan,
			make(hostAliasRegistry),
			1_000,
			defaultPortablePathLimits(),
		)
		if err != nil {
			t.Fatal(err)
		}
		var original syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &original); err != nil {
			t.Fatal(err)
		}
		if original.Cur < 128 {
			t.Skip("open-file limit is below test threshold")
		}
		limited := original
		limited.Cur = 128
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limited); err != nil {
			t.Skipf("cannot lower open-file limit: %v", err)
		}
		defer syscall.Setrlimit(syscall.RLIMIT_NOFILE, &original)
		client := newTestVolumeClient(t)
		verified, err := client.verifyStagingAfterExtraction(
			t.Context(),
			rooted,
			directories,
			plan,
			newProgressReporter(OperationPull, nil, nil),
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.verifyStagingForPublication(
			t.Context(),
			rooted,
			directories,
			plan,
			verified,
		); err != nil {
			t.Fatal(err)
		}
		assertMode(t, filepath.Join(root, "directory-0000"), 0o500)
	})

	t.Run("symlink substitution", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Chmod(root, 0o700); err != nil {
			t.Fatal(err)
		}
		target := t.TempDir()
		before, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "directory")); err != nil {
			t.Fatal(err)
		}
		rooted, err := openRootedDirectory(root)
		if err != nil {
			t.Fatal(err)
		}
		defer rooted.close()
		rootStat, err := rooted.currentStat()
		if err != nil {
			t.Fatal(err)
		}
		_, err = newTestVolumeClient(t).verifyStagingAfterExtraction(
			t.Context(),
			rooted,
			map[string]rootedFileStat{"": rootStat},
			pullPlan{
				directories: []directoryEntry{{Mode: 0o500, Path: "directory"}},
			},
			newProgressReporter(OperationPull, nil, nil),
		)
		if err == nil || !IsCode(err, ErrorIntegrity) {
			t.Fatalf("symlink substitution error = %v, want %s", err, ErrorIntegrity)
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != before.Mode().Perm() {
			t.Fatalf("symlink target mode changed to %04o", info.Mode().Perm())
		}
	})
}

func TestPullRejectsDirectoryToSymlinkSwapBeforeWrites(t *testing.T) {
	fixture := newPullFixture(t)
	destination := filepath.Join(t.TempDir(), "output")
	outside := t.TempDir()
	marker := filepath.Join(outside, "marker")
	if err := os.WriteFile(marker, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.client.filesystemHooks = &filesystemTestHooks{
		afterPullPrepared: func(resume *pullResume) {
			directory := filepath.Join(resume.dataRoot, "dir")
			if err := os.Rename(directory, directory+".moved"); err != nil {
				t.Error(err)
				return
			}
			if err := os.Symlink(outside, directory); err != nil {
				t.Error(err)
			}
		},
	}
	_, err := fixture.client.Pull(t.Context(), fixture.options(destination))
	if err == nil || !IsCode(err, ErrorFilesystem) {
		t.Fatalf("directory-to-symlink pull error = %v, want %s", err, ErrorFilesystem)
	}
	if body, readErr := os.ReadFile(marker); readErr != nil || string(body) != "unchanged" {
		t.Fatalf("outside marker = %q, %v", body, readErr)
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("directory swap published destination: %v", statErr)
	}
}

func TestPullRegularFileToFIFOTransitionCannotBlock(t *testing.T) {
	fixture := newPullFixture(t)
	destination := filepath.Join(t.TempDir(), "output")
	fixture.client.filesystemHooks = &filesystemTestHooks{
		afterPullPrepared: func(resume *pullResume) {
			path := filepath.Join(resume.dataRoot, "a.txt")
			if err := os.Remove(path); err != nil {
				t.Error(err)
				return
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Error(err)
			}
		},
	}
	done := make(chan error, 1)
	ctx := t.Context()
	go func() {
		_, err := fixture.client.Pull(ctx, fixture.options(destination))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !IsCode(err, ErrorFilesystem) {
			t.Fatalf("staged FIFO transition error = %v, want %s", err, ErrorFilesystem)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pull blocked while opening a staged file replaced by a FIFO")
	}
}

func TestPullRejectsStagingRootReplacementBeforePublication(t *testing.T) {
	fixture := newPullFixture(t)
	parent := t.TempDir()
	destination := filepath.Join(parent, "output")
	outside := t.TempDir()
	fixture.client.filesystemHooks = &filesystemTestHooks{
		beforePullPublish: func(resume *pullResume, _ destinationPreflight) {
			if err := os.Rename(resume.stagingRoot, resume.stagingRoot+".moved"); err != nil {
				t.Error(err)
				return
			}
			if err := os.Symlink(outside, resume.stagingRoot); err != nil {
				t.Error(err)
			}
		},
	}
	_, err := fixture.client.Pull(t.Context(), fixture.options(destination))
	if err == nil || !IsCode(err, ErrorPreconditionFailed) {
		t.Fatalf("staging replacement error = %v, want %s", err, ErrorPreconditionFailed)
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("staging replacement published destination: %v", statErr)
	}
}

func TestPullRejectsDestinationParentReplacementBeforePublication(t *testing.T) {
	fixture := newPullFixture(t)
	grandparent := t.TempDir()
	parent := filepath.Join(grandparent, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(parent, "output")
	fixture.client.filesystemHooks = &filesystemTestHooks{
		beforePullPublish: func(_ *pullResume, _ destinationPreflight) {
			if err := os.Rename(parent, parent+".moved"); err != nil {
				t.Error(err)
				return
			}
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Error(err)
			}
		},
	}
	_, err := fixture.client.Pull(t.Context(), fixture.options(destination))
	if err == nil || !IsCode(err, ErrorPreconditionFailed) {
		t.Fatalf("parent replacement error = %v, want %s", err, ErrorPreconditionFailed)
	}
	for _, path := range []string{
		destination,
		filepath.Join(parent+".moved", "output"),
	} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("parent replacement published %q: %v", path, statErr)
		}
	}
}

func TestPullExactFinalVerificationRejectsStagingMutations(t *testing.T) {
	tests := map[string]func(*testing.T, *pullResume){
		"unexpected regular file": func(t *testing.T, resume *pullResume) {
			t.Helper()
			if err := os.WriteFile(
				filepath.Join(resume.dataRoot, "unexpected"),
				[]byte("unexpected"),
				0o600,
			); err != nil {
				t.Fatal(err)
			}
		},
		"unexpected FIFO": func(t *testing.T, resume *pullResume) {
			t.Helper()
			if err := syscall.Mkfifo(filepath.Join(resume.dataRoot, "unexpected"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"hardlink alias": func(t *testing.T, resume *pullResume) {
			t.Helper()
			second := filepath.Join(resume.dataRoot, "dir", "b.txt")
			if err := os.Remove(second); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(filepath.Join(resume.dataRoot, "a.txt"), second); err != nil {
				t.Fatal(err)
			}
		},
		"same-size content corruption": func(t *testing.T, resume *pullResume) {
			t.Helper()
			path := filepath.Join(resume.dataRoot, "a.txt")
			if err := os.WriteFile(path, []byte("HELLO"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newPullFixture(t)
			destination := filepath.Join(t.TempDir(), "output")
			fixture.client.filesystemHooks = &filesystemTestHooks{
				beforePullFinalVerify: func(resume *pullResume) {
					mutate(t, resume)
				},
			}
			_, err := fixture.client.Pull(t.Context(), fixture.options(destination))
			if err == nil || !IsCode(err, ErrorIntegrity) {
				t.Fatalf("final verification error = %v, want %s", err, ErrorIntegrity)
			}
			if _, statErr := os.Lstat(destination); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("final verification published destination: %v", statErr)
			}
		})
	}
}

func TestPullPublicationReverificationRejectsPostVerificationMutations(t *testing.T) {
	tests := map[string]func(*testing.T, *pullResume){
		"same-size content": func(t *testing.T, resume *pullResume) {
			t.Helper()
			if err := os.WriteFile(
				filepath.Join(resume.dataRoot, "a.txt"),
				[]byte("HELLO"),
				0o600,
			); err != nil {
				t.Fatal(err)
			}
		},
		"file identity": func(t *testing.T, resume *pullResume) {
			t.Helper()
			path := filepath.Join(resume.dataRoot, "dir", "b.txt")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(" world"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"file mode": func(t *testing.T, resume *pullResume) {
			t.Helper()
			if err := os.Chmod(filepath.Join(resume.dataRoot, "a.txt"), 0o400); err != nil {
				t.Fatal(err)
			}
		},
		"directory mode": func(t *testing.T, resume *pullResume) {
			t.Helper()
			path := filepath.Join(resume.dataRoot, "dir")
			t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
			if err := os.Chmod(path, 0o500); err != nil {
				t.Fatal(err)
			}
		},
		"symlink target": func(t *testing.T, resume *pullResume) {
			t.Helper()
			path := filepath.Join(resume.dataRoot, "link")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("dir/b.txt", path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newPullFixture(t)
			destination := filepath.Join(t.TempDir(), "output")
			fixture.client.filesystemHooks = &filesystemTestHooks{
				beforePullPublish: func(resume *pullResume, _ destinationPreflight) {
					mutate(t, resume)
				},
			}
			_, err := fixture.client.Pull(t.Context(), fixture.options(destination))
			if err == nil ||
				(!IsCode(err, ErrorIntegrity) && !IsCode(err, ErrorPreconditionFailed)) {
				t.Fatalf("publication verification error = %v", err)
			}
			if _, statErr := os.Lstat(destination); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("post-verification mutation published destination: %v", statErr)
			}
		})
	}
}

func hostPathsAlias(t *testing.T, first, second string, directory bool) bool {
	t.Helper()
	root := t.TempDir()
	firstPath := filepath.Join(root, first)
	secondPath := filepath.Join(root, second)
	if directory {
		if err := os.Mkdir(firstPath, 0o700); err != nil {
			t.Fatal(err)
		}
		err := os.Mkdir(secondPath, 0o700)
		if errors.Is(err, fs.ErrExist) {
			return true
		}
		if err != nil {
			t.Skipf("cannot probe host directory aliases: %v", err)
		}
	} else {
		if err := os.WriteFile(firstPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(secondPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			return true
		}
		if err != nil {
			t.Skipf("cannot probe host file aliases: %v", err)
		}
		file.Close()
	}
	firstInfo, err := os.Lstat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Lstat(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(firstInfo, secondInfo)
}

func TestPullRejectsHostFilesystemPathAliasesBeforeChunkWrites(t *testing.T) {
	tests := []struct {
		name      string
		first     string
		second    string
		directory bool
	}{
		{name: "case-folded files", first: "Alias", second: "alias"},
		{name: "normalized files", first: "\u00e9", second: "e\u0301"},
		{name: "case-folded directories", first: "Directory", second: "directory", directory: true},
		{name: "normalized directories", first: "\u00e9-dir", second: "e\u0301-dir", directory: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !hostPathsAlias(t, test.first, test.second, test.directory) {
				t.Skip("host filesystem keeps these names distinct")
			}
			content := []byte("x")
			chunkDigest := testFixtureDigest(content)
			var directories []directoryEntry
			var files []manifestFile
			totalSize := uint64(0)
			if test.directory {
				directories = []directoryEntry{
					{Mode: 0o700, Path: test.first},
					{Mode: 0o700, Path: test.second},
				}
			} else {
				totalSize = 2
				for _, path := range []string{test.first, test.second} {
					files = append(files, manifestFile{
						Kind: fileKindChunk,
						Chunk: chunkEntry{
							Digest: chunkDigest,
							Length: 1,
							Target: targetForDigest(chunkDigest),
						},
						Mode: 0o600,
						Path: path,
						Size: 1,
					})
				}
			}
			manifestBody := encodeManifest(
				totalSize,
				directories,
				files,
				nil,
				"local://fixture",
			)
			manifestDigest := testFixtureDigest(manifestBody)
			reader := &memoryObjectReader{objects: map[Digest]storedObject{
				manifestDigest: {
					body: manifestBody, kind: ObjectKindManifest, encoding: ObjectEncodingIdentity,
				},
				chunkDigest: {
					body: content, kind: ObjectKindChunk, encoding: ObjectEncodingIdentity,
				},
			}}
			_, err := newTestVolumeClient(t).Pull(t.Context(), PullOptions{
				ManifestDigest: manifestDigest,
				Objects:        reader,
				Destination:    filepath.Join(t.TempDir(), "output"),
			})
			if err == nil || !IsCode(err, ErrorIntegrity) {
				t.Fatalf("host alias error = %v, want %s", err, ErrorIntegrity)
			}
			if got := chunkReadCount(reader); got != 0 {
				t.Fatalf("host alias reached concurrent chunk writes after %d reads", got)
			}
		})
	}
}

func onlyPullStaging(t *testing.T, parent string) string {
	t.Helper()
	var matches []string
	for _, entry := range mustReadDir(t, parent) {
		if strings.HasPrefix(entry.Name(), pullStagingPrefix) &&
			strings.HasSuffix(entry.Name(), pullStagingSuffix) {
			matches = append(matches, filepath.Join(parent, entry.Name()))
		}
	}
	if len(matches) != 1 {
		t.Fatalf("pull staging paths = %v, want exactly one", matches)
	}
	return matches[0]
}

func mustReadDir(t *testing.T, path string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
