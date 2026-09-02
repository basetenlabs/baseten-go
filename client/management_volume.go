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
	push := pushOptions(opts)
	if err := push.Validate(); err != nil {
		return nil, err
	}
	// Folded before the token is scoped to them, so the capability the
	// exchange returns names the volume the transfer will actually address.
	namespace := strings.ToLower(opts.Namespace)
	push.Namespace = namespace
	push.Volume = strings.ToLower(opts.Volume)

	client, _, err := c.volumeClient(namespace, pushScopes(opts), newCorrelationID())
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

// pushOptions translates the public options into the engine's, exhaustively
// and field by field — the form the parity tests require, because a
// struct-copy shortcut is exactly how a twin drifts silently.
func pushOptions(o PushVolumeOptions) transfer.PushOptions {
	opts := transfer.PushOptions{
		Namespace:       o.Namespace,
		Volume:          o.Volume,
		SourceDir:       o.SourceDir,
		SourceURI:       o.SourceURI,
		Tags:            o.Tags,
		RequireHeadMove: o.RequireHeadMove,
		NewHasher:       o.Hasher,
		Concurrency:     internalConcurrency(o.Concurrency),
		Progress:        progressAdapter(o.Progress),
	}
	if o.Store != nil {
		opts.DownloadObject = storeDownloader(o.Store)
		opts.Decompress = o.Store.Decompressor
	}
	return opts
}

func internalConcurrency(c VolumeConcurrencyOptions) volume.Concurrency {
	return volume.Concurrency{
		FileJobs:         c.FileJobs,
		ChunkOperations:  c.ChunkOperations,
		MaxBytesInFlight: c.MaxBytesInFlight,
	}
}

func progressAdapter(fn func(VolumeProgress)) volume.ProgressFunc {
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

// storeDownloader adapts the public store to the engine's downloader seam.
func storeDownloader(store VolumeObjectStore) volume.ObjectDownloader {
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
	pull := pullOptions(opts)
	if err := pull.Validate(); err != nil {
		return nil, err
	}
	ref, err := bdn.ParseRef(opts.Ref)
	if err != nil {
		return nil, err
	}

	client, _, err := c.volumeClient(ref.Namespace, []string{volumeScopePull}, newCorrelationID())
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

// pullOptions translates the public options into the engine's, exhaustively
// and field by field, like pushOptions.
func pullOptions(o DownloadVolumeOptions) transfer.PullOptions {
	opts := transfer.PullOptions{
		Ref:         o.Ref,
		DestDir:     o.DestDir,
		Overwrite:   o.Overwrite,
		Include:     o.Include,
		Restart:     o.Restart,
		NewHasher:   o.Hasher,
		Concurrency: internalConcurrency(o.Concurrency),
		Progress:    progressAdapter(o.Progress),
	}
	if o.Store != nil {
		opts.DownloadObject = storeDownloader(o.Store)
		opts.Decompress = o.Store.Decompressor
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
) (*bdn.Client, *volumeTokenSource, error) {
	// One HTTP client, the management client's own: the per-operation
	// override is gone — a program that needs a different transport
	// configures it once, where every other call already gets it.
	httpClient := c.api.HTTPClient
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
	exchanged, err := s.client.exchangeVolumeToken(exchangeCtx, s.namespace, s.scopes, s.correlationID)
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
// namespace. The call goes through the generated management API client, so
// the base URL, transport, authentication, and user agent behave the same as
// every other management call.
func (c *ManagementClient) exchangeVolumeToken(
	ctx context.Context,
	namespace string,
	scopes []string,
	correlationID string,
) (*volumeToken, error) {
	req := managementapi.CreateVolumeTokenRequest{
		Scopes:     make([]managementapi.VolumeTokenScope, 0, len(scopes)),
		Namespaces: []string{namespace},
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
