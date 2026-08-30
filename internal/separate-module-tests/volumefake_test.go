package separatemoduletests_test

// A fake volume service, backed by an in-memory object store, that the push
// and pull tests run against end to end with a real BLAKE3 and a real zstd.
// Everything the protocol depends on is exercised here: the digest the server
// echoes, the target it chooses, the storage encoding a reader has to discover
// from the media type, and the commit that makes any of it visible.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/blake3"
)

const (
	fakeOrg       = "org1"
	fakeNamespace = "models"
	fakeVolume    = "gpt2"
	fakeBucket    = "bdn-origin"
)

func newBlake3() hash.Hash { return blake3.New() }

// newZstdReader is the decompression seam the transfer engines take.
func newZstdReader(r io.Reader) (io.ReadCloser, error) {
	decoder, err := zstd.NewReader(r)
	if err != nil {
		return nil, err
	}
	return decoder.IOReadCloser(), nil
}

func zstdCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	return encoder.EncodeAll(data, nil)
}

// storedObject is one object as the service kept it: the bytes on disk plus
// the media type that says how they are encoded.
type storedObject struct {
	body        []byte
	contentType string
}

type commitRecord struct {
	manifestDigest string
	updateHead     bool
	tags           []string
	idempotencyKey string
}

// fakeService is a volume service and its object store.
type fakeService struct {
	t      *testing.T
	server *httptest.Server

	mu      sync.Mutex
	objects map[string]storedObject // keyed by digest
	uploads []uploadRecord
	commits []commitRecord

	// head is the manifest digest a resolve without a tag returns. Empty means
	// the volume has no version yet.
	head string

	// compressChunks stores chunk objects zstd-compressed, which the service
	// decides per object and a reader can only learn from the media type.
	compressChunks bool

	// failNextUploads makes the next n object uploads answer 503, to exercise
	// the retry path against a real client.
	failNextUploads int

	// leaseTTL is how long the credentials a resolve hands out are good for.
	// Zero issues none at all, which is what local development does.
	leaseTTL time.Duration

	// resolves counts calls, so a test can tell one renewal from a storm.
	resolves int

	// onUpload, when set, runs before each object upload is handled, so a test
	// can interfere partway through a transfer.
	onUpload func()
}

type uploadRecord struct {
	digest      string
	contentType string
	size        int
}

func newFakeService(t *testing.T) *fakeService {
	t.Helper()
	f := &fakeService{t: t, objects: map[string]storedObject{}}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/volumes/{namespace}/{volume}/uploads", f.handleBeginUpload)
	mux.HandleFunc("PUT /v1/volumes/{namespace}/{volume}/uploads/{id}/objects/{digest}", f.handleUpload)
	mux.HandleFunc("POST /v1/volumes/{namespace}/{volume}/uploads/{id}/commit", f.handleCommit)
	mux.HandleFunc("POST /v1/volumes/resolve", f.handleResolve)

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// client builds a protocol client pointed at the fake.
func (f *fakeService) client(t *testing.T) *bdn.Client {
	t.Helper()
	client, err := bdn.New(bdn.Options{
		HTTPClient: f.server.Client(),
		Tokens: func(context.Context, string) (string, string, error) {
			return f.token(), f.server.URL, nil
		},
		Retry: bdn.RetryConfig{MaxAttempts: 5, Base: time.Millisecond, Cap: 2 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// token returns a capability token granting everything on the fake's volume,
// so the client's own reading of its grants matches what the fake allows.
func (f *fakeService) token() string {
	return makeGrantToken(fakeOrg, fakeNamespace, fakeVolume, "*", "pull", "push", "tag")
}

func (f *fakeService) handleBeginUpload(w http.ResponseWriter, r *http.Request) {
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"upload_id": "upload-1",
		"object_upload_path": fmt.Sprintf("/v1/volumes/%s/%s/uploads/upload-1/objects/{digest}",
			r.PathValue("namespace"), r.PathValue("volume")),
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		"org_id":     fakeOrg,
		"namespace":  r.PathValue("namespace"),
		"volume":     r.PathValue("volume"),
	})
}

func (f *fakeService) handleUpload(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	hook := f.onUpload
	f.mu.Unlock()
	if hook != nil {
		hook()
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeServiceError(w, http.StatusInternalServerError, "INTERNAL", "INTERNAL", err.Error())
		return
	}

	f.mu.Lock()
	if f.failNextUploads > 0 {
		f.failNextUploads--
		f.mu.Unlock()
		writeServiceError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "UNAVAILABLE", "shedding")
		return
	}
	f.mu.Unlock()

	// The service verifies the bytes against the digest in the path before it
	// stores anything, which is what makes a re-upload safe.
	claimed := r.PathValue("digest")
	sum := blake3.Sum256(body)
	actual := volume.Digest(sum)
	if claimed != actual.String() {
		writeServiceError(w, http.StatusBadRequest, "FAILED_PRECONDITION", "DIGEST_MISMATCH",
			"body hashes to "+actual.String())
		return
	}
	contentType := r.Header.Get("Content-Type")
	if contentType == bdn.ContentTypeChunk && len(body) > volume.ChunkSize {
		writeServiceError(w, http.StatusRequestEntityTooLarge, "OUT_OF_RANGE", "CHUNK_TOO_LARGE", "too big")
		return
	}

	f.mu.Lock()
	f.uploads = append(f.uploads, uploadRecord{digest: claimed, contentType: contentType, size: len(body)})
	_, existed := f.objects[claimed]
	if !existed {
		f.objects[claimed] = f.store(contentType, body)
	}
	f.mu.Unlock()

	writeJSONResponse(w, http.StatusOK, map[string]any{
		"digest":  claimed,
		"target":  volume.TargetForDigest(actual),
		"created": !existed,
	})
}

// store applies the storage policy: metadata is always compressed, and a chunk
// is compressed or not at the service's discretion. Either way the media type
// records what was done, because nothing else does.
func (f *fakeService) store(contentType string, body []byte) storedObject {
	switch contentType {
	case bdn.ContentTypeChunkmap:
		return storedObject{body: zstdCompress(f.t, body), contentType: bdn.ContentTypeChunkmapZstd}
	case bdn.ContentTypeManifest:
		return storedObject{body: zstdCompress(f.t, body), contentType: bdn.ContentTypeManifestZstd}
	default:
		if f.compressChunks {
			return storedObject{body: zstdCompress(f.t, body), contentType: bdn.ContentTypeChunkZstd}
		}
		return storedObject{body: body, contentType: bdn.ContentTypeChunk}
	}
}

func (f *fakeService) handleCommit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ManifestDigest string   `json:"manifest_digest"`
		UpdateHead     bool     `json:"update_head"`
		Tags           []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeServiceError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "MALFORMED_BODY", err.Error())
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeServiceError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "MISSING_HEADER", "Idempotency-Key")
		return
	}

	f.mu.Lock()
	if _, ok := f.objects[body.ManifestDigest]; !ok {
		f.mu.Unlock()
		writeServiceError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "NOT_FOUND", "manifest not uploaded")
		return
	}
	f.commits = append(f.commits, commitRecord{
		manifestDigest: body.ManifestDigest,
		updateHead:     body.UpdateHead,
		tags:           body.Tags,
		idempotencyKey: key,
	})
	if body.UpdateHead {
		f.head = body.ManifestDigest
	}
	sequence := len(f.commits)
	f.mu.Unlock()

	writeJSONResponse(w, http.StatusOK, map[string]any{
		"manifest_digest": body.ManifestDigest,
		"sequence":        sequence,
		"head_updated":    body.UpdateHead,
		"tag_applied":     len(body.Tags) > 0,
	})
}

func (f *fakeService) handleResolve(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")

	// The real server requires the scheme and rejects a ref without it. The
	// fake enforces the same rule, because a client that built a bare
	// "namespace/volume" would otherwise pass every test here and fail
	// silently against the real service — which is exactly what happened to
	// the delta-reuse path.
	if !strings.HasPrefix(ref, "bdn://") {
		writeServiceError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "INVALID_REF",
			"ref must start with bdn://")
		return
	}
	ref = strings.TrimPrefix(ref, "bdn://")

	f.mu.Lock()
	digest := f.head
	if at := strings.LastIndexByte(ref, '@'); at >= 0 {
		digest = ref[at+1:]
	}
	f.mu.Unlock()

	if digest == "" {
		writeServiceError(w, http.StatusNotFound, "NOT_FOUND", "NOT_FOUND", "no version")
		return
	}
	parsed, err := volume.ParseDigest(digest)
	if err != nil {
		writeServiceError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "INVALID_DIGEST", err.Error())
		return
	}
	resolvedFrom := "head"
	if strings.Contains(ref, "@") {
		resolvedFrom = "pin"
	}

	f.mu.Lock()
	f.resolves++
	origin := map[string]any{
		"endpoint": f.server.URL, "region": "us-east-1", "bucket": fakeBucket,
		"access_key_id": "key", "secret_access_key": "secret",
	}
	if f.leaseTTL > 0 {
		origin["expires_at"] = time.Now().Add(f.leaseTTL).UTC().Format(time.RFC3339Nano)
	}
	f.mu.Unlock()

	writeJSONResponse(w, http.StatusOK, map[string]any{
		"resolved": map[string]any{
			"reference": "bdn://" + fakeNamespace + "/" + fakeVolume,
			"org_id":    fakeOrg, "origin_digest": digest, "kind": "manifest",
			"target": volume.TargetForDigest(parsed), "sequence": 1, "resolved_from": resolvedFrom,
		},
		"origin": origin,
	})
}

// resolveCount reports how many times a ref has been resolved.
func (f *fakeService) resolveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolves
}

// downloader reads from the fake's object store, standing in for the object
// storage client a caller supplies. It hands back the stored media type
// unchanged, since that is the only thing telling a reader how to decode.
func (f *fakeService) downloader() volume.ObjectDownloader {
	return func(_ context.Context, req volume.ObjectDownload) (*volume.ObjectResult, error) {
		if req.Bucket != fakeBucket {
			return nil, fmt.Errorf("unexpected bucket %q", req.Bucket)
		}
		prefix := fmt.Sprintf("bdn/%s/%s/objects/b3/", fakeOrg, fakeNamespace)
		if !strings.HasPrefix(req.Key, prefix) {
			return nil, fmt.Errorf("unexpected key %q", req.Key)
		}
		hex := req.Key[strings.LastIndexByte(req.Key, '/')+1:]

		f.mu.Lock()
		object, ok := f.objects["b3:"+hex]
		f.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("no such object %q", req.Key)
		}
		return &volume.ObjectResult{
			Body:        io.NopCloser(bytes.NewReader(object.body)),
			ContentType: object.contentType,
			Size:        int64(len(object.body)),
		}, nil
	}
}

// manifestBytes returns the canonical bytes of a stored manifest.
func (f *fakeService) manifestBytes(t *testing.T, digest string) []byte {
	t.Helper()
	f.mu.Lock()
	object, ok := f.objects[digest]
	f.mu.Unlock()
	if !ok {
		t.Fatalf("no manifest %s", digest)
	}
	reader, err := newZstdReader(bytes.NewReader(object.body))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// uploadedBytes totals the bytes of chunk objects the fake received, including
// re-uploads of objects it already had.
func (f *fakeService) uploadedBytes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, upload := range f.uploads {
		if upload.contentType == bdn.ContentTypeChunk {
			total += upload.size
		}
	}
	return total
}

func (f *fakeService) uploadCount(contentType string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, upload := range f.uploads {
		if upload.contentType == contentType {
			count++
		}
	}
	return count
}

func (f *fakeService) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = nil
}

// makeGrantToken builds an unsigned capability token carrying one grant. The
// client reads its own token to decide what to attempt; nothing here verifies
// a signature, and nothing could.
func makeGrantToken(org, namespace, vol, tag string, scopes ...string) string {
	payload, err := json.Marshal(map[string]any{
		"org": org,
		"grants": []map[string]any{{
			"org": org, "namespaces": []string{namespace}, "volumes": []string{vol},
			"tags": []string{tag}, "scopes": scopes,
		}},
	})
	if err != nil {
		panic(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func writeJSONResponse(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeServiceError(w http.ResponseWriter, status int, code, reason, message string) {
	writeJSONResponse(w, status, map[string]any{"error": map[string]any{
		"code": code, "reason": reason, "domain": volume.ErrorDomain, "message": message,
	}})
}
