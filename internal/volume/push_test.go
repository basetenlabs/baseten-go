package volume

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type uploadedObject struct {
	kind ObjectKind
	body []byte
}

type memoryUploadSession struct {
	mu sync.Mutex

	objects           map[Digest]uploadedObject
	missingBatches    []int
	published         []Digest
	publication       PublishResult
	beforeMissingDone func()
	missingErr        error
	uploadErr         error
	publishErr        error
	uploadAttempts    int
	publishAttempts   int
	activeUploads     int
	maxActiveUploads  int
}

func newMemoryUploadSession() *memoryUploadSession {
	return &memoryUploadSession{
		objects:     make(map[Digest]uploadedObject),
		publication: PublishResult{Outcome: PublishOutcomePublished},
	}
}

func (session *memoryUploadSession) MissingObjects(
	ctx context.Context,
	digests []Digest,
) ([]Digest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	session.mu.Lock()
	session.missingBatches = append(session.missingBatches, len(digests))
	err := session.missingErr
	callback := session.beforeMissingDone
	session.beforeMissingDone = nil
	var missing []Digest
	for _, digest := range digests {
		if _, exists := session.objects[digest]; !exists {
			missing = append(missing, digest)
		}
	}
	session.mu.Unlock()
	if callback != nil {
		callback()
	}
	if err != nil {
		return nil, err
	}
	return missing, nil
}

func (session *memoryUploadSession) UploadObject(
	ctx context.Context,
	object UploadObject,
) (UploadObjectResult, error) {
	if err := ctx.Err(); err != nil {
		return UploadObjectResult{}, err
	}
	session.mu.Lock()
	session.uploadAttempts++
	session.activeUploads++
	session.maxActiveUploads = max(session.maxActiveUploads, session.activeUploads)
	err := session.uploadErr
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.activeUploads--
		session.mu.Unlock()
	}()
	if err != nil {
		return UploadObjectResult{}, err
	}
	body, err := io.ReadAll(object.Body)
	if err != nil {
		return UploadObjectResult{}, err
	}
	if uint64(len(body)) != object.Size {
		return UploadObjectResult{}, errors.New("upload size mismatch")
	}
	if testFixtureDigest(body) != object.Digest {
		return UploadObjectResult{}, errors.New("upload digest mismatch")
	}
	session.mu.Lock()
	_, exists := session.objects[object.Digest]
	if !exists {
		session.objects[object.Digest] = uploadedObject{
			kind: object.Kind,
			body: append([]byte(nil), body...),
		}
	}
	session.mu.Unlock()
	return UploadObjectResult{Created: !exists}, nil
}

func (session *memoryUploadSession) Publish(
	ctx context.Context,
	manifest Digest,
) (PublishResult, error) {
	if err := ctx.Err(); err != nil {
		return PublishResult{
			Outcome:       PublishOutcomeNotPublished,
			FailureReason: PublishFailureReasonCanceled,
		}, err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	session.publishAttempts++
	if session.publishErr != nil {
		return session.publication, session.publishErr
	}
	object, ok := session.objects[manifest]
	if !ok || object.kind != ObjectKindManifest {
		return PublishResult{
			Outcome:       PublishOutcomeNotPublished,
			FailureReason: PublishFailureReasonRejected,
		}, errors.New("manifest unavailable")
	}
	session.published = append(session.published, manifest)
	return session.publication, nil
}

func assertPhaseLocalProgress(
	t *testing.T,
	events []ProgressEvent,
	operation Operation,
) {
	t.Helper()
	phaseOrder := map[ProgressPhase]int{
		ProgressScan:     0,
		ProgressHash:     1,
		ProgressUpload:   2,
		ProgressValidate: 0,
		ProgressDownload: 1,
		ProgressVerify:   2,
		ProgressPublish:  3,
	}
	var previous ProgressEvent
	for index, event := range events {
		if event.Operation != operation {
			t.Fatalf("progress[%d] operation = %q, want %q", index, event.Operation, operation)
		}
		if event.TotalItems != nil && event.CompletedItems > *event.TotalItems {
			t.Fatalf("progress[%d] items = %d/%d", index, event.CompletedItems, *event.TotalItems)
		}
		if event.TotalBytes != nil && event.CompletedBytes > *event.TotalBytes {
			t.Fatalf("progress[%d] bytes = %d/%d", index, event.CompletedBytes, *event.TotalBytes)
		}
		if index > 0 {
			if phaseOrder[event.Phase] < phaseOrder[previous.Phase] {
				t.Fatalf("progress phase regressed from %q to %q", previous.Phase, event.Phase)
			}
			if event.Phase == previous.Phase &&
				(event.CompletedItems < previous.CompletedItems ||
					event.CompletedBytes < previous.CompletedBytes) {
				t.Fatalf("progress regressed within %q: before %+v after %+v", event.Phase, previous, event)
			}
		}
		previous = event
	}
}

func TestPushTransfersDeterministicContentThroughSession(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	large := make([]byte, ChunkSize+3)
	for index := range large {
		large[index] = byte(index % 251)
	}
	if err := os.WriteFile(filepath.Join(root, "large.bin"), large, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "small.txt"), []byte("small"), 0o640); err != nil {
		t.Fatal(err)
	}
	wantSymlinks := 0
	if platformSourceSymlinkPolicy == sourceSymlinksPreserved {
		if err := os.Symlink("../large.bin", filepath.Join(root, "dir", "large-link")); err != nil {
			t.Fatal(err)
		}
		wantSymlinks = 1
	}

	sequence := uint64(7)
	changed := true
	session := newMemoryUploadSession()
	session.publication = PublishResult{
		Outcome:        PublishOutcomePublished,
		Sequence:       &sequence,
		PointerChanged: &changed,
	}
	var progressMu sync.Mutex
	var progress []ProgressEvent
	result, err := newTestVolumeClient(t).Push(t.Context(), PushOptions{
		Path:     root,
		Session:  session,
		Uploader: session,
		Progress: func(event ProgressEvent) {
			progressMu.Lock()
			defer progressMu.Unlock()
			progress = append(progress, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Publication.Sequence == nil ||
		*result.Publication.Sequence != sequence ||
		result.FileCount != 2 ||
		result.DirectoryCount != 1 ||
		result.LogicalBytes != uint64(len(large)+len("small")) ||
		result.UploadedBytes != result.LogicalBytes ||
		result.ReusedBytes != 0 ||
		!result.ContentCreated {
		t.Fatalf("unexpected result: %+v", result)
	}

	session.mu.Lock()
	if len(session.objects) != 5 {
		t.Fatalf("uploaded object count = %d, want 5", len(session.objects))
	}
	if len(session.published) != 1 || session.published[0] != result.ManifestDigest {
		t.Fatalf("published manifests = %v", session.published)
	}
	if session.maxActiveUploads > 4 {
		t.Fatalf("max active uploads = %d, want at most 4", session.maxActiveUploads)
	}
	var manifestBody, chunkmapBody []byte
	for _, object := range session.objects {
		switch object.kind {
		case ObjectKindManifest:
			manifestBody = append([]byte(nil), object.body...)
		case ObjectKindChunkmap:
			chunkmapBody = append([]byte(nil), object.body...)
		case ObjectKindChunk:
		default:
			t.Fatalf("unexpected object kind %q", object.kind)
		}
	}
	session.mu.Unlock()
	if strings.Contains(string(manifestBody), root) ||
		strings.Contains(string(manifestBody), "file://") {
		t.Fatalf("manifest provenance exposed local source path: %s", manifestBody)
	}
	if !strings.Contains(string(manifestBody), `"source_uri":"`+defaultLocalSourceURI+`"`) {
		t.Fatalf("manifest provenance omitted safe source identifier: %s", manifestBody)
	}
	manifest, err := decodeManifest(manifestBody, uint64(len(manifestBody)), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 2 || len(manifest.Symlinks) != wantSymlinks {
		t.Fatalf("manifest graph = %+v", manifest)
	}
	if _, err := decodeChunkmap(
		chunkmapBody,
		uint64(len(chunkmapBody)),
		uint64(len(large)),
		10,
	); err != nil {
		t.Fatal(err)
	}
	if len(progress) == 0 || progress[len(progress)-1].Phase != ProgressPublish {
		t.Fatalf("progress = %+v", progress)
	}
	assertPhaseLocalProgress(t, progress, OperationPush)
}

func TestPushReusesObjectsAcrossSessions(t *testing.T) {
	source := t.TempDir()
	content := []byte("stateless reuse")
	if err := os.WriteFile(filepath.Join(source, "weights.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	client := newTestVolumeClient(t)
	session := newMemoryUploadSession()
	first, err := client.Push(t.Context(), PushOptions{
		Path: source, Session: session, Uploader: session,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Push(t.Context(), PushOptions{
		Path: source, Session: session, Uploader: session,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ManifestDigest != first.ManifestDigest ||
		second.UploadedBytes != 0 ||
		second.ReusedBytes != uint64(len(content)) ||
		second.ContentCreated {
		t.Fatalf("second push = %+v, first = %+v", second, first)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.published) != 2 {
		t.Fatalf("publish calls = %d, want 2", len(session.published))
	}
}

func TestMissingInventoryIsBatchedAndValidated(t *testing.T) {
	session := newMemoryUploadSession()
	objects := make(map[Digest]*pushObject)
	for index := range 5000 {
		var digest Digest
		binary.LittleEndian.PutUint64(digest[:8], uint64(index))
		objects[digest] = &pushObject{digest: digest}
	}
	missing, err := missingObjects(t.Context(), session, objects)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != len(objects) {
		t.Fatalf("missing objects = %d, want %d", len(missing), len(objects))
	}
	session.mu.Lock()
	if len(session.missingBatches) != 2 ||
		session.missingBatches[0] != MaxMissingDigests ||
		session.missingBatches[1] != 904 {
		t.Fatalf("missing batch sizes = %v", session.missingBatches)
	}
	session.mu.Unlock()

	unrequested := testDigest(0xfe)
	bad := &badMissingSession{digest: unrequested}
	_, err = missingObjects(t.Context(), bad, map[Digest]*pushObject{
		testDigest(1): {digest: testDigest(1)},
	})
	if err == nil || !IsCode(err, ErrorProtocol) {
		t.Fatalf("unrequested digest error = %v, want %s", err, ErrorProtocol)
	}
}

type badMissingSession struct {
	digest Digest
}

func (session *badMissingSession) MissingObjects(context.Context, []Digest) ([]Digest, error) {
	return []Digest{session.digest}, nil
}

func (*badMissingSession) UploadObject(context.Context, UploadObject) (UploadObjectResult, error) {
	panic("unexpected upload")
}

func (*badMissingSession) Publish(context.Context, Digest) (PublishResult, error) {
	panic("unexpected publication")
}

func TestBuildPushPlanBoundsGraphsBeforeUnboundedWork(t *testing.T) {
	t.Run("aggregate chunks", func(t *testing.T) {
		root := t.TempDir()
		for _, name := range []string{"a", "b"} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		inputs, err := collectPushInputs(t.Context(), root, 10)
		if err != nil {
			t.Fatal(err)
		}
		defer inputs.close()
		client := newTestVolumeClient(t)
		client.maxManifestBytes = contentGraphChunkBudgetBytes
		_, err = client.buildPushPlan(
			t.Context(),
			inputs,
			newProgressReporter(OperationPush, nil, nil),
		)
		if err == nil || !IsCode(err, ErrorPreconditionFailed) {
			t.Fatalf("aggregate chunk error = %v, want %s", err, ErrorPreconditionFailed)
		}
	})

	t.Run("per-file fanout", func(t *testing.T) {
		client := newTestVolumeClient(t)
		client.maxManifestBytes = chunkmapFanoutBudgetBytes
		_, err := client.buildPushPlan(
			t.Context(),
			pushInputs{files: []sourceFile{{
				relativePath: "large",
				snapshot:     sourceSnapshot{size: 2 * ChunkSize},
			}}},
			newProgressReporter(OperationPush, nil, nil),
		)
		if err == nil || !IsCode(err, ErrorPreconditionFailed) {
			t.Fatalf("fanout error = %v, want %s", err, ErrorPreconditionFailed)
		}
	})

	if got := sourceChunkCount(^uint64(0)); got == 0 {
		t.Fatal("maximum source size overflowed its chunk count")
	}
}

func TestPushDetectsMutationBetweenPlanAndUpload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := newMemoryUploadSession()
	session.beforeMissingDone = func() {
		if err := os.WriteFile(path, []byte("after!"), 0o600); err != nil {
			t.Error(err)
		}
	}
	_, err := newTestVolumeClient(t).Push(t.Context(), PushOptions{
		Path:     filepath.Dir(path),
		Session:  session,
		Uploader: session,
	})
	if err == nil || !IsCode(err, ErrorPreconditionFailed) {
		t.Fatalf("source mutation error = %v, want %s", err, ErrorPreconditionFailed)
	}
}

func TestPushCancellationAndBoundaryErrorsAreTypedAndRedacted(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		session := newMemoryUploadSession()
		_, err := newTestVolumeClient(t).Push(ctx, PushOptions{
			Path:     t.TempDir(),
			Session:  session,
			Uploader: session,
		})
		if err == nil || !IsCode(err, ErrorCanceled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	})

	t.Run("publication redaction", func(t *testing.T) {
		session := newMemoryUploadSession()
		session.publication = PublishResult{
			Outcome:       PublishOutcomeUnknown,
			FailureReason: PublishFailureReasonTransportFailure,
		}
		session.publishErr = errors.New("https://storage.invalid/key?credential=secret")
		result, err := newTestVolumeClient(t).Push(t.Context(), PushOptions{
			Path:     t.TempDir(),
			Session:  session,
			Uploader: session,
		})
		var publicationErr *PushPublicationError
		if err == nil ||
			result == nil ||
			!errors.As(err, &publicationErr) ||
			!IsCode(err, ErrorPublicationUnknown) ||
			publicationErr.Result != result ||
			publicationErr.RetrySafe() {
			t.Fatalf("publication error = result %+v, error %v", result, err)
		}
		session.mu.Lock()
		attempts := session.publishAttempts
		session.mu.Unlock()
		if attempts != 1 {
			t.Fatalf("publication attempts = %d, want no implicit replay", attempts)
		}
		for _, forbidden := range []string{"storage.invalid", "credential", "secret", "/key"} {
			if strings.Contains(err.Error(), forbidden) {
				t.Fatalf("error leaked %q: %s", forbidden, err)
			}
		}
	})

	t.Run("object upload redaction", func(t *testing.T) {
		session := newMemoryUploadSession()
		session.uploadErr = errors.New("https://storage.invalid/key?credential=secret")
		_, err := newTestVolumeClient(t).Push(t.Context(), PushOptions{
			Path:     t.TempDir(),
			Session:  session,
			Uploader: session,
		})
		if err == nil || !IsCode(err, ErrorTransfer) {
			t.Fatalf("object upload error = %v, want %s", err, ErrorTransfer)
		}
		session.mu.Lock()
		uploadAttempts := session.uploadAttempts
		publishAttempts := session.publishAttempts
		session.mu.Unlock()
		if uploadAttempts != 1 || publishAttempts != 0 {
			t.Fatalf(
				"boundary attempts = upload %d publish %d, want 1 and 0",
				uploadAttempts,
				publishAttempts,
			)
		}
		for _, forbidden := range []string{"storage.invalid", "credential", "secret", "/key"} {
			if strings.Contains(err.Error(), forbidden) {
				t.Fatalf("error leaked %q: %s", forbidden, err)
			}
		}
	})
}

func TestPushPublicationOutcomeContract(t *testing.T) {
	push := func(t *testing.T, session *memoryUploadSession) (*PushResult, error) {
		t.Helper()
		return newTestVolumeClient(t).Push(t.Context(), PushOptions{
			Path:     t.TempDir(),
			Session:  session,
			Uploader: session,
		})
	}
	assertRedacted := func(t *testing.T, err error) {
		t.Helper()
		rendered := fmt.Sprintf("%#v", err)
		for _, forbidden := range []string{"storage.invalid", "credential", "secret", "/publish"} {
			if strings.Contains(rendered, forbidden) {
				t.Fatalf("publication error leaked %q: %s", forbidden, rendered)
			}
		}
	}

	t.Run("definite success", func(t *testing.T) {
		sequence := uint64(11)
		changed := true
		session := newMemoryUploadSession()
		session.publication = PublishResult{
			Outcome:        PublishOutcomePublished,
			Sequence:       &sequence,
			PointerChanged: &changed,
		}
		result, err := push(t, session)
		if err != nil ||
			result == nil ||
			result.Publication.Outcome != PublishOutcomePublished ||
			result.Publication.Sequence == nil ||
			*result.Publication.Sequence != sequence {
			t.Fatalf("definite publication = result %+v, error %v", result, err)
		}
	})

	t.Run("definite failure is retry safe", func(t *testing.T) {
		session := newMemoryUploadSession()
		session.publication = PublishResult{
			Outcome:       PublishOutcomeNotPublished,
			FailureReason: PublishFailureReasonRejected,
		}
		session.publishErr = errors.New(
			"https://storage.invalid/publish?credential=secret",
		)
		result, err := push(t, session)
		var publicationErr *PushPublicationError
		if result != nil ||
			!errors.As(err, &publicationErr) ||
			!IsCode(err, ErrorPublicationFailed) ||
			publicationErr.Result != nil ||
			!publicationErr.RetrySafe() ||
			publicationErr.PublicationMayHaveHappened() ||
			publicationErr.Publication.Outcome != PublishOutcomeNotPublished ||
			publicationErr.Publication.FailureReason != PublishFailureReasonRejected {
			t.Fatalf("definite failure = result %+v, error %v", result, err)
		}
		assertRedacted(t, err)
	})

	t.Run("reconciled success permits unavailable optional fields", func(t *testing.T) {
		session := newMemoryUploadSession()
		session.publication = PublishResult{
			Outcome:    PublishOutcomePublished,
			Reconciled: true,
		}
		result, err := push(t, session)
		if err != nil ||
			result == nil ||
			!result.Publication.Reconciled ||
			result.Publication.Sequence != nil ||
			result.Publication.PointerChanged != nil {
			t.Fatalf("reconciled publication = result %+v, error %v", result, err)
		}
	})

	t.Run("unknown outcome preserves result and canonical cancellation", func(t *testing.T) {
		session := newMemoryUploadSession()
		session.publication = PublishResult{
			Outcome:       PublishOutcomeUnknown,
			FailureReason: PublishFailureReasonTransportFailure,
		}
		session.publishErr = fmt.Errorf(
			"https://storage.invalid/publish?credential=secret: %w",
			context.DeadlineExceeded,
		)
		result, err := push(t, session)
		var publicationErr *PushPublicationError
		if result == nil ||
			!errors.As(err, &publicationErr) ||
			!IsCode(err, ErrorPublicationUnknown) ||
			!errors.Is(err, context.DeadlineExceeded) ||
			publicationErr.Result != result ||
			publicationErr.RetrySafe() ||
			!publicationErr.PublicationMayHaveHappened() ||
			result.Publication.Outcome != PublishOutcomeUnknown ||
			result.Publication.FailureReason != PublishFailureReasonCanceled {
			t.Fatalf("unknown publication = result %+v, error %v", result, err)
		}
		if normalized := errors.Unwrap(publicationErr); normalized == nil ||
			errors.Unwrap(normalized) != context.DeadlineExceeded {
			t.Fatalf("publication cause chain retained noncanonical state: %v", normalized)
		}
		assertRedacted(t, err)
	})

	t.Run("malformed retry-safe result fails closed", func(t *testing.T) {
		session := newMemoryUploadSession()
		session.publication = PublishResult{Outcome: PublishOutcomeNotPublished}
		result, err := push(t, session)
		var publicationErr *PushPublicationError
		if result == nil ||
			!errors.As(err, &publicationErr) ||
			!IsCode(err, ErrorPublicationUnknown) ||
			publicationErr.RetrySafe() ||
			publicationErr.Publication.Outcome != PublishOutcomeUnknown ||
			publicationErr.Publication.FailureReason != PublishFailureReasonInvalidResult {
			t.Fatalf("malformed publication = result %+v, error %v", result, err)
		}
	})
}

func TestNewRejectsNonBLAKE3SizedHash(t *testing.T) {
	_, err := New(Options{NewHasher: sha1.New, Decoder: copyDecoder{}})
	if err == nil || !IsCode(err, ErrorInvalidArgument) {
		t.Fatalf("short hash error = %v, want %s", err, ErrorInvalidArgument)
	}
}

func TestUploadBodyReaderIsSynchronous(t *testing.T) {
	session := newMemoryUploadSession()
	result, err := newTestVolumeClient(t).Push(t.Context(), PushOptions{
		Path:     t.TempDir(),
		Session:  session,
		Uploader: session,
	})
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	manifest := session.objects[result.ManifestDigest]
	session.mu.Unlock()
	if !bytes.HasSuffix(manifest.body, []byte{'\n'}) {
		t.Fatal("session did not synchronously consume the canonical manifest")
	}
}

func TestPushOmitsEmptyChunkObject(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := New(Options{NewHasher: newTestHasher})
	if err != nil {
		t.Fatal(err)
	}
	session := newMemoryUploadSession()
	result, err := client.Push(t.Context(), PushOptions{
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
	if session.uploadAttempts != 1 || len(session.objects) != 1 {
		t.Fatalf(
			"empty push transferred %d objects and retained %d, want only the manifest",
			session.uploadAttempts,
			len(session.objects),
		)
	}
	if object := session.objects[result.ManifestDigest]; object.kind != ObjectKindManifest {
		t.Fatalf("published object kind = %q, want %q", object.kind, ObjectKindManifest)
	}
}

func TestPushRejectsCrossKindDigestAliasesBeforeBoundaries(t *testing.T) {
	source := t.TempDir()
	payloadPath := filepath.Join(source, "payload.bin")
	payload := make([]byte, ChunkSize+1)
	for index := range payload {
		payload[index] = byte(index % 251)
	}
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	client := newTestVolumeClient(t)
	inputs, err := collectPushInputs(t.Context(), source, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer inputs.close()
	if len(inputs.files) != 1 || inputs.files[0].relativePath != "payload.bin" {
		t.Fatalf("push inputs = %+v, want payload.bin", inputs.files)
	}
	chunks, err := client.hashSourceFile(t.Context(), inputs.files[0])
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]chunkEntry, 0, len(chunks))
	for _, chunk := range chunks {
		entries = append(entries, chunkEntry{
			Digest: chunk.digest,
			Length: chunk.length,
			Offset: chunk.offset,
			Target: targetForDigest(chunk.digest),
		})
	}
	canonicalChunkmap := encodeChunkmap(uint64(len(payload)), entries)
	if err := os.WriteFile(
		filepath.Join(source, "raw-chunkmap.jsonl"),
		canonicalChunkmap,
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	session := newMemoryUploadSession()
	_, err = client.Push(t.Context(), PushOptions{
		Path:     source,
		Session:  session,
		Uploader: session,
	})
	if err == nil || !IsCode(err, ErrorPreconditionFailed) {
		t.Fatalf("cross-kind alias error = %v, want %s", err, ErrorPreconditionFailed)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.missingBatches) != 0 ||
		session.uploadAttempts != 0 ||
		session.publishAttempts != 0 {
		t.Fatalf(
			"cross-kind alias reached boundaries: inventory %v upload %d publish %d",
			session.missingBatches,
			session.uploadAttempts,
			session.publishAttempts,
		)
	}
}

func TestBoundaryCancellationUnwrapsOnlyCanonicalSentinel(t *testing.T) {
	tests := []struct {
		name     string
		sentinel error
	}{
		{name: "canceled", sentinel: context.Canceled},
		{name: "deadline", sentinel: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := newMemoryUploadSession()
			session.missingErr = fmt.Errorf(
				"https://signed.invalid/object?credential=secret: %w",
				test.sentinel,
			)
			_, err := newTestVolumeClient(t).Push(t.Context(), PushOptions{
				Path:     t.TempDir(),
				Session:  session,
				Uploader: session,
			})
			if err == nil ||
				!IsCode(err, ErrorCanceled) ||
				!errors.Is(err, test.sentinel) {
				t.Fatalf("wrapped cancellation error = %v", err)
			}
			if unwrapped := errors.Unwrap(err); unwrapped != test.sentinel {
				t.Fatalf("unwrapped error = %v, want exact canonical sentinel", unwrapped)
			}
			rendered := fmt.Sprintf("%+v", err)
			for _, secret := range []string{"signed.invalid", "credential", "secret", "/object"} {
				if strings.Contains(rendered, secret) {
					t.Fatalf("cancellation leaked %q: %s", secret, rendered)
				}
			}
		})
	}
}

func TestNewRejectsInsufficientExplicitMemoryLimit(t *testing.T) {
	maxManifestBytes := uint64(ChunkSize)
	minimum := maxManifestBytes + decodeLimits(maxManifestBytes).MaxMemoryBytes
	_, err := New(Options{
		NewHasher:        newTestHasher,
		MaxManifestBytes: maxManifestBytes,
		MaxBytesInFlight: minimum - 1,
	})
	if err == nil ||
		!IsCode(err, ErrorInvalidArgument) ||
		!strings.Contains(err.Error(), fmt.Sprintf("%d", minimum)) {
		t.Fatalf("insufficient memory error = %v, want minimum %d", err, minimum)
	}
}

func TestNewConfiguresPortablePathLimitsWithoutClamping(t *testing.T) {
	client, err := New(Options{
		NewHasher:                 newTestHasher,
		MaxPortablePathBytes:      17,
		MaxPortablePathComponents: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.portablePathLimits.maxPathBytes != 17 ||
		client.portablePathLimits.maxPathComponents != 3 {
		t.Fatalf("portable path limits = %+v", client.portablePathLimits)
	}
	maximumClient, err := New(Options{
		NewHasher:                 newTestHasher,
		MaxPortablePathComponents: maximumPortablePathComponents,
	})
	if err != nil {
		t.Fatalf("maximum portable path component limit error = %v", err)
	}
	if maximumClient.portablePathLimits.maxPathComponents != maximumPortablePathComponents {
		t.Fatalf(
			"maximum portable path component limit = %d, want %d",
			maximumClient.portablePathLimits.maxPathComponents,
			maximumPortablePathComponents,
		)
	}

	for _, options := range []Options{
		{NewHasher: newTestHasher, MaxPortablePathBytes: -1},
		{NewHasher: newTestHasher, MaxPortablePathComponents: -1},
		{
			NewHasher:                 newTestHasher,
			MaxPortablePathComponents: maximumPortablePathComponents + 1,
		},
	} {
		if _, err := New(options); err == nil || !IsCode(err, ErrorInvalidArgument) {
			t.Fatalf("invalid portable path limit error = %v", err)
		}
	}
}

func TestPushScanEnforcesDepthAndComposedSymlinkSafety(t *testing.T) {
	t.Run("deep tree", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := collectPushInputsWithHook(
			t.Context(),
			root,
			10,
			portablePathLimits{maxPathBytes: 64, maxPathComponents: 2},
			nil,
		)
		if err == nil || !IsCode(err, ErrorPreconditionFailed) {
			t.Fatalf("deep-tree scan error = %v, want %s", err, ErrorPreconditionFailed)
		}
	})

	t.Run("composed symlink escape", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../outside", filepath.Join(root, "dir", "jump")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("dir/jump/../..", filepath.Join(root, "escape")); err != nil {
			t.Fatal(err)
		}
		if _, err := collectPushInputs(t.Context(), root, 10); err == nil ||
			!IsCode(err, ErrorInvalidArgument) {
			t.Fatalf("composed escape scan error = %v, want %s", err, ErrorInvalidArgument)
		}
	})

	t.Run("safe dangling symlink", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Symlink("missing/child", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		inputs, err := collectPushInputs(t.Context(), root, 10)
		if err != nil {
			t.Fatal(err)
		}
		defer inputs.close()
		if len(inputs.symlinks) != 1 || inputs.symlinks[0].Target != "missing/child" {
			t.Fatalf("safe dangling symlinks = %+v", inputs.symlinks)
		}
	})
}

func TestPushOnlyClientDoesNotRequireDecoder(t *testing.T) {
	client, err := New(Options{NewHasher: newTestHasher})
	if err != nil {
		t.Fatal(err)
	}
	session := newMemoryUploadSession()
	if _, err := client.Push(t.Context(), PushOptions{
		Path:     t.TempDir(),
		Session:  session,
		Uploader: session,
	}); err != nil {
		t.Fatal(err)
	}
}
