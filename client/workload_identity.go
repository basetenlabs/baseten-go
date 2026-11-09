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
)

// jwtBearerGrantType is the RFC 7523 grant the token exchange accepts.
const jwtBearerGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// defaultWorkloadIdentityRefreshLeeway is how long before expiry a cached
// access token is replaced. Capped at half the token's lifetime so a
// short-lived token is still reused.
const defaultWorkloadIdentityRefreshLeeway = 2 * time.Minute

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

// AccessTokenSource supplies a current Baseten access token. A client
// configured with one calls it before every request, so the source decides
// when to refresh.
type AccessTokenSource interface {
	AccessToken(ctx context.Context) (AccessToken, error)
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

	// HTTPClient sends the exchange. If nil, http.DefaultClient is used.
	HTTPClient interface {
		Do(*http.Request) (*http.Response, error)
	}

	// RefreshLeeway is how long before expiry the access token is replaced.
	// If zero, two minutes is used. Either way it is capped at half the
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

	// sem is a one-slot lock that, unlike sync.Mutex, lets a waiter give up
	// when its context ends while another caller's exchange is in flight.
	sem       chan struct{}
	current   AccessToken
	refreshAt time.Time

	// now is replaced in tests.
	now func() time.Time
}

var _ AccessTokenSource = (*WorkloadIdentityCredentials)(nil)

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
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &WorkloadIdentityCredentials{
		opts:        opts,
		exchangeURL: baseURL + "/v1/auth/token",
		httpClient:  httpClient,
		sem:         make(chan struct{}, 1),
		now:         time.Now,
	}, nil
}

// AccessToken returns a current access token, exchanging the identity token
// for a new one when none is cached or the cached one is within the refresh
// leeway of expiry.
func (c *WorkloadIdentityCredentials) AccessToken(ctx context.Context) (AccessToken, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return AccessToken{}, ctx.Err()
	}
	defer func() { <-c.sem }()

	if c.current.Token != "" && c.now().Before(c.refreshAt) {
		return c.current, nil
	}
	// Bounded because every other caller waits on sem behind this exchange.
	exchangeCtx, cancel := context.WithTimeout(ctx, workloadIdentityExchangeTimeout)
	defer cancel()
	token, lifetime, err := c.exchange(exchangeCtx)
	if err != nil {
		return AccessToken{}, err
	}
	leeway := min(c.opts.RefreshLeeway, lifetime/2)
	c.current = token
	c.refreshAt = token.ExpiresAt.Add(-leeway)
	return c.current, nil
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
	GrantType   string `json:"grant_type"`
	Assertion   string `json:"assertion"`
	TrustRuleID string `json:"trust_rule_id"`
}

type tokenExchangeResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Error       string `json:"error"`
}

// exchange returns the new token and its lifetime.
func (c *WorkloadIdentityCredentials) exchange(ctx context.Context) (AccessToken, time.Duration, error) {
	assertion, err := c.identityToken(ctx)
	if err != nil {
		return AccessToken{}, 0, err
	}
	body, err := json.Marshal(tokenExchangeRequest{
		GrantType:   jwtBearerGrantType,
		Assertion:   assertion,
		TrustRuleID: c.opts.TrustRuleID,
	})
	if err != nil {
		return AccessToken{}, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.exchangeURL, bytes.NewReader(body))
	if err != nil {
		return AccessToken{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	ApplyUserAgentHeader(req.Header)

	// Taken before sending, so the computed expiry errs early.
	issuedAt := c.now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return AccessToken{}, 0, fmt.Errorf("workload identity token exchange: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return AccessToken{}, 0, fmt.Errorf("workload identity token exchange: reading response: %w", err)
	}

	var decoded tokenExchangeResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return AccessToken{}, 0, fmt.Errorf(
			"workload identity token exchange: HTTP %d with a non-JSON response: %s",
			resp.StatusCode, truncateForError(respBody))
	}
	if resp.StatusCode != http.StatusOK {
		if decoded.Error == "invalid_grant" {
			return AccessToken{}, 0, fmt.Errorf(
				"workload identity token exchange refused for trust rule %s: %w "+
					"(the trust rule may not exist or may not match the identity token's issuer, audience, or claims, "+
					"or the identity token may have expired)",
				c.opts.TrustRuleID, ErrInvalidGrant)
		}
		return AccessToken{}, 0, fmt.Errorf("workload identity token exchange: HTTP %d: %s",
			resp.StatusCode, truncateForError(respBody))
	}
	if decoded.AccessToken == "" {
		return AccessToken{}, 0, errors.New("workload identity token exchange: the response carried no access_token")
	}
	if !strings.EqualFold(decoded.TokenType, "Bearer") {
		return AccessToken{}, 0, fmt.Errorf("workload identity token exchange: unsupported token_type %q", decoded.TokenType)
	}
	if decoded.ExpiresIn <= 0 {
		return AccessToken{}, 0, fmt.Errorf("workload identity token exchange: invalid expires_in %d", decoded.ExpiresIn)
	}
	lifetime := time.Duration(decoded.ExpiresIn) * time.Second
	return AccessToken{Token: decoded.AccessToken, ExpiresAt: issuedAt.Add(lifetime)}, lifetime, nil
}

func truncateForError(body []byte) string {
	const maxLen = 512
	s := strings.TrimSpace(string(body))
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

// workloadIdentityExchangeTimeout bounds one identity token exchange.
const workloadIdentityExchangeTimeout = 10 * time.Second

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// bearerAuthDoer sets "Authorization: Bearer" from source on each request to
// host, so a long-running operation picks up refreshed tokens. Requests to
// other hosts (such as pre-signed storage URLs) and requests that already
// carry an Authorization header are passed through untouched. When next is an
// *http.Client, redirects to any other host drop the Authorization header;
// net/http alone would still send it to subdomains and other ports of host.
// Any other doer must handle redirects itself.
type bearerAuthDoer struct {
	next   httpDoer
	source AccessTokenSource
	host   string
}

func newBearerAuthDoer(next httpDoer, source AccessTokenSource, baseURL string) (*bearerAuthDoer, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing BaseURL: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("BaseURL %q has no host", baseURL)
	}
	if hc, ok := next.(*http.Client); ok {
		next = withExactHostRedirectAuth(hc, u.Host)
	}
	return &bearerAuthDoer{next: next, source: source, host: u.Host}, nil
}

// withExactHostRedirectAuth returns a copy of hc whose redirects keep the
// Authorization header only while they stay on host. hc is not modified.
func withExactHostRedirectAuth(hc *http.Client, host string) *http.Client {
	clone := *hc
	checkRedirect := hc.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != host {
			req.Header.Del("Authorization")
		}
		if checkRedirect != nil {
			return checkRedirect(req, via)
		}
		// net/http's default policy for a nil CheckRedirect.
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &clone
}

func (d *bearerAuthDoer) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Host != d.host || req.Header.Get("Authorization") != "" {
		return d.next.Do(req)
	}
	token, err := d.source.AccessToken(req.Context())
	if err != nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	// Do must not modify the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+token.Token)
	return d.next.Do(req)
}
