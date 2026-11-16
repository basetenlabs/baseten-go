package sandbox

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/internal/tokenauth"
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
// between this machine and the server, and for time spent in flight. Capped at
// half the token's lifetime.
const tokenExpiryLeeway = 60 * time.Second

// Revocation rejects every token issued before a cutoff, the time of the
// revoking event rounded up to the next whole second. A token minted right
// after the event can fall before the cutoff and be rejected too, so after
// the first resend, each one waits this long to be minted past it. A variable
// so tests can shorten it.
var tokenRevokedRetryDelay = time.Second

// tokenMintTimeout bounds one token exchange, which no caller's cancellation
// reaches. A variable so tests can shorten it.
var tokenMintTimeout = 30 * time.Second

// tokenSource is where each request's bearer token comes from: a caller's
// token provider, or a token minted through a management client, cached until
// shortly before it expires.
//
// Tokens are minted because the sandbox APIs do not accept API keys directly.
// Should that change, only this type changes.
type tokenSource struct {
	provider   TokenProvider
	management *client.ManagementClient

	cacheOnce sync.Once
	cache     *tokenauth.Cache
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
	if revokedToken != "" {
		s.invalidate(revokedToken)
	}
	token, err := s.tokenCache().Token(ctx)
	return token.Value, err
}

func (s *tokenSource) tokenCache() *tokenauth.Cache {
	s.cacheOnce.Do(func() {
		s.cache = tokenauth.NewCache(tokenauth.CacheOptions{
			Mint:          s.mint,
			RefreshBefore: tokenExpiryLeeway,
			MintTimeout:   tokenMintTimeout,
		})
	})
	return s.cache
}

func (s *tokenSource) mint(ctx context.Context) (tokenauth.Token, error) {
	minted, err := s.management.API().PostToken(ctx, managementapi.CreateTokenRequest{
		Scopes: []managementapi.TokenScope{managementapi.TokenScope_sandboxes},
	})
	if err != nil {
		return tokenauth.Token{}, controlPlaneError(err)
	}
	return tokenauth.Token{Value: minted.Token, ExpiresAt: minted.ExpiresAt}, nil
}

// invalidate drops a token the server rejected, so the next request gets a
// new one.
func (s *tokenSource) invalidate(token string) {
	if s.provider == nil && s.management != nil {
		s.tokenCache().Invalidate(token)
	}
}

// authenticatedClient sends every request with a token from the source, and
// sends it again with a new one when the server rejects the token as revoked.
type authenticatedClient struct {
	inner  httpDoer
	tokens *tokenSource
}

func (c *authenticatedClient) Do(req *http.Request) (*http.Response, error) {
	return tokenauth.Do(c.inner, req, c.tokens.token, c.tokens.invalidate, tokenRevokedRetryDelay)
}
