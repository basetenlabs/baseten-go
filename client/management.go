package client

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/basetenlabs/baseten-go/client/managementapi"
)

// ManagementClientOptions configures a ManagementClient.
type ManagementClientOptions struct {
	// APIKey is the Baseten API key used for authentication. Required
	// unless DeferAuth is true.
	APIKey string

	// BaseURL overrides the default management API base URL. If empty,
	// "https://api.baseten.co" is used.
	BaseURL string

	// HTTPClient overrides the default HTTP client. If nil, http.DefaultClient
	// is used.
	HTTPClient interface {
		Do(*http.Request) (*http.Response, error)
	}

	// DeferAuth, when true, skips the APIKey requirement and does not set
	// any Authorization header. The caller is expected to provide an
	// HTTPClient that injects the appropriate auth header (e.g. via a
	// custom RoundTripper). This is intended for CLI use where auth may
	// come from OAuth tokens or other credential sources.
	DeferAuth bool
}

// ManagementClient provides access to the Baseten management API.
type ManagementClient struct {
	api *managementapi.Client
}

// NewManagementClient creates a new ManagementClient.
func NewManagementClient(opts ManagementClientOptions) (*ManagementClient, error) {
	if opts.DeferAuth && opts.APIKey != "" {
		return nil, fmt.Errorf("APIKey and DeferAuth are mutually exclusive")
	}
	if opts.APIKey == "" && !opts.DeferAuth {
		return nil, fmt.Errorf("APIKey is required")
	}

	baseURL := strings.TrimRight(opts.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.baseten.co"
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	var headers http.Header
	if opts.APIKey != "" {
		headers = http.Header{"Authorization": {"Api-Key " + opts.APIKey}}
	}

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
