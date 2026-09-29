package client

import (
	"net/http"

	"github.com/basetenlabs/baseten-go/client/sandboxapi"
)

// SandboxClientOptions configures a client for one sandbox's execution API.
type SandboxClientOptions struct {
	// Token is a short-lived sandbox bearer token obtained through the management
	// API. An empty token opts out of setting Authorization. The caller manages
	// token expiry.
	Token string

	// BaseURL is the sandbox URL returned by the management API. Required;
	// sandbox hosts must not be reconstructed from the sandbox name.
	BaseURL string

	// HTTPClient overrides http.DefaultClient. It must honor request contexts
	// and the http.Client contract for closing request bodies.
	HTTPClient interface {
		Do(*http.Request) (*http.Response, error)
	}

	// Headers are copied at construction and sent with every request.
	Headers http.Header
}

// SandboxClient provides direct access to a sandbox's execution API. It does
// not exchange tokens, poll readiness, or retry operations.
type SandboxClient struct {
	api     *sandboxapi.Client
	options SandboxClientOptions
}

// NewSandboxClient constructs a client without making a network request.
func NewSandboxClient(opts SandboxClientOptions) (*SandboxClient, error) {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	headers := opts.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	if opts.Token != "" {
		headers.Set("Authorization", "Bearer "+opts.Token)
	}
	ApplyUserAgentHeader(headers)
	return &SandboxClient{options: opts, api: &sandboxapi.Client{
		BaseURL: opts.BaseURL, HTTPClient: httpClient, Headers: headers,
	}}, nil
}

// API returns the generated execution client. Its generated surface may change
// between versions. Raw responses belong to the caller, who must close Body.
func (c *SandboxClient) API() *sandboxapi.Client {
	return c.api
}

// Options returns the options this client was constructed with.
func (c *SandboxClient) Options() SandboxClientOptions {
	return c.options
}
