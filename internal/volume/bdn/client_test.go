package bdn

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
	"github.com/basetenlabs/baseten-go/internal/volume"
)

// newTestClient starts handler as a server and returns a client pointed at it,
// along with a counter of how many tokens were handed out.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *atomic.Int64) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	var tokens atomic.Int64
	client, err := New(Options{
		HTTPClient: server.Client(),
		Tokens: func(_ context.Context, rejected string) (string, string, error) {
			n := tokens.Add(1)
			if rejected != "" {
				return "refreshed", server.URL, nil
			}
			return "token-" + string(rune('a'+n%26)), server.URL, nil
		},
		// Backoff is real time, so tests that exercise retries keep it short.
		Retry: RetryConfig{MaxAttempts: 5, Base: time.Millisecond, Cap: 2 * time.Millisecond},
	})
	require.NoError(t, err)
	return client, &tokens
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.NewEncoder(w).Encode(body))
}

// writeError sends the service's error envelope.
func writeError(w http.ResponseWriter, status int, code, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":{"code":"`+code+`","reason":"`+reason+
		`","domain":"bdn.baseten.co","message":"`+message+`","metadata":{"expected_sequence":"7"},`+
		`"details":[{"type":"RetryInfo","retry_delay_ms":250},{"type":"SomethingNew","x":1}]}}`)
}

func testSession(uploadPath string) *UploadSession {
	return &UploadSession{UploadID: "upload-1", ObjectUploadPath: uploadPath, Namespace: "ns", Volume: "vol"}
}

func TestBeginUpload(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		writeJSON(t, w, http.StatusOK, map[string]any{
			"upload_id":          "01HTYJ5N6R",
			"object_upload_path": "/v1/volumes/ns/vol/uploads/01HTYJ5N6R/objects/{digest}",
			"expires_at":         "2026-03-03T01:10:00Z",
			"org_id":             "org1",
			"namespace":          "ns",
			"volume":             "vol",
		})
	})

	session, err := client.BeginUpload(context.Background(), BeginUploadRequest{
		Namespace: "ns", Volume: "vol", CreateIfMissing: true, ClaimKey: "key",
	})
	require.NoError(t, err)
	require.Equal(t, "/v1/volumes/ns/vol/uploads", gotPath)
	require.Equal(t, `{"create_if_missing":true,"claim_key":"key"}`, strings.TrimSpace(gotBody))
	require.True(t, strings.HasPrefix(gotAuth, "Bearer "), "no bearer token in %q", gotAuth)
	require.Equal(t, "01HTYJ5N6R", session.UploadID)
	require.Equal(t, "org1", session.OrgID)
	require.Equal(t, int64(1772500200), session.ExpiresAt.Unix())
}

// TestBeginUploadRejectsUnusableSession covers a server that answers without
// the digest placeholder. Substituting into that path would silently upload
// every object over the same key.
func TestBeginUploadRejectsUnusableSession(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"upload_id":          "u",
			"object_upload_path": "/v1/volumes/ns/vol/uploads/u/objects/",
			"expires_at":         "2026-03-03T01:10:00Z",
		})
	})

	_, err := client.BeginUpload(context.Background(), BeginUploadRequest{Namespace: "ns", Volume: "vol"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unusable session")
}

func TestUploadObjectSubstitutesDigest(t *testing.T) {
	digest, err := volume.ParseDigest("b3:" + strings.Repeat("ab", 32))
	require.NoError(t, err)

	var gotPath, gotContentType, gotLength string
	var gotBody []byte
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotContentType = r.URL.Path, r.Header.Get("Content-Type")
		gotLength = r.Header.Get("Content-Length")
		gotBody, _ = io.ReadAll(r.Body)
		writeJSON(t, w, http.StatusOK, map[string]any{
			"digest":  digest.String(),
			"target":  volume.TargetForDigest(digest),
			"created": true,
		})
	})

	result, err := client.UploadObject(context.Background(),
		testSession("/v1/volumes/ns/vol/uploads/u/objects/{digest}"), ContentTypeChunk, digest, []byte("chunk"))
	require.NoError(t, err)
	require.Equal(t, "/v1/volumes/ns/vol/uploads/u/objects/"+digest.String(), gotPath)
	require.Equal(t, ContentTypeChunk, gotContentType)
	require.Equal(t, "5", gotLength)
	require.Equal(t, "chunk", string(gotBody))
	require.Equal(t, digest, result.Digest)
	require.Equal(t, volume.TargetForDigest(digest).RelativeKey, result.Target.RelativeKey)
	require.True(t, result.Created, "object should be reported as created")
	require.Equal(t, volume.Success, result.Outcome)
}

// TestUploadObjectEmptyBody covers the empty file, whose chunk is zero bytes.
// The service requires a length header, and a nil body would omit it.
func TestUploadObjectEmptyBody(t *testing.T) {
	digest := volume.Digest{}
	var gotLength string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotLength = r.Header.Get("Content-Length")
		writeJSON(t, w, http.StatusOK, map[string]any{
			"digest": digest.String(), "target": volume.TargetForDigest(digest), "created": false,
		})
	})

	result, err := client.UploadObject(context.Background(),
		testSession("/objects/{digest}"), ContentTypeChunk, digest, nil)
	require.NoError(t, err)
	require.Equal(t, "0", gotLength)
	require.False(t, result.Created, "an existing object should not be reported as created")
}

// TestUploadObjectRejectsDigestMismatch covers the one check that stands
// between a corrupted transfer and a published manifest pointing at bytes that
// are not what it says they are.
func TestUploadObjectRejectsDigestMismatch(t *testing.T) {
	sent, other := volume.Digest{1}, volume.Digest{2}
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"digest": other.String(), "target": volume.TargetForDigest(other), "created": true,
		})
	})

	_, err := client.UploadObject(context.Background(),
		testSession("/objects/{digest}"), ContentTypeChunk, sent, []byte("x"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "server stored it as")
}

func TestCommit(t *testing.T) {
	digest := volume.Digest{0xaa}
	var gotBody, gotKey string
	var calls atomic.Int64
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotKey = r.Header.Get("Idempotency-Key")
		body, _ := io.ReadAll(r.Body)
		gotBody = strings.TrimSpace(string(body))
		writeJSON(t, w, http.StatusOK, map[string]any{
			"manifest_digest": digest.String(), "sequence": 42, "head_updated": true, "tag_applied": true,
		})
	})

	result, err := client.Commit(context.Background(), CommitRequest{
		Namespace: "ns", Volume: "vol", UploadID: "u",
		ManifestDigest: digest, UpdateHead: true, Tags: []string{"prod"}, IdempotencyKey: "key-1",
	})
	require.NoError(t, err)
	require.Equal(t, "key-1", gotKey)
	require.Equal(t, `{"manifest_digest":"`+digest.String()+`","update_head":true,"tags":["prod"]}`, gotBody)
	require.Equal(t, int64(42), result.Sequence)
	require.True(t, result.HeadUpdated, "head should be reported as updated")
	require.True(t, result.TagApplied, "tag should be reported as applied")
}

func TestCommitRejectsReservedTag(t *testing.T) {
	client, _ := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("a commit naming the reserved tag should never reach the server")
	})

	_, err := client.Commit(context.Background(), CommitRequest{
		Namespace: "ns", Volume: "vol", UploadID: "u", Tags: []string{"head"}, IdempotencyKey: "k",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "reserved")
}

// TestCommitReusesIdempotencyKeyAcrossRetries pins the property that makes a
// retried commit safe: the server sees one logical commit, not several.
func TestCommitReusesIdempotencyKeyAcrossRetries(t *testing.T) {
	var keys []string
	var calls atomic.Int64
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if calls.Add(1) < 3 {
			writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "UNAVAILABLE", "try later")
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"sequence": 7, "head_updated": true})
	})

	result, err := client.Commit(context.Background(), CommitRequest{
		Namespace: "ns", Volume: "vol", UploadID: "u", IdempotencyKey: "one-key",
	})
	require.NoError(t, err)
	require.Equal(t, int64(7), result.Sequence)
	require.Len(t, keys, 3)
	for _, key := range keys {
		require.Equal(t, "one-key", key)
	}
}

func TestResolve(t *testing.T) {
	digest := volume.Digest{0xab, 0xcd}
	var gotMethod, gotQuery string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotQuery = r.Method, r.URL.RawQuery
		writeJSON(t, w, http.StatusOK, map[string]any{
			"resolved": map[string]any{
				"reference": "bdn://ns/vol", "org_id": "org1", "origin_digest": digest.String(),
				"kind": "manifest", "target": volume.TargetForDigest(digest),
				"sequence": 42, "resolved_from": "head",
			},
			"origin": map[string]any{
				"endpoint": "", "region": "us-east-1", "bucket": "bdn-origin",
				"access_key_id": "AKIA", "secret_access_key": "secret", "session_token": "session",
				"expires_at": "2026-03-04T17:50:15Z",
			},
		})
	})

	result, err := client.Resolve(context.Background(), "ns/vol:tag with space")
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "ref=ns%2Fvol%3Atag+with+space", gotQuery)
	require.Equal(t, digest, result.Resolved.OriginDigest)
	require.Equal(t, int64(42), result.Resolved.Sequence)
	require.Equal(t, "head", result.Resolved.ResolvedFrom)
	require.Equal(t, "us-east-1", result.Origin.Region)
	require.Equal(t, "session", result.Origin.SessionToken)
	require.Equal(t, int64(1772646615), result.Origin.ExpiresAt.Unix())
}

// TestResolveWithoutCredentialExpiry covers local development, which hands out
// static credentials with no stated expiry. An absent expiry must not read as
// one already past.
func TestResolveWithoutCredentialExpiry(t *testing.T) {
	digest := volume.Digest{0xab}
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"resolved": map[string]any{"origin_digest": digest.String(), "target": volume.TargetForDigest(digest)},
			"origin":   map[string]any{"bucket": "b", "region": "r"},
		})
	})

	result, err := client.Resolve(context.Background(), "ns/vol")
	require.NoError(t, err)
	require.True(t, result.Origin.ExpiresAt.IsZero(), "expiry should be absent, got %v", result.Origin.ExpiresAt)
}

// asyncCloseClient answers every request immediately but keeps reading the
// request body on another goroutine, closing it only after a delay — which
// the RoundTripper contract explicitly permits ("may do so in a separate
// goroutine even after RoundTrip returns"). It records what it read and when
// the body was closed relative to the caller's return.
type asyncCloseClient struct {
	response func() *http.Response
	delay    time.Duration

	mu     sync.Mutex
	closed bool
	read   []byte
}

func (c *asyncCloseClient) Do(req *http.Request) (*http.Response, error) {
	body := req.Body
	go func() {
		time.Sleep(c.delay)
		read, _ := io.ReadAll(body)
		_ = body.Close()
		c.mu.Lock()
		c.closed = true
		c.read = read
		c.mu.Unlock()
	}()
	return c.response(), nil
}

// TestUploadObjectWaitsForTheTransportToReleaseTheBody pins the property the
// push's buffer pooling stands on: when UploadObject returns, the transport
// has closed the request body and can no longer be reading the caller's
// buffer. Without the wait, the caller's return races the transport's
// deferred close, this test's overwrite below plays the role of the pool
// handing the buffer to the next chunk, and the transport reads the next
// chunk's bytes into this chunk's upload.
func TestUploadObjectWaitsForTheTransportToReleaseTheBody(t *testing.T) {
	payload := []byte("the chunk's own bytes")
	digest := volume.Digest{0x42}

	respBody, err := json.Marshal(map[string]any{
		"digest": digest.String(), "target": volume.TargetForDigest(digest), "created": true,
	})
	require.NoError(t, err)
	fake := &asyncCloseClient{
		delay: 30 * time.Millisecond,
		response: func() *http.Response {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(respBody)),
			}
		},
	}
	client, err := New(Options{
		HTTPClient: fake,
		Tokens:     func(context.Context, string) (string, string, error) { return "t", "http://origin", nil },
	})
	require.NoError(t, err)

	buffer := append([]byte(nil), payload...)
	_, err = client.UploadObject(context.Background(),
		testSession("/objects/{digest}"), ContentTypeChunk, digest, buffer)
	require.NoError(t, err)

	// The pool's next user, in miniature. If UploadObject returned while the
	// transport was still draining the body, these bytes are what it reads.
	for i := range buffer {
		buffer[i] = 0xA5
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.True(t, fake.closed, "UploadObject returned before the transport closed the request body")
	if got := string(fake.read); got != string(payload) {
		t.Errorf("the transport read %q — bytes written after UploadObject returned", got)
	}
}
