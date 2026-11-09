package client

import (
	"net/http"
	"strings"

	"github.com/basetenlabs/baseten-go/client/managementapi"
)

// ManagementClientOptions configures a ManagementClient.
type ManagementClientOptions struct {
	// APIKey is the Baseten API key used for authentication. Exactly one of
	// APIKey, Credentials, and DeferAuth is required.
	APIKey string

	// Credentials supplies short-lived access tokens instead of an API key,
	// for example [WorkloadIdentityCredentials]. A token is fetched for every
	// request, so long-running operations survive token refresh.
	Credentials AccessTokenSource

	// BaseURL overrides the default management API base URL. If empty,
	// "https://api.baseten.co" is used.
	BaseURL string

	// HTTPClient overrides the default HTTP client. If nil, http.DefaultClient
	// is used.
	//
	// Volume transfers use this client too, and lean on http.Client's
	// contract that Do closes the request body even on error: an
	// implementation that never closes it hangs an upload rather than
	// leaking the buffer behind it. Any real transport already complies.
	HTTPClient interface {
		Do(*http.Request) (*http.Response, error)
	}

	// DeferAuth, when true, skips the APIKey requirement and does not set
	// any Authorization header. The caller is expected to provide an
	// HTTPClient that injects the appropriate auth header (e.g. via a
	// custom RoundTripper). This is intended for CLI use where auth may
	// come from OAuth tokens or other credential sources.
	DeferAuth bool

	// Headers are added to every request. The map is cloned at construction,
	// so later mutations by the caller do not affect the live client.
	Headers http.Header
}

// ManagementClient provides access to the Baseten management API.
type ManagementClient struct {
	api *managementapi.Client
}

// NewManagementClient creates a new ManagementClient.
func NewManagementClient(opts ManagementClientOptions) (*ManagementClient, error) {
	if err := validateAuthOptions(opts.APIKey, opts.Credentials, opts.DeferAuth, opts.Headers); err != nil {
		return nil, err
	}

	baseURL := strings.TrimRight(opts.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.baseten.co"
	}

	var httpClient httpDoer = opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if opts.Credentials != nil {
		doer, err := newBearerAuthDoer(httpClient, opts.Credentials, baseURL)
		if err != nil {
			return nil, err
		}
		httpClient = doer
	}

	headers := opts.Headers.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	if opts.APIKey != "" {
		headers.Set("Authorization", "Bearer "+opts.APIKey)
	}
	ApplyUserAgentHeader(headers)

	return &ManagementClient{api: &managementapi.Client{
		BaseURL:    baseURL,
		HTTPClient: httpClient,
		Headers:    headers,
	}}, nil
}

// API returns the underlying generated management API client. The generated
// API surface is not covered by Go compatibility guarantees and may change
// between versions.
func (c *ManagementClient) API() *managementapi.Client {
	return c.api
}
