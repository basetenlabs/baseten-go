package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/internal/tokenauth"
)

// The token exchange takes an RFC 8693 request naming the trust rule as its audience.
const (
	tokenExchangeGrantType = "urn:ietf:params:oauth:grant-type:token-exchange"
	jwtTokenType           = "urn:ietf:params:oauth:token-type:jwt"
)

// defaultWorkloadIdentityRefreshLeeway is how long before expiry a cached
// access token is replaced. Capped at half the token's lifetime so a
// short-lived token is still reused.
const defaultWorkloadIdentityRefreshLeeway = 5 * time.Minute

// AccessToken is a short-lived Baseten access token, sent as
// "Authorization: Bearer <Token>".
type AccessToken struct {
	Token     string
	ExpiresAt time.Time
}

// Credentials supplies a current Baseten access token. A client
// configured with one calls it before every request, so the credentials decide
// when to refresh.
type Credentials interface {
	AccessToken(ctx context.Context, opts AccessTokenOptions) (AccessToken, error)
}

// AccessTokenOptions is what [Credentials] are told about the token they are
// asked for.
type AccessTokenOptions struct {
	// RefusedToken is a token Baseten refused before its stated expiry.
	// Credentials that cache tokens must not return it again.
	RefusedToken string
}

// IdentityTokenOptions is passed to
// [WorkloadIdentityCredentialsOptions.IdentityToken]. It is empty for now;
// fields may be added in later versions.
type IdentityTokenOptions struct{}

// WorkloadIdentityCredentialsOptions configures WorkloadIdentityCredentials.
// Credentials are supplied explicitly; the SDK reads no environment variables.
type WorkloadIdentityCredentialsOptions struct {
	// IdentityTokenFile is a file holding the identity provider's JWT (for
	// example a projected Kubernetes service account token). It is re-read on
	// every exchange, since such files are rotated in place. Exactly one of
	// IdentityTokenFile and IdentityToken is required.
	IdentityTokenFile string

	// IdentityToken returns the identity provider's JWT. It is called on every
	// exchange. Exactly one of IdentityTokenFile and IdentityToken is required.
	IdentityToken func(ctx context.Context, opts IdentityTokenOptions) (string, error)

	// TrustRuleID is the Baseten trust rule the identity token is exchanged
	// under. Required.
	TrustRuleID string

	// BaseURL is the management API base URL the exchange is sent to. If
	// empty, "https://api.baseten.co" is used.
	BaseURL string

	// HTTPClient sends the exchange. If nil, [NewDefaultHTTPClient] is used,
	// which does not follow redirects. A custom client should also not follow
	// redirects, since the request body carries the identity token.
	HTTPClient interface {
		Do(*http.Request) (*http.Response, error)
	}

	// RefreshLeeway is how long before expiry the access token is replaced.
	// If zero, five minutes is used. Either way it is capped at half the
	// token's lifetime.
	RefreshLeeway time.Duration
}

// WorkloadIdentityCredentials exchanges an identity provider's JWT for a
// Baseten access token under a trust rule (OIDC workload identity
// federation). The exchange happens on first use, the access token is cached
// in memory and replaced shortly before it expires. Safe for concurrent use;
// concurrent callers share one exchange.
type WorkloadIdentityCredentials struct {
	opts  WorkloadIdentityCredentialsOptions
	api   *managementapi.Client
	cache *tokenauth.Cache

	// now is replaced in tests.
	now func() time.Time
}

var _ Credentials = (*WorkloadIdentityCredentials)(nil)

// NewWorkloadIdentityCredentials validates opts and returns credentials. It
// does not contact Baseten; the first exchange happens on first use.
func NewWorkloadIdentityCredentials(opts WorkloadIdentityCredentialsOptions) (*WorkloadIdentityCredentials, error) {
	if (opts.IdentityTokenFile == "") == (opts.IdentityToken == nil) {
		return nil, errors.New("exactly one of IdentityTokenFile and IdentityToken is required")
	}
	if opts.TrustRuleID == "" {
		return nil, errors.New("TrustRuleID is required")
	}
	if opts.RefreshLeeway < 0 {
		return nil, errors.New("RefreshLeeway must not be negative")
	}
	if opts.RefreshLeeway == 0 {
		opts.RefreshLeeway = defaultWorkloadIdentityRefreshLeeway
	}
	baseURL := strings.TrimRight(opts.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.baseten.co"
	}
	var httpClient httpDoer = opts.HTTPClient
	if httpClient == nil {
		httpClient = NewDefaultHTTPClient()
	}
	headers := http.Header{}
	ApplyUserAgentHeader(headers)
	c := &WorkloadIdentityCredentials{
		opts: opts,
		// No credentials: the identity token in the body is what authenticates.
		api: &managementapi.Client{BaseURL: baseURL, HTTPClient: httpClient, Headers: headers},
		now: time.Now,
	}
	c.cache = tokenauth.NewCache(tokenauth.CacheOptions{
		Mint:          c.exchange,
		RefreshLeeway: opts.RefreshLeeway,
		MintTimeout:   workloadIdentityExchangeTimeout,
		Now:           func() time.Time { return c.now() },
	})
	return c, nil
}

// AccessToken returns a current access token, exchanging the identity token
// for a new one when none is cached or the cached one is within the refresh
// leeway of expiry.
func (c *WorkloadIdentityCredentials) AccessToken(ctx context.Context, opts AccessTokenOptions) (AccessToken, error) {
	if opts.RefusedToken != "" {
		c.cache.Invalidate(opts.RefusedToken)
	}
	token, err := c.cache.Token(ctx)
	if err != nil {
		return AccessToken{}, err
	}
	return AccessToken{Token: token.Value, ExpiresAt: token.ExpiresAt}, nil
}

func (c *WorkloadIdentityCredentials) identityToken(ctx context.Context) (string, error) {
	if c.opts.IdentityToken != nil {
		token, err := c.opts.IdentityToken(ctx, IdentityTokenOptions{})
		if err != nil {
			return "", fmt.Errorf("getting identity token: %w", err)
		}
		token = strings.TrimSpace(token)
		if token == "" {
			return "", errors.New("getting identity token: the token provider returned an empty token")
		}
		return token, nil
	}
	data, err := os.ReadFile(c.opts.IdentityTokenFile)
	if err != nil {
		return "", fmt.Errorf("reading identity token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("reading identity token file %s: the file is empty", c.opts.IdentityTokenFile)
	}
	return token, nil
}

// exchange swaps the identity token for a new access token.
func (c *WorkloadIdentityCredentials) exchange(ctx context.Context) (tokenauth.Token, error) {
	assertion, err := c.identityToken(ctx)
	if err != nil {
		return tokenauth.Token{}, err
	}
	// Taken before sending, so the computed expiry errs early.
	issuedAt := c.now()
	resp, err := c.api.PostAuthToken(ctx, managementapi.TokenExchangeRequest{
		GrantType:        tokenExchangeGrantType,
		SubjectToken:     assertion,
		SubjectTokenType: jwtTokenType,
		Audience:         c.opts.TrustRuleID,
	})
	if err != nil {
		// The server sends every refusal as the same OAuth invalid_grant, so
		// the hint covers each possible cause.
		var respErr *managementapi.ResponseError
		if errors.As(err, &respErr) && isInvalidGrant(respErr.Body) {
			return tokenauth.Token{}, fmt.Errorf(
				"workload identity token exchange refused for trust rule %s "+
					"(the trust rule may not exist or may not match the identity token's issuer, audience, or claims, "+
					"or the identity token may have expired): %w",
				c.opts.TrustRuleID, err)
		}
		return tokenauth.Token{}, fmt.Errorf("workload identity token exchange: %w", err)
	}
	if resp.AccessToken == "" {
		return tokenauth.Token{}, errors.New("workload identity token exchange: the response carried no access_token")
	}
	if !strings.EqualFold(resp.TokenType, "Bearer") {
		return tokenauth.Token{}, fmt.Errorf("workload identity token exchange: unsupported token_type %q", resp.TokenType)
	}
	if resp.ExpiresIn <= 0 {
		return tokenauth.Token{}, fmt.Errorf("workload identity token exchange: invalid expires_in %d", resp.ExpiresIn)
	}
	lifetime := time.Duration(resp.ExpiresIn) * time.Second
	return tokenauth.Token{Value: resp.AccessToken, ExpiresAt: issuedAt.Add(lifetime)}, nil
}

func isInvalidGrant(body string) bool {
	var oauthErr struct {
		Error string `json:"error"`
	}
	return json.Unmarshal([]byte(body), &oauthErr) == nil && oauthErr.Error == "invalid_grant"
}

// workloadIdentityExchangeTimeout bounds one identity token exchange, which
// every caller waiting for a token shares.
const workloadIdentityExchangeTimeout = 10 * time.Second

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// bearerAuthDoer sets "Authorization: Bearer" from source on each request to
// host, so a long-running operation picks up refreshed tokens. Requests to
// other hosts (such as pre-signed storage URLs) and requests that already
// carry an Authorization header are passed through untouched.
type bearerAuthDoer struct {
	next   httpDoer
	source Credentials
	host   string
	// refused is the last token Baseten refused, passed to the next
	// AccessToken call so the credentials replace it.
	refused atomic.Pointer[string]
}

func newBearerAuthDoer(next httpDoer, source Credentials, baseURL string) (*bearerAuthDoer, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing BaseURL: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("BaseURL %q has no host", baseURL)
	}
	return &bearerAuthDoer{next: next, source: source, host: u.Host}, nil
}

func (d *bearerAuthDoer) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Host != d.host || req.Header.Get("Authorization") != "" {
		return d.next.Do(req)
	}
	var opts AccessTokenOptions
	if refused := d.refused.Swap(nil); refused != nil {
		opts.RefusedToken = *refused
	}
	token, err := d.source.AccessToken(req.Context(), opts)
	if err == nil && token.Token == "" {
		err = errors.New("the credentials returned an empty access token")
	}
	if err != nil {
		// Do closes the request body even on error, as http.Client does.
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	// Do must not modify the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+token.Token)
	resp, err := d.next.Do(req)
	// The request is not sent again, since it may have side effects; only the
	// next request gets a new token.
	if err == nil && refusedByBaseten(resp) {
		d.refused.Store(&token.Token)
	}
	return resp, err
}

// refusedByBaseten reports whether Baseten's auth layer refused the request's
// access token, which it signals with this challenge. A model's own error
// response cannot carry it, since its headers are not forwarded.
func refusedByBaseten(resp *http.Response) bool {
	if resp.StatusCode != http.StatusUnauthorized {
		return false
	}
	for _, challenge := range resp.Header.Values("WWW-Authenticate") {
		if strings.HasPrefix(challenge, "Bearer ") &&
			strings.Contains(challenge, `realm="baseten"`) &&
			strings.Contains(challenge, `error="invalid_token"`) {
			return true
		}
	}
	return false
}
