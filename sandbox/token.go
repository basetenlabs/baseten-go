package sandbox

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/managementapi"
)

// TokenProvider returns the bearer token to send on a request. Called for
// every request.
//
// A provider that caches tokens must drop its cached token when it matches
// [TokenProviderOptions.RevokedToken], since the server rejects that token for
// good.
type TokenProvider func(ctx context.Context, opts TokenProviderOptions) (string, error)

// TokenProviderOptions is what a [TokenProvider] is told about the token it is
// asked for.
type TokenProviderOptions struct {
	// RevokedToken is a token the server just rejected as revoked, set when
	// the request is being sent again with a new one. Revocation can happen
	// well before a token's expiry, for example when any API key of the
	// token's user changes.
	RevokedToken string
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Refresh this long before the token's stated expiry, to allow for clock skew
// between this machine and the server, and for time spent in flight.
const tokenExpiryLeeway = 60 * time.Second

// A few retries cover a new token being invalidated in the same event as the
// old one. Past that, the rejection is returned, since tokens that keep
// getting rejected point at something a new token cannot fix.
const tokenInvalidationMaxRetries = 2

// Revocation rejects every token issued before a cutoff, the time of the
// revoking event rounded up to the next whole second. A token minted right
// after the event can fall before the cutoff and be rejected too, so after
// the first resend, each one waits this long to be minted past it. A variable
// so tests can shorten it.
var tokenRevokedRetryDelay = time.Second

// tokenSource is where each request's bearer token comes from: a caller's
// token provider, or a token minted through a management client, cached until
// shortly before it expires.
//
// Tokens are minted because the sandbox APIs do not accept API keys directly.
// Should that change, only this type changes.
type tokenSource struct {
	provider   TokenProvider
	management *client.ManagementClient

	mu      sync.Mutex
	current *tokenMint
}

// tokenMint is one exchange, shared by every caller that asks while it is in
// flight. Its fields are set before done is closed.
type tokenMint struct {
	done      chan struct{}
	token     string
	expiresAt time.Time
	err       error
}

// token returns the token for a request, or the empty string when no
// Authorization should be sent. revokedToken is the token the previous attempt
// was rejected with, if any.
func (s *tokenSource) token(ctx context.Context, revokedToken string) (string, error) {
	if s.provider != nil {
		return s.provider(ctx, TokenProviderOptions{RevokedToken: revokedToken})
	}
	if s.management == nil {
		return "", nil
	}
	for {
		s.mu.Lock()
		mint := s.current
		started := mint == nil
		if started {
			mint = s.startMint(ctx)
			s.current = mint
		}
		s.mu.Unlock()
		select {
		case <-mint.done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if mint.err != nil {
			// Another caller's failed exchange gets one more try of this
			// caller's own; the failed one already left the cache.
			if !started {
				continue
			}
			return "", mint.err
		}
		// A token this caller just minted is used even if already close to
		// expiry, so a skewed clock cannot loop here forever.
		if started || time.Now().Before(mint.expiresAt.Add(-tokenExpiryLeeway)) {
			return mint.token, nil
		}
		s.mu.Lock()
		if s.current == mint {
			s.current = nil
		}
		s.mu.Unlock()
	}
}

// startMint starts an exchange. It runs without the caller's cancellation,
// since one caller giving up must not fail the others waiting on it; each only
// stops waiting on its own. Must be called with mu held.
func (s *tokenSource) startMint(ctx context.Context) *tokenMint {
	mint := &tokenMint{done: make(chan struct{})}
	go func() {
		minted, err := s.management.API().PostToken(context.WithoutCancel(ctx), managementapi.CreateTokenRequest{
			Scopes: []managementapi.TokenScope{managementapi.TokenScope_sandboxes},
		})
		if err != nil {
			mint.err = controlPlaneError(err)
		} else {
			mint.token = minted.Token
			mint.expiresAt = minted.ExpiresAt
		}
		if mint.err != nil {
			// A failed exchange is not cached, so the next request tries
			// again.
			s.mu.Lock()
			if s.current == mint {
				s.current = nil
			}
			s.mu.Unlock()
		}
		close(mint.done)
	}()
	return mint
}

// invalidate drops a token the server rejected, so the next request gets a
// new one.
func (s *tokenSource) invalidate(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return
	}
	select {
	case <-s.current.done:
		// Compared by value, so a rejection of an older token never discards
		// a newer one another request already minted.
		if s.current.token == token {
			s.current = nil
		}
	default:
		// Still minting, so the cached token is already a newer one.
	}
}

// authenticatedClient sends every request with a token from the source, and
// sends it again with a new one when the server rejects the token as revoked.
type authenticatedClient struct {
	inner  httpDoer
	tokens *tokenSource
}

func (c *authenticatedClient) Do(req *http.Request) (*http.Response, error) {
	// A body that cannot be rebuilt can be sent only once, so such a request
	// is never sent again. The SDK itself never sends one; only raw API calls
	// can.
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	var revokedToken string
	for retry := 0; ; retry++ {
		token, err := c.tokens.token(req.Context(), revokedToken)
		if err != nil {
			// Do closes the request body even on error, as http.Client does.
			if req.Body != nil {
				req.Body.Close()
			}
			return nil, err
		}
		attempt := req
		if token != "" {
			attempt = req.Clone(req.Context())
			attempt.Header.Set("Authorization", "Bearer "+token)
		}
		if retry > 0 && req.GetBody != nil {
			if attempt.Body, err = req.GetBody(); err != nil {
				return nil, err
			}
		}
		resp, err := c.inner.Do(attempt)
		if err != nil {
			return nil, err
		}
		if token == "" || !replayable || retry >= tokenInvalidationMaxRetries || !isTokenRevoked(resp) {
			return resp, nil
		}
		c.tokens.invalidate(token)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		revokedToken = token
		if retry > 0 {
			timer := time.NewTimer(tokenRevokedRetryDelay)
			select {
			case <-req.Context().Done():
				timer.Stop()
				return nil, req.Context().Err()
			case <-timer.C:
			}
		}
	}
}

// isTokenRevoked reports whether the server rejected a request's token as
// revoked, which it can do before the token's stated expiry, for example after
// a role change. The request was rejected before any work was done, so sending
// it again with a new token is safe even when it has side effects.
func isTokenRevoked(resp *http.Response) bool {
	// Both the control plane and sandboxes send this header, while their
	// bodies differ.
	return resp.StatusCode == http.StatusUnauthorized && resp.Header.Get("x-blaxel-error-code") == "TOKEN_REVOKED"
}
