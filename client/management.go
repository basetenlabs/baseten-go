package client

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/basetenlabs/baseten-go/client/managementapi"
)

// ManagementClientOptions configures a ManagementClient.
type ManagementClientOptions struct {
	// APIKey is the Baseten API key used for authentication. Required.
	APIKey string

	// BaseURL overrides the default management API base URL. If empty,
	// "https://api.baseten.co" is used.
	BaseURL string

	// HTTPClient overrides the default HTTP client. If nil, http.DefaultClient
	// is used.
	HTTPClient interface {
		Do(*http.Request) (*http.Response, error)
	}
}

// ManagementClient provides access to the Baseten management API.
type ManagementClient struct {
	api *managementapi.Client
}

// NewManagementClient creates a new ManagementClient.
func NewManagementClient(opts ManagementClientOptions) (*ManagementClient, error) {
	if opts.APIKey == "" {
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

	return &ManagementClient{api: &managementapi.Client{
		BaseURL:    baseURL,
		HTTPClient: httpClient,
		Headers:    http.Header{"Authorization": {"Api-Key " + opts.APIKey}},
	}}, nil
}

// API returns the underlying generated management API client. The generated
// API surface is not covered by Go compatibility guarantees and may change
// between versions.
func (c *ManagementClient) API() *managementapi.Client {
	return c.api
}
