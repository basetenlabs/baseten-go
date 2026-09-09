package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
	"github.com/basetenlabs/baseten-go/internal/volume/transfer"
	"hash"
	"io"
)

// PushVolume uploads a directory and publishes it as a new version of a
// volume. [PushVolumeOptions] configures it.
//
// A volume is a versioned directory tree stored by content, so pushing a
// tree that mostly matches one already stored transfers only what differs,
// and downloading the same version twice transfers nothing the second time.
//
// Nothing is visible until the whole tree has been uploaded, so an interrupted
// push publishes nothing. What it uploaded is not wasted: the next push finds
// those objects already stored and skips them.
func (c *ManagementClient) PushVolume(ctx context.Context, opts PushVolumeOptions) (*PushVolumeResult, error) {
	push := volumePushOptions(opts)
	if err := push.Validate(); err != nil {
		return nil, err
	}
	// The exchange reads the namespace from the translated options rather
	// than folding the caller's value a second time: one fold, in the
	// translation, so the namespace the capability token is scoped to and
	// the one the transfer addresses cannot drift apart.
	client, _, err := c.volumeClient(
		push.Namespace, push.Volume, volumePushScopes(opts), newVolumeCorrelationID())
	if err != nil {
		return nil, err
	}

	result, err := transfer.Push(ctx, client, push)
	if err != nil {
		return nil, volumeOpError(err)
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

// volumeOpError dresses a volume-service error in the public error type,
// carrying the stable reason and the service's message and wrapping the
// whole original chain, so sentinel matches through errors.Is keep working.
// An error that is not the service's own — a local filesystem failure, a
// cancelled context — comes back as itself.
func volumeOpError(err error) error {
	var se *volume.Error
	if errors.As(err, &se) {
		return &VolumeError{Reason: VolumeErrorReason(se.Reason), Message: se.Message, Err: err}
	}
	return err
}

// volumePushOptions translates the public options into the engine's, exhaustively
// and field by field — the form the parity tests require, because a
// struct-copy shortcut is exactly how a twin drifts silently.
func volumePushOptions(o PushVolumeOptions) transfer.PushOptions {
	opts := transfer.PushOptions{
		// Namespace and Volume fold to lowercase here, with the other
		// conversions, and before the token is scoped to them: the capability
		// the exchange returns must name the volume the transfer will
		// actually address.
		Namespace:       strings.ToLower(o.Namespace),
		Volume:          strings.ToLower(o.Volume),
		SourceDir:       o.SourceDir,
		SourceURI:       o.SourceURI,
		Tags:            o.Tags,
		RequireHeadMove: o.RequireHeadMove,
		NewHasher:       o.Hasher,
		// The concurrency literal is keyed and exhaustive for the same reason
		// the translation is: the parity tests hold the twins together, and a
		// field dropped here is a silently ignored option.
		Concurrency: volume.Concurrency{
			FileJobs:         o.Concurrency.FileJobs,
			ChunkOperations:  o.Concurrency.ChunkOperations,
			MaxBytesInFlight: o.Concurrency.MaxBytesInFlight,
		},
		Progress: volumeProgressAdapter(o.Progress),
	}
	if o.Store != nil {
		opts.DownloadObject = volumeStoreDownloader(o.Store)
		opts.Decompress = o.Store.Decompressor
	}
	return opts
}

func volumeProgressAdapter(fn func(VolumeProgress)) volume.ProgressFunc {
	if fn == nil {
		return nil
	}
	return func(p volume.Progress) {
		fn(VolumeProgress{
			Phase:      VolumePhase(p.Phase),
			Files:      p.Files,
			TotalFiles: p.TotalFiles,
			Bytes:      p.Bytes,
			TotalBytes: p.TotalBytes,
		})
	}
}

// volumeStoreDownloader adapts the public store to the engine's downloader seam.
func volumeStoreDownloader(store VolumeObjectStore) volume.ObjectDownloader {
	return func(ctx context.Context, req volume.ObjectDownload) (*volume.ObjectResult, error) {
		res, err := store.DownloadObject(ctx, VolumeObjectDownload{
			Endpoint: req.Endpoint,
			Region:   req.Region,
			Bucket:   req.Bucket,
			Key:      req.Key,
			Credentials: VolumeObjectCredentials{
				AccessKeyID:     req.Credentials.AccessKeyID,
				SecretAccessKey: req.Credentials.SecretAccessKey,
				SessionToken:    req.Credentials.SessionToken,
			},
			ExpectedSize: req.ExpectedSize,
		})
		if err != nil || res == nil {
			return nil, err
		}
		return &volume.ObjectResult{Body: res.Body, ContentType: res.ContentType, Size: res.Size}, nil
	}
}

// DownloadVolume downloads a version of a volume into a directory.
// [DownloadVolumeOptions] configures it.
//
// A volume is a versioned directory tree stored by content, so pushing a
// tree that mostly matches one already stored transfers only what differs,
// and downloading the same version twice transfers nothing the second time.
//
// Every chunk is checked against its recorded digest before it is written, so
// a corrupted or truncated read fails the download rather than producing a
// file that merely looks complete. An interrupted download can be run again
// and picks up where it stopped.
func (c *ManagementClient) DownloadVolume(ctx context.Context, opts DownloadVolumeOptions) (*DownloadVolumeResult, error) {
	pull := volumePullOptions(opts)
	if err := pull.Validate(); err != nil {
		return nil, err
	}
	ref, err := bdn.ParseRef(opts.Ref)
	if err != nil {
		return nil, err
	}

	client, _, err := c.volumeClient(
		ref.Namespace, ref.Volume, []string{volumeScopePull}, newVolumeCorrelationID())
	if err != nil {
		return nil, err
	}

	result, err := transfer.Pull(ctx, client, pull)
	if err != nil {
		return nil, volumeOpError(err)
	}
	warnings := make([]VolumeWarning, 0, len(result.Warnings))
	for _, w := range result.Warnings {
		warnings = append(warnings, VolumeWarning{Path: w.Path, Kind: VolumeWarningKind(w.Kind), Detail: w.Detail})
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
		Warnings:       warnings,
	}, nil
}

// volumePullOptions translates the public options into the engine's, exhaustively
// and field by field, like volumePushOptions.
func volumePullOptions(o DownloadVolumeOptions) transfer.PullOptions {
	opts := transfer.PullOptions{
		Ref:       o.Ref,
		DestDir:   o.DestDir,
		Overwrite: o.Overwrite,
		Include:   o.Include,
		Restart:   o.Restart,
		NewHasher: o.Hasher,
		// Keyed and exhaustive like volumePushOptions's, and for the same
		// reason.
		Concurrency: volume.Concurrency{
			FileJobs:         o.Concurrency.FileJobs,
			ChunkOperations:  o.Concurrency.ChunkOperations,
			MaxBytesInFlight: o.Concurrency.MaxBytesInFlight,
		},
		Progress: volumeProgressAdapter(o.Progress),
	}
	if o.Store != nil {
		opts.DownloadObject = volumeStoreDownloader(o.Store)
		opts.Decompress = o.Store.Decompressor
	}
	return opts
}

// volumeClient builds a protocol client whose credentials come from exchanging
// the API key for a capability token over a namespace and a volume.
//
// The token is narrowed to the one volume the transfer addresses and carries
// only the scopes asked for, so credentials are not reusable across volumes.
func (c *ManagementClient) volumeClient(
	namespace string,
	volume string,
	scopes []string,
	correlationID string,
) (*bdn.Client, *volumeTokenSource, error) {
	// One HTTP client, the management client's own: the per-operation
	// override is gone — a program that needs a different transport
	// configures it once, where every other call already gets it.
	httpClient := c.api.HTTPClient
	tokens := c.volumeTokenSource(namespace, volume, scopes, correlationID)
	client, err := bdn.New(bdn.Options{
		HTTPClient: httpClient,
		Tokens:     tokens.tokenSource(),
	})
	if err != nil {
		return nil, nil, err
	}
	return client, tokens, nil
}

// volumePushScopes is what a push asks for, and asks for nothing beyond it.
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
func volumePushScopes(opts PushVolumeOptions) []string {
	scopes := []string{volumeScopePush}
	if len(opts.Tags) > 0 {
		scopes = append(scopes, volumeScopeTag)
	}
	if opts.Store != nil {
		scopes = append(scopes, volumeScopePull)
	}
	return scopes
}

// Scopes a capability token can carry. A token is granted a set of these for
// a set of namespaces. The exchange refuses a scope it will not grant rather
// than returning a smaller set, so the response's scope list is the request
// echoed back, never a narrowing.
const (
	volumeScopePull = "PULL"
	volumeScopePush = "PUSH"
	volumeScopeTag  = "TAG"
)

// ErrMalformedVolumeEndpoint reports a token-exchange response whose volume
// endpoint is present but unusable — an empty string where a URL belongs. It
// is a service-side defect, deliberately distinct from [ErrNoVolumeAPI]'s
// null, which is the deployment saying it has no volume API at all. It stays
// a sentinel here rather than becoming a reason on the volume error type:
// reasons report the volume service refusing an operation, and this response
// comes from the management API's exchange before that service is ever
// reached — a reason would report a refusal from a service that never spoke.
var ErrMalformedVolumeEndpoint = errors.New("the token exchange returned an empty volume endpoint")

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

// tokenExchangeTimeout bounds one token exchange. The exchange runs while
// the token source's mutex is held — every other operation on the transfer
// queues behind it — so a hung network call must not be allowed to hold the
// lock until the caller's own deadline, which a long push may not have set
// at all. Ten seconds is generous for one small HTTPS POST and still frees
// the mutex promptly when the management API is unreachable.
const tokenExchangeTimeout = 10 * time.Second

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
	// kept for diagnostics. Nothing is decided from scopes: the response
	// describes them as the capabilities granted, but the exchange refuses a
	// scope it will not grant rather than returning a smaller set, so the list
	// is the request echoed back. A field that cannot differ from what was
	// sent carries no information, and if it ever began to differ, believing
	// it would mean claiming a capability the minted token does not hold.
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
func (c *ManagementClient) volumeTokenSource(
	namespace string, volume string, scopes []string, correlationID string,
) *volumeTokenSource {
	return &volumeTokenSource{
		client:        c,
		namespace:     namespace,
		volume:        volume,
		scopes:        scopes,
		correlationID: correlationID,
		proactive:     true,
	}
}

type volumeTokenSource struct {
	client        *ManagementClient
	namespace     string
	volume        string
	scopes        []string
	correlationID string

	mu      sync.Mutex
	current *volumeToken

	// proactive goes false once a replacement arrives already inside the
	// refresh margin. A freshly minted token that is already near expiry is
	// pathological — it means the server's TTL fits inside our refresh
	// margin — and the latch chooses degraded once-per-refusal refresh over
	// hammering the exchange's rate limit for nothing. A deployment issuing tokens shorter than the margin — or
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
	// Bounded because the mutex is held across this call; see
	// tokenExchangeTimeout.
	exchangeCtx, cancel := context.WithTimeout(ctx, tokenExchangeTimeout)
	defer cancel()
	exchanged, err := s.client.exchangeVolumeToken(
		exchangeCtx, s.namespace, s.volume, s.scopes, s.correlationID)
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

// exchangeVolumeToken trades the API key for a capability token over a
// namespace, narrowed to a single volume, which the endpoint requires. The
// call goes through the generated management API client, so
// the base URL, transport, authentication, and user agent behave the same as
// every other management call.
func (c *ManagementClient) exchangeVolumeToken(
	ctx context.Context,
	namespace string,
	volume string,
	scopes []string,
	correlationID string,
) (*volumeToken, error) {
	req := managementapi.CreateVolumeTokenRequest{
		Scopes:     make([]managementapi.VolumeTokenScope, 0, len(scopes)),
		Namespaces: []string{namespace},
		Volumes:    []string{volume},
	}
	for _, scope := range scopes {
		req.Scopes = append(req.Scopes, managementapi.VolumeTokenScope(scope))
	}
	if correlationID != "" {
		req.CorrelationId = &correlationID
	}

	decoded, err := c.api.PostVolumesToken(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("exchange volume token: %w", err)
	}
	if decoded.Token == "" {
		return nil, errors.New("exchange volume token: the response carried no token")
	}
	if decoded.BdnEndpoint == nil {
		// Distinguished from a malformed response on purpose: null means the
		// deployment is not serving volumes yet, which the caller can do
		// nothing about and should not see as a transport failure later.
		return nil, fmt.Errorf("exchange volume token: %w", ErrNoVolumeAPI)
	}
	if *decoded.BdnEndpoint == "" {
		// An empty string is not the deployment saying "no volumes" — null
		// says that — it is a response this client cannot use, and calling it
		// a missing capability would send an operator hunting deployment
		// configuration for what is actually a service bug.
		return nil, fmt.Errorf("exchange volume token: %w", ErrMalformedVolumeEndpoint)
	}

	granted := make([]string, 0, len(decoded.Scopes))
	for _, scope := range decoded.Scopes {
		granted = append(granted, string(scope))
	}
	return &volumeToken{
		token:      decoded.Token,
		endpoint:   *decoded.BdnEndpoint,
		expiresAt:  decoded.ExpiresAt,
		scopes:     granted,
		namespaces: decoded.Namespaces,
	}, nil
}

// newVolumeCorrelationID mints an identifier for one transfer, which the service
// echoes into its own logs and carries onward as a header. It makes a report
// of "my push failed" answerable from the other side without guessing which
// push.
//
// One per transfer rather than per exchange, so a refresh or a retry mid-push
// correlates with the push it belongs to. The characters are constrained to
// printable ASCII with no spaces, which is what the field accepts; an empty or
// space-bearing value would be rejected, and rejection there fails the whole
// exchange rather than dropping the field.
func newVolumeCorrelationID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// The identifier only correlates logs; a transfer should not fail
		// because one could not be minted.
		return "baseten-go"
	}
	return "baseten-go-" + hex.EncodeToString(b[:])
}

// The volume vocabulary — options, results, progress, warnings, the
// concurrency knobs, and the seam a caller fills in — is defined in this file,
// fully and without aliases; the error type lives in volume_errors.go and the
// operations are methods on ManagementClient in management_volume.go. The
// translation between these types and the internal engines is deliberately
// exhaustive and field-by-field, and the parity tests hold the two sides
// together: a field added on either side without its twin is a red test.
// The engines' mechanics — the limiter, its permit, its outcome — are never
// public; their semantics changed twice in one review cycle, and neither
// change would have been API-compatible had they been exported.

// VolumeConcurrencyOptions is a transfer's concurrency limits. The zero value
// means defaults: concurrent files follow the pinned object-operation count
// when one is set and otherwise default to 256, in-flight object operations
// adapt to what the origin will bear, and two gibibytes of chunk data may be
// resident in memory.
//
// A wide fan-out is served best by an injected HTTPClient whose transport
// raises MaxIdleConnsPerHost toward the operation ceiling: the default
// transport parks only two idle connections per host, so most requests in a
// wide wave open a fresh connection instead of reusing one.
type VolumeConcurrencyOptions struct {
	// FileJobs is how many files are processed concurrently on push. Zero
	// means the default.
	FileJobs int

	// ChunkOperations pins how many object operations may be in flight. Zero
	// does not mean "some default": it means the limit is not pinned, and a
	// transfer adapts it to what the origin will bear. Setting it caps the
	// load a transfer places on a shared machine or a metered link, and is
	// honoured exactly.
	ChunkOperations int

	// MaxBytesInFlight caps the chunk data resident in memory. Zero means
	// the two-gibibyte default.
	MaxBytesInFlight int64
}

// VolumePhase names the part of a transfer that progress is being reported
// for.
type VolumePhase string

const (
	VolumePhaseScan     VolumePhase = "scan"
	VolumePhaseUpload   VolumePhase = "upload"
	VolumePhaseCommit   VolumePhase = "commit"
	VolumePhaseResolve  VolumePhase = "resolve"
	VolumePhaseDownload VolumePhase = "download"
	VolumePhasePublish  VolumePhase = "publish"
)

// VolumeProgress is a transfer's state at one moment. Counts within a phase
// only ever increase.
type VolumeProgress struct {
	Phase VolumePhase

	Files      int64
	TotalFiles int64

	Bytes      int64
	TotalBytes int64
}

// VolumeObjectCredentials are the short-lived read-only credentials the
// volume service leases for reading a namespace's objects.
type VolumeObjectCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// VolumeObjectDownload names one object to read from the store.
type VolumeObjectDownload struct {
	// Endpoint is empty for AWS itself and a base URL otherwise. The two
	// address buckets differently, which is why the distinction is passed
	// through rather than resolved here.
	Endpoint string
	Region   string
	Bucket   string
	Key      string

	Credentials VolumeObjectCredentials

	// ExpectedSize is the object's size when it is known, and zero when it
	// is not. A chunk's length comes from the manifest, so it is known; a
	// manifest's own size is not.
	ExpectedSize int64
}

// VolumeObjectResult is an open object.
type VolumeObjectResult struct {
	Body io.ReadCloser

	// ContentType is the stored media type, and is the only thing that says
	// how the bytes are encoded. The service decides at write time whether
	// to compress an object, and the key does not record what it chose, so
	// a reader that guessed from the key would eventually guess wrong.
	ContentType string

	// Size is the stored length, which for a compressed object is the
	// compressed length rather than the object's own.
	Size int64
}

// VolumeObjectStore reads stored objects. The caller supplies it so this
// module needs no cloud SDK or compression library of its own; the two
// methods are one interface because they are only ever usable together — the
// service decides per object whether to compress what it stores, so a reader
// that can download but not decompress will fail on an arbitrary subset of
// objects. aws-sdk-go-v2's GetObject and github.com/klauspost/compress/zstd
// fill the two methods.
//
// DownloadObject owns its own retrying: an error returned from it has
// already exhausted whatever budget the implementation has, and ends the
// operation that asked for the object.
type VolumeObjectStore interface {
	// DownloadObject opens one object for reading.
	DownloadObject(ctx context.Context, req VolumeObjectDownload) (*VolumeObjectResult, error)

	// Decompressor wraps a reader of zstd-compressed bytes in one that
	// yields the original bytes.
	Decompressor(r io.Reader) (io.ReadCloser, error)
}

// VolumeWarningKind names what a download's containment check found, so a
// caller can branch without parsing prose.
type VolumeWarningKind uint8

const (
	// VolumeWarningKindDanglingLink: a symlink's target resolves to a path
	// the volume has nothing at.
	VolumeWarningKindDanglingLink VolumeWarningKind = iota + 1
	// VolumeWarningKindLinkThroughFile: a symlink resolves through an entry
	// recorded as a file, which a real filesystem answers with ENOTDIR.
	VolumeWarningKindLinkThroughFile
	// VolumeWarningKindParentUnrecorded: an entry's ancestors up to the
	// nearest recorded one are all implicit — nothing records the parent
	// directory.
	VolumeWarningKindParentUnrecorded
	// VolumeWarningKindPathNormalized: the entry's path was recorded
	// root-anchored — "/a/b" — and was normalized to a relative one on
	// download. Only volumes published before the containment rule carry
	// that shape; pushing it is refused, so this warning marks a legacy
	// volume, never a fresh push.
	VolumeWarningKindPathNormalized
)

// VolumeWarning is a containment finding that did not stop a download:
// harmless to write out, present in volumes published before the containment
// rule, and worth telling the caller about.
type VolumeWarning struct {
	// Path is the entry the finding is about.
	Path string
	// Kind is the finding, typed.
	Kind VolumeWarningKind
	// Detail says what was found, in prose.
	Detail string
}

// String renders the finding for a human; the typed fields are the API.
func (w VolumeWarning) String() string {
	return w.Path + ": " + w.Detail
}

// PushVolumeOptions configures [ManagementClient.PushVolume].
type PushVolumeOptions struct {
	// Namespace and Volume name where to publish. The volume is created if
	// it does not exist; the namespace must already.
	Namespace string
	Volume    string

	// SourceDir is the directory to push. It is walked without following
	// symlinks, which are recorded as links.
	SourceDir string

	// Tags are applied atomically with the publish, so a tag never points at
	// a version that is only partly uploaded. The reserved name "head" is
	// not a tag; see RequireHeadMove.
	Tags []string

	// RequireHeadMove fails the push before it uploads anything if the
	// credential does not carry permission to move the volume's head.
	// Without it, such a push still publishes the version, and refs without
	// a tag keep resolving to the previous one — reported as HeadMoveDenied.
	//
	// See [PushVolumeResult.HeadMoveDenied]: with a credential from this
	// client's own exchange, the condition this guards against is not
	// expected to arise.
	RequireHeadMove bool

	// Hasher returns an unkeyed BLAKE3 hash with a 32-byte digest. Required —
	// the digest is the whole content-addressing scheme, so getting it wrong
	// produces a volume no other client can read, and it is supplied here so
	// this module carries no hashing library of its own. Both common
	// libraries produce the right hash when told the size explicitly or by
	// default:
	//
	//	github.com/zeebo/blake3:   func() hash.Hash { return blake3.New() }
	//	lukechampine.com/blake3:   func() hash.Hash { return blake3.New(32, nil) }
	//
	// A 64-byte extended output is the mistake to watch for; the hasher is
	// checked against the published test vectors before a transfer starts.
	Hasher func() hash.Hash

	// Store lets the push read the volume's previous version, so a file
	// whose bytes have not changed is not uploaded again. Optional: without
	// it the push uploads everything, which is slower and produces the same
	// version.
	Store VolumeObjectStore

	// Progress is called as the push proceeds, never concurrently with
	// itself. It should return promptly.
	Progress func(VolumeProgress)

	// Concurrency overrides the transfer's concurrency limits. The zero
	// value means defaults.
	Concurrency VolumeConcurrencyOptions

	// SourceURI records where the tree came from, and is inside the bytes
	// the version's digest covers: two pushes of identical trees from
	// different paths are different versions. Empty derives it from
	// SourceDir, which means a push from a different absolute path publishes
	// a new version of an unchanged tree. Set it to a stable string to avoid
	// that.
	SourceURI string
}

// PushVolumeResult is what a push published.
type PushVolumeResult struct {
	// ManifestDigest names the published version: the string "b3:" followed
	// by sixty-four lowercase hex digits — an unkeyed BLAKE3 hash with a
	// 32-byte output over the manifest's content bytes. It is what to pin a
	// download to in order to get this exact tree back.
	ManifestDigest string

	// Sequence is the volume's snapshot sequence at this publish.
	Sequence int64

	// HeadUpdated reports whether this push moved head. It is false when
	// head already pointed at this exact version, which is what re-pushing
	// an unchanged tree does — so false does not mean untagged refs resolve
	// elsewhere, only that nothing had to move.
	HeadUpdated bool

	// HeadMoveDenied reports that the push did not ask to move head, because
	// the credential's grants do not cover it. The version was published and
	// can be reached by digest or tag; what did not happen is head moving to
	// it.
	//
	// Not expected to occur with a credential obtained the way this client
	// obtains one. The exchange grants push and tag together or refuses
	// outright, and the credential it mints covers every volume and tag in
	// the namespaces it names — so a credential that can push can also move
	// head. The field describes a real state of the underlying protocol,
	// which admits credentials minted other ways, and reports what this
	// client actually asked for rather than asserting what the service
	// would allow.
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

// DownloadVolumeOptions configures [ManagementClient.DownloadVolume].
type DownloadVolumeOptions struct {
	// Ref names what to download: "namespace/volume" for the current
	// version, "namespace/volume:tag", or "namespace/volume@b3:..." for one
	// exact version. Whatever it names is resolved once and pinned, so a tag
	// that moves partway through cannot produce a tree assembled from two
	// versions.
	Ref string

	// DestDir is where the tree ends up. Unless Overwrite is set it must not
	// exist or must be empty, and the tree is assembled beside it and moved
	// into place only once it is complete.
	DestDir string

	// Overwrite writes into an existing DestDir in place. Files already
	// there that the volume does not describe are left alone; a failed
	// download leaves a partly written directory rather than an untouched
	// one.
	Overwrite bool

	// Include narrows the download to named entries. Each is an exact path
	// or a directory whose contents are wanted, matched on slash boundaries.
	// One that matches nothing fails the download rather than silently
	// producing less than was asked for. Empty downloads the whole volume.
	Include []string

	// Restart discards a partly downloaded tree from an earlier attempt
	// instead of continuing it.
	Restart bool

	// Hasher returns an unkeyed BLAKE3 hash with a 32-byte digest. Required:
	// every chunk is verified against the digest recorded for it, and
	// continuing an interrupted download works by hashing what is already on
	// disk. It is supplied here so this module carries no hashing library of
	// its own, and getting it wrong fails every verification. Both common
	// libraries produce the right hash when told the size explicitly or by
	// default:
	//
	//	github.com/zeebo/blake3:   func() hash.Hash { return blake3.New() }
	//	lukechampine.com/blake3:   func() hash.Hash { return blake3.New(32, nil) }
	//
	// A 64-byte extended output is the mistake to watch for; the hasher is
	// checked against the published test vectors before a transfer starts.
	Hasher func() hash.Hash

	// Store reads the volume's objects. Required: the service compresses
	// what it stores at its own discretion, so a reader has to be able to
	// download and decompress whatever comes back.
	Store VolumeObjectStore

	// Progress is called as the download proceeds, never concurrently with
	// itself.
	Progress func(VolumeProgress)

	// Concurrency overrides the transfer's concurrency limits. The zero
	// value means defaults.
	Concurrency VolumeConcurrencyOptions
}

// DownloadVolumeResult is what a download produced.
type DownloadVolumeResult struct {
	// VersionRef is the ref pinned to the version that was downloaded, which
	// is the form to quote to get this exact tree again.
	VersionRef string

	// ManifestDigest names the version that was downloaded: the string "b3:"
	// followed by sixty-four lowercase hex digits — an unkeyed BLAKE3 hash
	// with a 32-byte output over the manifest's content bytes.
	ManifestDigest string

	// Files and Bytes are what was written.
	Files int64
	Bytes int64

	// SelectedFiles and TotalFiles report what Include narrowed to. They are
	// equal when the whole volume was downloaded.
	SelectedFiles int64
	TotalFiles    int64

	// ChunksFetched and ChunksReused partition the chunks. Reused were
	// already on disk with the right contents, from an earlier attempt.
	ChunksFetched int64
	ChunksReused  int64

	// Warnings are the containment findings that did not stop the download —
	// a dangling link, a link through a file, an entry whose parent has no
	// record, a root-anchored path normalized on the way out. Volumes
	// published before the containment rule carry these; they are written
	// out faithfully and reported here rather than silently.
	Warnings []VolumeWarning
}

// VolumeErrorReason is the stable constant naming what specifically went
// wrong with a volume operation. It mirrors the service's own reason strings,
// so a reason the service adds tomorrow flows through a [VolumeError] without
// new API here — the constants below are the ones a caller has a reason to
// branch on, not the whole registry.
type VolumeErrorReason string

const (
	// VolumeErrorReasonUploadSessionExpired means the push outlived its
	// session. There is no way to extend one, so the work has to start over.
	VolumeErrorReasonUploadSessionExpired VolumeErrorReason = "UPLOAD_SESSION_EXPIRED"

	// VolumeErrorReasonCASConflict means someone else published while this
	// push was running: the view of the volume was stale.
	VolumeErrorReasonCASConflict VolumeErrorReason = "CAS_CONFLICT"

	VolumeErrorReasonNotFound           VolumeErrorReason = "NOT_FOUND"
	VolumeErrorReasonPermissionDenied   VolumeErrorReason = "PERMISSION_DENIED"
	VolumeErrorReasonAmbiguousPrefix    VolumeErrorReason = "AMBIGUOUS_PREFIX"
	VolumeErrorReasonRateLimited        VolumeErrorReason = "RATE_LIMITED"
	VolumeErrorReasonChunkTooLarge      VolumeErrorReason = "CHUNK_TOO_LARGE"
	VolumeErrorReasonManifestTooLarge   VolumeErrorReason = "MANIFEST_TOO_LARGE"
	VolumeErrorReasonServiceUnavailable VolumeErrorReason = "UNAVAILABLE"
)

// VolumeError is what a volume operation returns when the volume service
// refused or failed it. Match with errors.As:
//
//	var ve *client.VolumeError
//	if errors.As(err, &ve) && ve.Reason == client.VolumeErrorReasonCASConflict { ... }
//
// Errors that are not the service's own — a local filesystem failure, a
// cancelled context, a malformed response — come back as themselves,
// wrapped with context, not as a VolumeError with an invented reason.
type VolumeError struct {
	// Reason is the stable constant to branch on. A message is for humans
	// and changes between releases.
	Reason VolumeErrorReason

	// Message is the service's human-readable description.
	Message string

	// Err is the underlying error, when there is one.
	Err error
}

func (e *VolumeError) Error() string {
	if e.Message != "" {
		return string(e.Reason) + ": " + e.Message
	}
	return string(e.Reason)
}

func (e *VolumeError) Unwrap() error {
	return e.Err
}
