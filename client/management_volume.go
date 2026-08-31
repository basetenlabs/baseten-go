package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
	"github.com/basetenlabs/baseten-go/internal/volume/transfer"
)

// A volume is a versioned directory tree stored by content, so pushing a tree
// that mostly matches one already stored transfers only what differs, and
// downloading one twice transfers nothing the second time.
//
// Two pieces are not built in, because building them in would put a hashing
// library and a cloud SDK into every program that imports this package. Both
// are supplied as functions on the options:
//
//   - NewHasher must return an unkeyed BLAKE3 hash with a 32-byte digest. That
//     is the whole content-addressing scheme, so getting it wrong produces a
//     volume no other client can read. Both common libraries need to be told
//     the size explicitly or by default:
//     github.com/zeebo/blake3 with func() hash.Hash { return blake3.New() },
//     or lukechampine.com/blake3 with
//     func() hash.Hash { return blake3.New(32, nil) }.
//     A 64-byte extended output is the mistake to watch for; it is checked
//     against the published test vectors before a transfer starts.
//
//   - DownloadObject reads an object from S3, and NewDecompressor unwraps a
//     zstd stream. aws-sdk-go-v2's GetObject and
//     github.com/klauspost/compress/zstd fill both.
//
// These are aliases of internal types so that a program can name them without
// importing anything else.
type (
	// VolumeObjectDownload names one object to read from the store.
	VolumeObjectDownload = volume.ObjectDownload

	// VolumeObjectResult is an open object. ContentType is not decoration: the
	// service decides per object whether to compress it, and the media type is
	// the only record of what it chose.
	VolumeObjectResult = volume.ObjectResult

	// VolumeObjectCredentials are the short-lived read-only credentials the
	// service leases for reading a namespace's objects.
	VolumeObjectCredentials = volume.Credentials

	// VolumeObjectDownloader reads one object from the store. Named, like the
	// ModelUploader it sits beside, so the field declaring it reads as a role
	// rather than as a signature.
	VolumeObjectDownloader = volume.ObjectDownloader

	// VolumeDecompressor unwraps a zstd stream.
	VolumeDecompressor = volume.Decompressor

	// VolumeProgress reports how far a transfer has got.
	VolumeProgress = volume.Progress

	// VolumePhase names what a transfer is doing. Totals reset when it
	// changes.
	VolumePhase = volume.Phase

	// VolumeConcurrency tunes how much of a transfer runs at once. Every zero
	// field takes a default.
	VolumeConcurrency = volume.Concurrency

	// VolumeError is a structured error from the volume service. Branch on its
	// Reason, which is stable, rather than on its Message, which is not.
	VolumeError = volume.Error
)

// Phases a transfer passes through.
const (
	VolumePhaseScan     = volume.PhaseScan
	VolumePhaseUpload   = volume.PhaseUpload
	VolumePhaseCommit   = volume.PhaseCommit
	VolumePhaseResolve  = volume.PhaseResolve
	VolumePhaseDownload = volume.PhaseDownload
	VolumePhasePublish  = volume.PhasePublish
)

// Reasons a [VolumeError] can carry that a caller has a reason to act on.
const (
	// VolumeReasonUploadSessionExpired means the push outlived the session it
	// was uploading into. Sessions cannot be extended; the push has to run
	// again, and will reuse whatever the previous version already had.
	VolumeReasonUploadSessionExpired = volume.ReasonUploadSessionExpired

	// VolumeReasonCASConflict means something else published to the volume
	// while this push was running.
	VolumeReasonCASConflict = volume.ReasonCASConflict

	VolumeReasonNotFound           = volume.ReasonNotFound
	VolumeReasonPermissionDenied   = volume.ReasonPermissionDenied
	VolumeReasonAmbiguousPrefix    = volume.ReasonAmbiguousPrefix
	VolumeReasonRateLimited        = volume.ReasonRateLimited
	VolumeReasonChunkTooLarge      = volume.ReasonChunkTooLarge
	VolumeReasonManifestTooLarge   = volume.ReasonManifestTooLarge
	VolumeReasonServiceUnavailable = volume.ReasonUnavailable
)

// HasVolumeReason reports whether err is a [VolumeError] carrying the given
// reason, which is the supported way to branch on a specific failure. The
// alternative is errors.As plus a field comparison; this is the same thing
// spelled once.
func HasVolumeReason(err error, reason string) bool {
	return volume.HasReason(err, reason)
}

// PushVolumeOptions configures [ManagementClient.PushVolume].
type PushVolumeOptions struct {
	// Namespace and Volume name where to publish. The volume is created if it
	// does not exist; the namespace must already.
	Namespace string
	Volume    string

	// SourceDir is the directory to push. It is walked without following
	// symlinks, which are recorded as links.
	SourceDir string

	// Tags are applied atomically with the publish, so a tag never points at a
	// version that is only partly uploaded. The reserved name "head" is not a
	// tag; see RequireHeadMove.
	Tags []string

	// RequireHeadMove fails the push before it uploads anything if the
	// credential does not carry permission to move the volume's head. Without
	// it, such a push still publishes the version, and refs without a tag keep
	// resolving to the previous one — reported as HeadMoveDenied.
	//
	// See HeadMoveDenied: with a credential from this client's own exchange,
	// the condition this guards against is not expected to arise.
	RequireHeadMove bool

	// NewHasher returns an unkeyed BLAKE3 hash with a 32-byte digest.
	// Required; see the package notes above for the exact constructors.
	NewHasher func() hash.Hash

	// NewDecompressor and DownloadObject let the push read the volume's
	// previous version, so a file whose bytes have not changed is not uploaded
	// again. Both optional, and only together: without them the push uploads
	// everything, which is slower and produces the same version.
	NewDecompressor VolumeDecompressor
	DownloadObject  VolumeObjectDownloader

	// Progress is called as the push proceeds, never concurrently with itself.
	// It should return promptly.
	Progress func(VolumeProgress)

	// HTTPClient overrides the client used for the volume service. Nil uses
	// the one this ManagementClient was built with.
	HTTPClient interface {
		Do(*http.Request) (*http.Response, error)
	}

	// Concurrency overrides the transfer's concurrency limits.
	Concurrency *VolumeConcurrency

	// SourceURI records where the tree came from, and is inside the bytes the
	// version's digest covers: two pushes of identical trees from different
	// paths are different versions. Empty derives it from SourceDir, which
	// means a push from a different absolute path publishes a new version of
	// an unchanged tree. Set it to a stable string to avoid that.
	SourceURI string
}

// Validate reports whether the options describe a push that can be attempted.
// [ManagementClient.PushVolume] calls it before its first request.
func (o PushVolumeOptions) Validate() error {
	if o.NewDecompressor == nil && o.DownloadObject != nil {
		return errors.New("NewDecompressor is required alongside DownloadObject")
	}
	if o.NewDecompressor != nil && o.DownloadObject == nil {
		return errors.New("DownloadObject is required alongside NewDecompressor")
	}
	return o.pushOptions().Validate()
}

// PushVolumeResult is what a push published.
type PushVolumeResult struct {
	// ManifestDigest names the published version, and is what to pin a
	// download to in order to get this exact tree back.
	ManifestDigest string

	// Sequence is the volume's snapshot sequence at this publish.
	Sequence int64

	// HeadUpdated reports whether this push moved head. It is false when head
	// already pointed at this exact version, which is what re-pushing an
	// unchanged tree does — so false does not mean untagged refs resolve
	// elsewhere, only that nothing had to move.
	HeadUpdated bool

	// HeadMoveDenied reports that the push did not ask to move head, because
	// the credential's grants do not cover it. The version was published and
	// can be reached by digest or tag; what did not happen is head moving to
	// it.
	//
	// Not expected to occur with a credential obtained the way this client
	// obtains one. The exchange grants push and tag together or refuses
	// outright, and the credential it mints covers every volume and tag in the
	// namespaces it names — so a credential that can push can also move head.
	// The field describes a real state of the underlying protocol, which
	// admits credentials minted other ways, and reports what this client
	// actually asked for rather than asserting what the service would allow.
	HeadMoveDenied bool

	TagsApplied []string

	// Files and Bytes are what the source tree held.
	Files int64
	Bytes int64

	// Chunks counts every object the push accounted for, and Unique, Reused,
	// and Existing partition it. Reused never reached the network, because a
	// previous version or an earlier file in the same push already had those
	// bytes; Existing was sent and the service already had it.
	Chunks   int64
	Unique   int64
	Reused   int64
	Existing int64
}

// PushVolume uploads a directory and publishes it as a new version of a
// volume.
//
// Nothing is visible until the whole tree has been uploaded, so an interrupted
// push publishes nothing. What it uploaded is not wasted: the next push finds
// those objects already stored and skips them.
func (c *ManagementClient) PushVolume(ctx context.Context, opts PushVolumeOptions) (*PushVolumeResult, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	// Folded before the token is scoped to them, so the capability the
	// exchange returns names the volume the transfer will actually address.
	opts.Namespace = strings.ToLower(opts.Namespace)
	opts.Volume = strings.ToLower(opts.Volume)

	client, _, err := c.volumeClient(opts.Namespace, pushScopes(opts), newCorrelationID(), opts.HTTPClient)
	if err != nil {
		return nil, err
	}

	push := opts.pushOptions()
	result, err := transfer.Push(ctx, client, push)
	if err != nil {
		return nil, err
	}
	return &PushVolumeResult{
		ManifestDigest: result.ManifestDigest.String(),
		Sequence:       result.Sequence,
		HeadUpdated:    result.HeadUpdated,
		HeadMoveDenied: result.HeadMoveDenied,
		TagsApplied:    result.TagsApplied,
		Files:          result.Files,
		Bytes:          result.Bytes,
		Chunks:         result.Chunks,
		Unique:         result.Unique,
		Reused:         result.Reused,
		Existing:       result.Existing,
	}, nil
}

// pushOptions translates the public options into the engine's.
func (o PushVolumeOptions) pushOptions() transfer.PushOptions {
	opts := transfer.PushOptions{
		Namespace:       o.Namespace,
		Volume:          o.Volume,
		SourceDir:       o.SourceDir,
		SourceURI:       o.SourceURI,
		Tags:            o.Tags,
		RequireHeadMove: o.RequireHeadMove,
		NewHasher:       o.NewHasher,
		Progress:        o.Progress,
	}
	if o.NewDecompressor != nil {
		opts.Decompress = o.NewDecompressor
	}
	if o.DownloadObject != nil {
		opts.DownloadObject = o.DownloadObject
	}
	if o.Concurrency != nil {
		opts.Concurrency = *o.Concurrency
	}
	return opts
}

// DownloadVolumeOptions configures [ManagementClient.DownloadVolume].
type DownloadVolumeOptions struct {
	// Ref names what to download: "namespace/volume" for the current version,
	// "namespace/volume:tag", or "namespace/volume@b3:..." for one exact
	// version. Whatever it names is resolved once and pinned, so a tag that
	// moves partway through cannot produce a tree assembled from two versions.
	Ref string

	// DestDir is where the tree ends up. Unless Overwrite is set it must not
	// exist or must be empty, and the tree is assembled beside it and moved
	// into place only once it is complete.
	DestDir string

	// Overwrite writes into an existing DestDir in place. Files already there
	// that the volume does not describe are left alone; a failed download
	// leaves a partly written directory rather than an untouched one.
	Overwrite bool

	// Include narrows the download to named entries. Each is an exact path or
	// a directory whose contents are wanted, matched on slash boundaries. One
	// that matches nothing fails the download rather than silently producing
	// less than was asked for. Empty downloads the whole volume.
	Include []string

	// Restart discards a partly downloaded tree from an earlier attempt
	// instead of continuing it.
	Restart bool

	// NewHasher returns an unkeyed BLAKE3 hash with a 32-byte digest.
	// Required: every chunk is verified against the digest recorded for it,
	// and continuing an interrupted download works by hashing what is already
	// on disk. See the package notes above for the exact constructors.
	NewHasher func() hash.Hash

	// NewDecompressor unwraps a zstd stream. Required: the service compresses
	// what it stores at its own discretion, so a reader has to be able to
	// decompress whatever comes back.
	NewDecompressor VolumeDecompressor

	// DownloadObject reads one object from S3. Required, and it owns its own
	// retries: an error returned from it ends the download.
	DownloadObject VolumeObjectDownloader

	// Progress is called as the download proceeds, never concurrently with
	// itself.
	Progress func(VolumeProgress)

	// HTTPClient overrides the client used for the volume service. Nil uses
	// the one this ManagementClient was built with.
	HTTPClient interface {
		Do(*http.Request) (*http.Response, error)
	}

	// Concurrency overrides the transfer's concurrency limits.
	Concurrency *VolumeConcurrency
}

// Validate reports whether the options describe a download that can be
// attempted. [ManagementClient.DownloadVolume] calls it before its first
// request.
func (o DownloadVolumeOptions) Validate() error {
	return o.pullOptions().Validate()
}

// DownloadVolumeResult is what a download produced.
type DownloadVolumeResult struct {
	// VersionRef is the ref pinned to the version that was downloaded, which
	// is the form to quote to get this exact tree again.
	VersionRef string

	// ManifestDigest names that version.
	ManifestDigest string

	// Files and Bytes are what was written.
	Files int64
	Bytes int64

	// SelectedFiles and TotalFiles report what Include narrowed to. They are
	// equal when the whole volume was downloaded.
	SelectedFiles int64
	TotalFiles    int64

	// ChunksFetched and ChunksReused partition the chunks. Reused were already
	// on disk with the right contents, from an earlier attempt.
	ChunksFetched int64
	ChunksReused  int64
}

// DownloadVolume downloads a version of a volume into a directory.
//
// Every chunk is checked against its recorded digest before it is written, so
// a corrupted or truncated read fails the download rather than producing a
// file that merely looks complete. An interrupted download can be run again
// and picks up where it stopped.
func (c *ManagementClient) DownloadVolume(ctx context.Context, opts DownloadVolumeOptions) (*DownloadVolumeResult, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	ref, err := bdn.ParseRef(opts.Ref)
	if err != nil {
		return nil, err
	}
	client, _, err := c.volumeClient(ref.Namespace,
		[]string{volumeScopePull}, newCorrelationID(), opts.HTTPClient)
	if err != nil {
		return nil, err
	}

	result, err := transfer.Pull(ctx, client, opts.pullOptions())
	if err != nil {
		return nil, err
	}
	return &DownloadVolumeResult{
		VersionRef:     result.VersionRef,
		ManifestDigest: result.ManifestDigest.String(),
		Files:          result.Files,
		Bytes:          result.Bytes,
		SelectedFiles:  result.SelectedFiles,
		TotalFiles:     result.TotalFiles,
		ChunksFetched:  result.ChunksFetched,
		ChunksReused:   result.ChunksReused,
	}, nil
}

// pullOptions translates the public options into the engine's.
func (o DownloadVolumeOptions) pullOptions() transfer.PullOptions {
	opts := transfer.PullOptions{
		Ref:       o.Ref,
		DestDir:   o.DestDir,
		Overwrite: o.Overwrite,
		Include:   o.Include,
		Restart:   o.Restart,
		NewHasher: o.NewHasher,
		Progress:  o.Progress,
	}
	if o.NewDecompressor != nil {
		opts.Decompress = o.NewDecompressor
	}
	if o.DownloadObject != nil {
		opts.DownloadObject = o.DownloadObject
	}
	if o.Concurrency != nil {
		opts.Concurrency = *o.Concurrency
	}
	return opts
}

// volumeClient builds a protocol client whose credentials come from exchanging
// the API key for a capability token over a namespace.
//
// The token covers a namespace and a set of scopes, not a single volume, so
// the scopes asked for are what distinguishes one transfer's credentials from
// another's.
func (c *ManagementClient) volumeClient(
	namespace string,
	scopes []string,
	correlationID string,
	httpClient interface {
		Do(*http.Request) (*http.Response, error)
	},
) (*bdn.Client, *volumeTokenSource, error) {
	if httpClient == nil {
		httpClient = c.api.HTTPClient
	}
	tokens := c.volumeTokenSource(namespace, scopes, correlationID)
	client, err := bdn.New(bdn.Options{
		HTTPClient: httpClient,
		Tokens:     tokens.tokenSource(),
	})
	if err != nil {
		return nil, nil, err
	}
	return client, tokens, nil
}

// pushScopes is what a push asks for, and asks for nothing beyond it.
//
// The exchange refuses rather than narrows: a scope the caller has no
// permission for fails the whole exchange with no token at all. So an
// unnecessary scope does not cost a smaller grant, it costs the transfer.
//
// PULL earns its place only when the caller supplied both seams that read the
// previous version, and that condition has to stay exactly that: request PUSH
// without PULL and the prior-version lookup is refused, which the push treats
// as "no previous version" and carries on uploading everything — no error,
// nothing in the result to notice.
//
// TAG is asked for only when tags are actually being applied, since applying
// one at commit is gated like setting a tag directly. Moving head needs no
// scope of its own beyond push.
func pushScopes(opts PushVolumeOptions) []string {
	scopes := []string{volumeScopePush}
	if len(opts.Tags) > 0 {
		scopes = append(scopes, volumeScopeTag)
	}
	if opts.DownloadObject != nil && opts.NewDecompressor != nil {
		scopes = append(scopes, volumeScopePull)
	}
	return scopes
}

// Scopes a capability token can carry. A token is granted a set of these for a
// set of namespaces, and the service returns the set it actually granted,
// which may be smaller than the set asked for.
const (
	volumeScopePull = "PULL"
	volumeScopePush = "PUSH"
	volumeScopeTag  = "TAG"
)

// ErrNoVolumeAPI reports that the environment has no volume service to talk
// to. It is a deployment fact rather than a failure, and distinct from a
// transport error, so a caller can say so plainly instead of surfacing
// something that reads like an outage.
var ErrNoVolumeAPI = errors.New("this environment does not expose a volume API")

// tokenRefreshMargin is how long before a token expires the client exchanges a
// new one. Tokens cannot be renewed, so this is the difference between a
// transfer that carries on and one that has every request in flight rejected
// at once and has to recover.
const tokenRefreshMargin = 2 * time.Minute

// volumeToken is one exchanged capability token and what the service granted
// with it.
type volumeToken struct {
	token string

	// endpoint is the base URL for the volume service. The service returns it
	// as null in an environment that does not expose one.
	endpoint string

	// expiresAt is when the token stops working. Non-renewable, so the only
	// response to it approaching is to exchange another.
	expiresAt time.Time

	// scopes and namespaces are what the service reported alongside the token,
	// kept for diagnostics. Nothing is decided from scopes; see
	// volumeTokenResponse.Scopes.
	scopes     []string
	namespaces []string
}

// volumeTokenSource exchanges the API key for a capability token and holds on
// to it until it nears expiry or something rejects it.
//
// Tokens are short-lived and cannot be renewed, so a transfer long enough to
// outlive one has to exchange again rather than fail. There are two ways that
// happens. The expiry the service reports lets the token be replaced before
// anything breaks, which is the quiet path. The service rejecting a token is
// the fallback, for a clock that disagrees or a credential revoked early.
//
// The fallback has to handle an expiry rejecting every request in flight at
// once, so the caller names the token it was refused rather than merely asking
// for a refresh. A rejection naming a token that has already been replaced is
// answered with the replacement, which turns a transfer's worth of
// simultaneous rejections into one exchange instead of one per request. That
// collapse is not only an efficiency: the exchange endpoint is rate limited
// per API key, and a transfer's worth of simultaneous exchanges would spend
// that budget on a single expiry.
func (c *ManagementClient) volumeTokenSource(namespace string, scopes []string, correlationID string) *volumeTokenSource {
	return &volumeTokenSource{
		client:        c,
		namespace:     namespace,
		scopes:        scopes,
		correlationID: correlationID,
		proactive:     true,
	}
}

type volumeTokenSource struct {
	client        *ManagementClient
	namespace     string
	scopes        []string
	correlationID string

	mu      sync.Mutex
	current *volumeToken

	// proactive goes false once a replacement arrives already inside the
	// refresh margin. A deployment issuing tokens shorter than the margin — or
	// a clock far enough ahead — would otherwise make every attempt of every
	// request exchange a token that is instantly stale again, serialized
	// behind this mutex, until the endpoint's per-key rate limit ends the
	// transfer and starves everything else using that key.
	//
	// Giving up on the quiet path leaves the rejection path, which exchanges
	// once per refusal rather than once per attempt. That is the right
	// degraded mode: slower and noisier, but bounded.
	proactive bool
}

// get returns a usable token, exchanging one when there is none, when the one
// held is near expiry, or when the caller reports that the one it used was
// refused.
func (s *volumeTokenSource) get(ctx context.Context, rejected string) (*volumeToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Whether this exchange is a replacement for a token still in hand, as
	// opposed to acquiring the first one. Only a replacement can show that
	// refreshing is not helping: a first token that happens to arrive near
	// expiry says nothing about what a fresh one would look like.
	replacing := s.current != nil && s.proactive && s.current.nearExpiry()

	if s.current != nil && s.current.token != rejected && !replacing {
		return s.current, nil
	}
	exchanged, err := s.client.exchangeVolumeToken(ctx, s.namespace, s.scopes, s.correlationID)
	if err != nil {
		return nil, err
	}
	s.current = exchanged

	// A replacement already inside the margin bought nothing, and asking again
	// on the next attempt would mean an exchange per attempt for the rest of
	// the transfer.
	if replacing && exchanged.nearExpiry() {
		s.proactive = false
	}
	return s.current, nil
}

// nearExpiry reports whether the token is close enough to expiring that a
// transfer should not start another request with it. A token with no stated
// expiry never is.
func (t *volumeToken) nearExpiry() bool {
	return !t.expiresAt.IsZero() && time.Until(t.expiresAt) < tokenRefreshMargin
}

// tokenSource adapts the source to what the protocol client asks for.
func (s *volumeTokenSource) tokenSource() bdn.TokenSource {
	return func(ctx context.Context, rejected string) (string, string, error) {
		token, err := s.get(ctx, rejected)
		if err != nil {
			return "", "", err
		}
		return token.token, token.endpoint, nil
	}
}

// granted returns the token currently held, for a caller that needs to know
// what the service actually authorized. Nil before the first exchange.
func (s *volumeTokenSource) granted() *volumeToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

type volumeTokenRequest struct {
	Scopes        []string `json:"scopes"`
	Namespaces    []string `json:"namespaces"`
	CorrelationID string   `json:"correlation_id,omitempty"`
}

type volumeTokenResponse struct {
	Token string `json:"token"`
	// ExpiresAt is ISO 8601.
	ExpiresAt string `json:"expires_at"`
	// Scopes is described as the capabilities granted, but is the request
	// echoed back — the exchange refuses a scope it will not grant rather than
	// returning a smaller set. Nothing is decided from it: a field that cannot
	// differ from what was sent carries no information, and if it ever began
	// to differ, believing it would mean claiming a capability the minted
	// token does not hold.
	Scopes []string `json:"scopes"`
	// Namespaces come back canonicalized to lowercase.
	Namespaces []string `json:"namespaces"`
	// Endpoint is null in an environment with no public volume API.
	Endpoint *string `json:"bdn_endpoint"`
}

// exchangeVolumeToken trades the API key for a capability token over a
// namespace.
//
// The request is built here rather than through the generated API client
// because this endpoint is not in the generated surface yet. It borrows that
// client's base URL, transport, and headers so authentication and user agent
// behave the same as every other call.
//
// TODO: switch to the generated client once the endpoint lands in the
// management API's OpenAPI spec, and re-verify the request and response
// shapes against what shipped.
func (c *ManagementClient) exchangeVolumeToken(
	ctx context.Context,
	namespace string,
	scopes []string,
	correlationID string,
) (*volumeToken, error) {
	body, err := json.Marshal(volumeTokenRequest{
		Scopes:        scopes,
		Namespaces:    []string{namespace},
		CorrelationID: correlationID,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.api.BaseURL, "/")+"/v1/volumes/token", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for name, values := range c.api.Headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.api.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchange volume token: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("exchange volume token: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("exchange volume token: %w",
			&managementapi.ResponseError{StatusCode: resp.StatusCode, Body: string(payload)})
	}

	var decoded volumeTokenResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("exchange volume token: %w", err)
	}
	if decoded.Token == "" {
		return nil, errors.New("exchange volume token: the response carried no token")
	}
	if decoded.Endpoint == nil || *decoded.Endpoint == "" {
		// Distinguished from a malformed response on purpose: this one means
		// the deployment is not serving volumes yet, which the caller can do
		// nothing about and should not see as a transport failure later.
		return nil, fmt.Errorf("exchange volume token: %w", ErrNoVolumeAPI)
	}

	token := &volumeToken{
		token:      decoded.Token,
		endpoint:   *decoded.Endpoint,
		scopes:     decoded.Scopes,
		namespaces: decoded.Namespaces,
	}
	if decoded.ExpiresAt != "" {
		if token.expiresAt, err = time.Parse(time.RFC3339Nano, decoded.ExpiresAt); err != nil {
			return nil, fmt.Errorf("exchange volume token: expires_at: %w", err)
		}
	}
	return token, nil
}

// newCorrelationID mints an identifier for one transfer, which the service
// echoes into its own logs and carries onward as a header. It makes a report
// of "my push failed" answerable from the other side without guessing which
// push.
//
// One per transfer rather than per exchange, so a refresh or a retry mid-push
// correlates with the push it belongs to. The characters are constrained to
// printable ASCII with no spaces, which is what the field accepts; an empty or
// space-bearing value would be rejected, and rejection there fails the whole
// exchange rather than dropping the field.
func newCorrelationID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// The identifier only correlates logs; a transfer should not fail
		// because one could not be minted.
		return "baseten-go"
	}
	return "baseten-go-" + hex.EncodeToString(b[:])
}
