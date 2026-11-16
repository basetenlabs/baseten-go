package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

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

// ErrInvalidGrant is returned (wrapped) when Baseten refuses a workload
// identity token exchange. The server gives no reason: the trust rule may not
// exist, may not match the identity token's issuer or claims, or the identity
// token may be expired or have the wrong audience.
var ErrInvalidGrant = errors.New("invalid_grant")

// AccessToken is a short-lived Baseten access token, sent as
// "Authorization: Bearer <Token>".
type AccessToken struct {
	Token     string
	ExpiresAt time.Time
}

// Credentials supplies a current Baseten access token. A client
// configured with one calls it before every request, so the source decides
// when to refresh.
type Credentials interface {
	AccessToken(ctx context.Context) (AccessToken, error)
}

// AccessTokenInvalidator is implemented by a [Credentials] that caches
// tokens. When Baseten refuses a token before it expires, for example because
// it was revoked, a client drops it with InvalidateAccessToken, asks the source
// for a new one, and sends the request again. Clients resend only for sources
// that implement this.
type AccessTokenInvalidator interface {
	// InvalidateAccessToken drops token if it is still the cached one.
	InvalidateAccessToken(token string)
}

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
	IdentityToken func(ctx context.Context) (string, error)

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
	opts        WorkloadIdentityCredentialsOptions
	exchangeURL string
	httpClient  httpDoer
	cache       *tokenauth.Cache

	// now is replaced in tests.
	now func() time.Time
}

var (
	_ Credentials            = (*WorkloadIdentityCredentials)(nil)
	_ AccessTokenInvalidator = (*WorkloadIdentityCredentials)(nil)
)

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
	c := &WorkloadIdentityCredentials{
		opts:        opts,
		exchangeURL: baseURL + "/v1/auth/token",
		httpClient:  httpClient,
		now:         time.Now,
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
func (c *WorkloadIdentityCredentials) AccessToken(ctx context.Context) (AccessToken, error) {
	token, err := c.cache.Token(ctx)
	if err != nil {
		return AccessToken{}, err
	}
	return AccessToken{Token: token.Value, ExpiresAt: token.ExpiresAt}, nil
}

// InvalidateAccessToken drops token if it is still the cached one, so the next
// call exchanges the identity token again.
func (c *WorkloadIdentityCredentials) InvalidateAccessToken(token string) {
	c.cache.Invalidate(token)
}

func (c *WorkloadIdentityCredentials) identityToken(ctx context.Context) (string, error) {
	if c.opts.IdentityToken != nil {
		token, err := c.opts.IdentityToken(ctx)
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

type tokenExchangeRequest struct {
	GrantType        string `json:"grant_type"`
	SubjectToken     string `json:"subject_token"`
	SubjectTokenType string `json:"subject_token_type"`
	Audience         string `json:"audience"`
}

type tokenExchangeResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Error       string `json:"error"`
}

// exchange swaps the identity token for a new access token.
func (c *WorkloadIdentityCredentials) exchange(ctx context.Context) (tokenauth.Token, error) {
	assertion, err := c.identityToken(ctx)
	if err != nil {
		return tokenauth.Token{}, err
	}
	body, err := json.Marshal(tokenExchangeRequest{
		GrantType:        tokenExchangeGrantType,
		SubjectToken:     assertion,
		SubjectTokenType: jwtTokenType,
		Audience:         c.opts.TrustRuleID,
	})
	if err != nil {
		return tokenauth.Token{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.exchangeURL, bytes.NewReader(body))
	if err != nil {
		return tokenauth.Token{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	ApplyUserAgentHeader(req.Header)

	// Taken before sending, so the computed expiry errs early.
	issuedAt := c.now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return tokenauth.Token{}, fmt.Errorf("workload identity token exchange: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenauth.Token{}, fmt.Errorf("workload identity token exchange: reading response: %w", err)
	}

	var decoded tokenExchangeResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return tokenauth.Token{}, fmt.Errorf(
			"workload identity token exchange: HTTP %d with a non-JSON response: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if resp.StatusCode != http.StatusOK {
		if decoded.Error == "invalid_grant" {
			return tokenauth.Token{}, fmt.Errorf(
				"workload identity token exchange refused for trust rule %s: %w "+
					"(the trust rule may not exist or may not match the identity token's issuer, audience, or claims, "+
					"or the identity token may have expired)",
				c.opts.TrustRuleID, ErrInvalidGrant)
		}
		return tokenauth.Token{}, fmt.Errorf("workload identity token exchange: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if decoded.AccessToken == "" {
		return tokenauth.Token{}, errors.New("workload identity token exchange: the response carried no access_token")
	}
	if !strings.EqualFold(decoded.TokenType, "Bearer") {
		return tokenauth.Token{}, fmt.Errorf("workload identity token exchange: unsupported token_type %q", decoded.TokenType)
	}
	if decoded.ExpiresIn <= 0 {
		return tokenauth.Token{}, fmt.Errorf("workload identity token exchange: invalid expires_in %d", decoded.ExpiresIn)
	}
	lifetime := time.Duration(decoded.ExpiresIn) * time.Second
	return tokenauth.Token{Value: decoded.AccessToken, ExpiresAt: issuedAt.Add(lifetime)}, nil
}

// workloadIdentityExchangeTimeout bounds one identity token exchange, which
// every caller waiting for a token shares.
const workloadIdentityExchangeTimeout = 10 * time.Second

// accessTokenResendDelay is how long a client waits before its second resend
// of a request whose access token was refused. A variable so tests can shorten
// it.
var accessTokenResendDelay = time.Second

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
	invalidator, resend := d.source.(AccessTokenInvalidator)
	get := func(ctx context.Context, _ string) (string, error) {
		token, err := d.source.AccessToken(ctx)
		if err != nil {
			return "", err
		}
		if token.Token == "" {
			return "", errors.New("the access token source returned an empty token")
		}
		return token.Token, nil
	}
	if !resend {
		// Without a way to drop the refused token, the source would return it
		// again, so the request is sent once.
		token, err := get(req.Context(), "")
		if err != nil {
			if req.Body != nil {
				req.Body.Close()
			}
			return nil, err
		}
		// Do must not modify the caller's request.
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+token)
		return d.next.Do(req)
	}
	return tokenauth.Do(d.next, req, get, invalidator.InvalidateAccessToken, accessTokenResendDelay)
}
