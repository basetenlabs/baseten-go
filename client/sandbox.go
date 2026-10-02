package client

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/basetenlabs/baseten-go/client/sandboxapi"
)

// SandboxClientOptions configures a SandboxClient.
type SandboxClientOptions struct {
	// Token is the sandbox token used for authentication, minted from an API
	// key through the management API. Required unless DeferAuth is true.
	Token string

	// BaseURL is the sandbox URL returned by the management API. Required.
	BaseURL string

	// HTTPClient overrides the default HTTP client. If nil, http.DefaultClient
	// is used.
	HTTPClient interface {
		Do(*http.Request) (*http.Response, error)
	}

	// DeferAuth, when true, skips the Token requirement and does not set any
	// Authorization header. The caller is expected to provide an HTTPClient
	// that injects the appropriate auth header.
	DeferAuth bool

	// Headers are added to every request. The map is cloned at construction,
	// so later mutations by the caller do not affect the live client.
	Headers http.Header
}

// SandboxClient provides access to the Baseten sandbox API for a specific
// sandbox.
type SandboxClient struct {
	api *sandboxapi.Client
}

// NewSandboxClient creates a new SandboxClient.
func NewSandboxClient(opts SandboxClientOptions) (*SandboxClient, error) {
	if opts.DeferAuth && opts.Token != "" {
		return nil, fmt.Errorf("Token and DeferAuth are mutually exclusive")
	}
	if opts.Token == "" && !opts.DeferAuth {
		return nil, fmt.Errorf("Token is required")
	}

	baseURL := strings.TrimRight(opts.BaseURL, "/")
	if baseURL == "" {
		return nil, fmt.Errorf("BaseURL is required")
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	headers := opts.Headers.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	if opts.Token != "" {
		headers.Set("Authorization", "Bearer "+opts.Token)
	}
	ApplyUserAgentHeader(headers)

	return &SandboxClient{api: &sandboxapi.Client{
		BaseURL:    baseURL,
		HTTPClient: httpClient,
		Headers:    headers,
	}}, nil
}

// API returns the underlying generated sandbox API client. The generated
// API surface is not covered by Go compatibility guarantees and may change
// between versions. The caller must close the body of a response returned by
// a Raw method.
func (c *SandboxClient) API() *sandboxapi.Client {
	return c.api
}
