// Package bdn is the HTTP client for BDN, the data-plane API of the volume
// service — the thing a bdn:// ref names and a bdn_endpoint points at. It
// covers upload sessions, content-addressed object uploads, commits, and ref
// resolution.
//
// The package is named for the protocol rather than for the service
// implementing it, which is what the wire says: refs are bdn://, the token
// exchange returns a bdn_endpoint, and errors carry the bdn domain.
//
// It owns the protocol and nothing else. Object bytes on the read path never
// pass through here — a pull reads them from object storage directly, through
// a seam its caller fills — and neither does the filesystem.
package bdn

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-go/internal/volume"
)

// HTTPClient is the subset of http.Client the protocol needs.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// TokenSource supplies the capability token and the host to send it to.
//
// It is called once per attempt rather than once per client. Tokens are
// short-lived and cannot be renewed, so a push large enough to outlive its
// token has to be able to get another one; a client that read the token at
// construction would fail partway through with nothing to do about it.
//
// rejected names the token the service just refused, and is empty when the
// caller simply wants whatever is current. Passing the token rather than a
// "please refresh" flag is what keeps an expiry cheap: when a token expires
// mid-transfer every request in flight is rejected at once, and a source that
// has already replaced the named token can hand back the new one instead of
// exchanging again for each of them.
type TokenSource func(ctx context.Context, rejected string) (token, host string, err error)

// Options configures a Client.
type Options struct {
	// HTTPClient sends requests. Required.
	HTTPClient HTTPClient

	// Tokens supplies credentials. Required.
	Tokens TokenSource

	// Retry overrides the retry policy. The zero value takes the default.
	Retry RetryConfig
}

// Client speaks the volume service protocol.
type Client struct {
	http   HTTPClient
	tokens TokenSource
	retry  RetryConfig
}

// New builds a Client.
func New(opts Options) (*Client, error) {
	if opts.HTTPClient == nil {
		return nil, fmt.Errorf("bdn: HTTPClient is required")
	}
	if opts.Tokens == nil {
		return nil, fmt.Errorf("bdn: Tokens is required")
	}
	return &Client{http: opts.HTTPClient, tokens: opts.Tokens, retry: opts.Retry.withDefaults()}, nil
}

// Vendor media types. Each names both the kind of object and how its bytes are
// encoded on the wire; the server reads the kind from here rather than from
// the key, which is why every object can live at one path shape.
const (
	ContentTypeChunk    = "application/vnd.baseten.bdn.chunk.v1"
	ContentTypeChunkmap = "application/vnd.baseten.bdn.chunkmap.v1"
	ContentTypeManifest = "application/vnd.baseten.bdn.manifest.v1"

	ContentTypeChunkZstd    = ContentTypeChunk + "+zstd"
	ContentTypeChunkmapZstd = ContentTypeChunkmap + "+zstd"
	ContentTypeManifestZstd = ContentTypeManifest + "+zstd"
)

// BeginUploadRequest opens an upload session.
type BeginUploadRequest struct {
	Namespace string
	Volume    string

	// CreateIfMissing creates the volume when it does not exist. The namespace
	// is never created.
	CreateIfMissing bool

	// ClaimKey elects a single writer: at most one live session per volume and
	// key. A caller that loses gets a CLAIM_HELD error rather than a second
	// session. Empty means no election.
	ClaimKey string
}

// UploadSession is an open session. Every object upload and the commit are
// scoped to it, and it cannot be extended, so the whole push has to finish
// before ExpiresAt.
type UploadSession struct {
	UploadID string
	// ObjectUploadPath is a template containing the literal "{digest}", which
	// the client substitutes per object. It is the server's to choose, so it
	// is used verbatim rather than reconstructed.
	ObjectUploadPath string
	ExpiresAt        time.Time
	OrgID            string
	Namespace        string
	Volume           string
}

type beginUploadBody struct {
	CreateIfMissing bool   `json:"create_if_missing"`
	ClaimKey        string `json:"claim_key,omitempty"`
}

type beginUploadResponse struct {
	UploadID         string `json:"upload_id"`
	ObjectUploadPath string `json:"object_upload_path"`
	ExpiresAt        string `json:"expires_at"`
	OrgID            string `json:"org_id"`
	Namespace        string `json:"namespace"`
	Volume           string `json:"volume"`
}

// BeginUpload opens an upload session for a volume.
func (c *Client) BeginUpload(ctx context.Context, req BeginUploadRequest) (*UploadSession, error) {
	body, err := json.Marshal(beginUploadBody{CreateIfMissing: req.CreateIfMissing, ClaimKey: req.ClaimKey})
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/v1/volumes/%s/%s/uploads", url.PathEscape(req.Namespace), url.PathEscape(req.Volume))

	var out beginUploadResponse
	if _, err := c.call(ctx, request{method: http.MethodPost, path: path, body: body, contentType: "application/json"}, &out); err != nil {
		return nil, fmt.Errorf("begin upload: %w", err)
	}
	if out.UploadID == "" || !strings.Contains(out.ObjectUploadPath, digestPlaceholder) {
		return nil, fmt.Errorf("begin upload: server returned an unusable session")
	}
	session := &UploadSession{
		UploadID:         out.UploadID,
		ObjectUploadPath: out.ObjectUploadPath,
		OrgID:            out.OrgID,
		Namespace:        out.Namespace,
		Volume:           out.Volume,
	}
	if session.ExpiresAt, err = parseTime(out.ExpiresAt); err != nil {
		return nil, fmt.Errorf("begin upload: expires_at: %w", err)
	}
	return session, nil
}

// digestPlaceholder is what the session's upload path carries in place of the
// object's digest.
const digestPlaceholder = "{digest}"

// UploadResult is what one object upload produced.
type UploadResult struct {
	Digest volume.Digest

	// Target is where the object landed, and is what a manifest or chunkmap
	// records. It comes from the server rather than being derived, so the
	// server stays free to change its key layout.
	Target volume.Target

	// Created is false when the object was already stored. Re-uploading a
	// content-addressed object is success, not a conflict.
	Created bool

	// Outcome is what the request observed about the origin's capacity, for
	// the limiter. A request that succeeded only after backing off still
	// reports a stall: the pushback happened whether or not it was survived.
	Outcome volume.Outcome
}

type uploadResponse struct {
	Digest  string        `json:"digest"`
	Target  volume.Target `json:"target"`
	Created bool          `json:"created"`
}

// UploadObject stores one content-addressed object. The digest is the caller's
// own hash of body; the server recomputes it and the echoed value is compared,
// so a mismatch anywhere in between is caught rather than published.
func (c *Client) UploadObject(
	ctx context.Context,
	session *UploadSession,
	contentType string,
	digest volume.Digest,
	body []byte,
) (*UploadResult, error) {
	path := strings.Replace(session.ObjectUploadPath, digestPlaceholder, digest.String(), 1)

	var out uploadResponse
	outcome, err := c.call(ctx, request{
		method:      http.MethodPut,
		path:        path,
		body:        body,
		contentType: contentType,
	}, &out)
	if err != nil {
		return nil, fmt.Errorf("upload object %s: %w", digest, err)
	}
	echoed, err := volume.ParseDigest(out.Digest)
	if err != nil {
		return nil, fmt.Errorf("upload object %s: server echoed %w", digest, err)
	}
	if echoed != digest {
		return nil, fmt.Errorf("upload object %s: server stored it as %s", digest, echoed)
	}
	// The echoed target lands in the manifest this push will commit, so it
	// is held to the same rule every decoded target is.
	if err := volume.ValidateObjectTarget(out.Target); err != nil {
		return nil, fmt.Errorf("upload object %s: server returned %w", digest, err)
	}
	return &UploadResult{Digest: digest, Target: out.Target, Created: out.Created, Outcome: outcome}, nil
}

// CommitRequest publishes a manifest as a new version of the volume. It is the
// only point at which anything a push uploaded becomes visible.
type CommitRequest struct {
	Namespace string
	Volume    string
	UploadID  string

	ManifestDigest volume.Digest

	// UpdateHead moves the volume's head to this version, which is what makes
	// a ref without a tag resolve to it.
	UpdateHead bool

	// Tags are applied atomically with the commit. The reserved name "head" is
	// rejected here: moving head is UpdateHead's job.
	Tags []string

	// IdempotencyKey collapses retries of one logical commit into one visible
	// commit. It must be the same string across those retries, which is why
	// the caller mints it rather than this method.
	IdempotencyKey string
}

// CommitResult is what a commit published.
type CommitResult struct {
	ManifestDigest string
	Sequence       int64
	HeadUpdated    bool
	TagApplied     bool
}

type commitBody struct {
	ManifestDigest string   `json:"manifest_digest"`
	UpdateHead     bool     `json:"update_head"`
	Tags           []string `json:"tags,omitempty"`
}

type commitResponse struct {
	ManifestDigest string `json:"manifest_digest"`
	// Sequence is absent on versions committed before sequences were stamped,
	// and reads as zero.
	Sequence    int64 `json:"sequence"`
	HeadUpdated bool  `json:"head_updated"`
	TagApplied  bool  `json:"tag_applied"`
}

// Commit publishes the manifest.
func (c *Client) Commit(ctx context.Context, req CommitRequest) (*CommitResult, error) {
	for _, tag := range req.Tags {
		if tag == HeadTag {
			return nil, fmt.Errorf("commit: %q is reserved and moves with UpdateHead", HeadTag)
		}
	}
	if req.IdempotencyKey == "" {
		return nil, fmt.Errorf("commit: IdempotencyKey is required")
	}
	body, err := json.Marshal(commitBody{
		ManifestDigest: req.ManifestDigest.String(),
		UpdateHead:     req.UpdateHead,
		Tags:           req.Tags,
	})
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/v1/volumes/%s/%s/uploads/%s/commit",
		url.PathEscape(req.Namespace), url.PathEscape(req.Volume), url.PathEscape(req.UploadID))

	var out commitResponse
	if _, err := c.call(ctx, request{
		method:      http.MethodPost,
		path:        path,
		body:        body,
		contentType: "application/json",
		headers:     map[string]string{"Idempotency-Key": req.IdempotencyKey},
	}, &out); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &CommitResult{
		ManifestDigest: out.ManifestDigest,
		Sequence:       out.Sequence,
		HeadUpdated:    out.HeadUpdated,
		TagApplied:     out.TagApplied,
	}, nil
}

// HeadTag is the reserved tag naming a volume's current version.
const HeadTag = "head"

// Resolved is what a ref resolved to. The digest pins it: a tag can move
// afterwards, but this version cannot.
type Resolved struct {
	Reference    string
	OrgID        string
	OriginDigest volume.Digest
	Kind         string
	Target       volume.Target
	Sequence     int64
	ResolvedFrom string
}

// Origin is a short-lived read-only credential lease for the object store,
// scoped to the namespace's objects. Reads go there directly rather than
// through the service.
type Origin struct {
	// Endpoint is empty for AWS itself, and a base URL for anything else. The
	// two address their buckets differently, so the distinction matters to
	// whoever performs the read.
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string

	// ExpiresAt is when the lease stops working. A transfer that outlives it
	// resolves the pinned digest again, which mints a new lease and changes
	// nothing else.
	ExpiresAt time.Time
}

// ResolveResult pairs the resolved version with credentials to read it.
type ResolveResult struct {
	Resolved Resolved
	Origin   Origin
}

type resolveResponse struct {
	Resolved struct {
		Reference    string        `json:"reference"`
		OrgID        string        `json:"org_id"`
		OriginDigest string        `json:"origin_digest"`
		Kind         string        `json:"kind"`
		Target       volume.Target `json:"target"`
		Sequence     int64         `json:"sequence"`
		ResolvedFrom string        `json:"resolved_from"`
	} `json:"resolved"`
	Origin struct {
		Endpoint        string `json:"endpoint"`
		Region          string `json:"region"`
		Bucket          string `json:"bucket"`
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SessionToken    string `json:"session_token"`
		ExpiresAt       string `json:"expires_at"`
	} `json:"origin"`
}

// Resolve turns a ref into a pinned version and credentials to read it.
func (c *Client) Resolve(ctx context.Context, ref string) (*ResolveResult, error) {
	var out resolveResponse
	path := "/v1/volumes/resolve?ref=" + url.QueryEscape(ref)
	if _, err := c.call(ctx, request{method: http.MethodPost, path: path}, &out); err != nil {
		return nil, fmt.Errorf("resolve %s: %w", ref, err)
	}

	digest, err := volume.ParseDigest(out.Resolved.OriginDigest)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: origin_digest: %w", ref, err)
	}
	if err := volume.ValidateObjectTarget(out.Resolved.Target); err != nil {
		return nil, fmt.Errorf("resolve %s: %w", ref, err)
	}
	result := &ResolveResult{
		Resolved: Resolved{
			Reference:    out.Resolved.Reference,
			OrgID:        out.Resolved.OrgID,
			OriginDigest: digest,
			Kind:         out.Resolved.Kind,
			Target:       out.Resolved.Target,
			Sequence:     out.Resolved.Sequence,
			ResolvedFrom: out.Resolved.ResolvedFrom,
		},
		Origin: Origin{
			Endpoint:        out.Origin.Endpoint,
			Region:          out.Origin.Region,
			Bucket:          out.Origin.Bucket,
			AccessKeyID:     out.Origin.AccessKeyID,
			SecretAccessKey: out.Origin.SecretAccessKey,
			SessionToken:    out.Origin.SessionToken,
		},
	}
	// A lease with no stated expiry is what local development returns; treat
	// it as never expiring rather than as already expired.
	if out.Origin.ExpiresAt != "" {
		if result.Origin.ExpiresAt, err = parseTime(out.Origin.ExpiresAt); err != nil {
			return nil, fmt.Errorf("resolve %s: origin expires_at: %w", ref, err)
		}
	}
	return result, nil
}

// Grants reads the capabilities the current token claims. See [Grants] for
// what that is and is not good for. A token that cannot be fetched yields
// permissive grants, since the point is to skip requests that would certainly
// be denied and an unknown token makes nothing certain.
func (c *Client) Grants(ctx context.Context) Grants {
	token, _, err := c.tokens(ctx, "")
	if err != nil {
		return Grants{permissive: true}
	}
	return DecodeGrants(token)
}

// NewIdempotencyKey mints a key for one logical commit. The server only checks
// that a key is present and consistent across retries, so any unique opaque
// string does; this one is random.
func NewIdempotencyKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// request is one logical call, which the retry loop may send several times.
type request struct {
	method      string
	path        string
	body        []byte
	contentType string
	headers     map[string]string
}

// call sends a request, decodes a successful JSON response into out, and
// reports what the attempts observed about the origin's capacity.
func (c *Client) call(ctx context.Context, req request, out any) (volume.Outcome, error) {
	resp, outcome, err := c.send(ctx, req)
	if err != nil {
		return outcome, err
	}
	if out != nil {
		if err := json.Unmarshal(resp.body, out); err != nil {
			return outcome, fmt.Errorf("decode response: %w", err)
		}
	}
	return outcome, nil
}

// rawResponse is a successful response, read into memory. Every response this
// package handles is small JSON; object bytes take a different path entirely.
type rawResponse struct {
	status int
	header http.Header
	body   []byte
}

// send performs the request, retrying transient failures, and returns the
// first non-retryable outcome.
//
// The token is fetched per attempt, and a rejected one is re-exchanged once:
// the point of a short-lived credential is that it expires, and a transfer
// long enough to see that happen should survive it.
func (c *Client) send(ctx context.Context, req request) (*rawResponse, volume.Outcome, error) {
	// A request that only ever saw a peer close the connection before
	// answering carries no evidence about capacity, even though it succeeded
	// on a later attempt: connection reuse churn is not the origin asking for
	// less. A stall anywhere outranks that.
	outcome, sawClose := volume.Success, false
	// One re-exchange per call: a second rejection means the credential is
	// wrong rather than stale, and asking again would only loop.
	refreshUsed := false
	rejected := ""

	// spent counts attempts the retry budget has actually paid for. Getting a
	// fresh credential is not one of them, so it is incremented where an
	// attempt is consumed rather than once per turn of the loop.
	spent := 0
	for {
		token, host, err := c.tokens(ctx, rejected)
		if err != nil {
			return nil, outcome, fmt.Errorf("get token: %w", err)
		}
		rejected = ""

		resp, err := c.attempt(ctx, host, token, req)
		if err != nil {
			// A cancelled context is the caller's decision, not a failure to
			// retry around.
			if ctx.Err() != nil {
				return nil, outcome, ctx.Err()
			}
			if classifyTransport(err) == volume.Stall {
				outcome = volume.Stall
			} else {
				sawClose = true
			}
			spent++
			if spent >= c.retry.MaxAttempts {
				return nil, outcome, err
			}
			if err := sleep(ctx, c.retry.backoff(spent)); err != nil {
				return nil, outcome, err
			}
			continue
		}

		switch {
		case resp.status >= 200 && resp.status < 300:
			if outcome == volume.Success && sawClose {
				outcome = volume.Neutral
			}
			return resp, outcome, nil

		case resp.status == http.StatusUnauthorized && !refreshUsed:
			// The token this attempt used is no longer accepted. Naming it
			// lets the source tell "mine is stale" from "someone already
			// replaced it", so a whole transfer's worth of simultaneous
			// rejections costs one exchange rather than one each. Getting a
			// fresh credential is a different request, not a retry of the
			// same one, so it does not spend an attempt.
			refreshUsed, rejected = true, token

		case retryableStatus(resp.status):
			outcome = volume.Stall
			spent++
			if spent >= c.retry.MaxAttempts {
				return nil, outcome, decodeError(resp)
			}
			if err := sleep(ctx, c.retry.waitFor(spent, resp.header)); err != nil {
				return nil, outcome, err
			}

		default:
			return nil, outcome, decodeError(resp)
		}
	}
}

// attempt sends the request once and reads the whole response.
func (c *Client) attempt(ctx context.Context, host, token string, req request) (*rawResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, req.method, strings.TrimRight(host, "/")+req.path, bytes.NewReader(req.body))
	if err != nil {
		return nil, err
	}
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	if req.contentType != "" {
		httpReq.Header.Set("Content-Type", req.contentType)
	}
	for name, value := range req.headers {
		httpReq.Header.Set(name, value)
	}
	// The service requires a length on object uploads, and a bytes.Reader body
	// gives net/http one for free — but only if the body is non-nil, which it
	// is even for an empty chunk.
	httpReq.ContentLength = int64(len(req.body))

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &rawResponse{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// parseTime reads a timestamp the service wrote.
func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}
