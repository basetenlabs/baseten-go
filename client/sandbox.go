package client

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/basetenlabs/baseten-go/client/sandboxapi"
)

// SandboxClientOptions configures a client for one sandbox's execution API.
type SandboxClientOptions struct {
	// Token is a short-lived sandbox bearer token obtained through the management
	// API. Required unless DeferAuth is true. The caller manages token expiry.
	Token string

	// BaseURL is the sandbox URL returned by the management API. Required;
	// sandbox hosts must not be reconstructed from the sandbox name.
	BaseURL string

	// HTTPClient overrides http.DefaultClient. It must honor request contexts
	// and the http.Client contract for closing request bodies.
	HTTPClient interface {
		Do(*http.Request) (*http.Response, error)
	}

	// DeferAuth leaves authentication to HTTPClient or Headers. It is mutually
	// exclusive with Token, matching the other client constructors.
	DeferAuth bool

	// Headers are copied at construction and sent with every request.
	Headers http.Header
}

// SandboxClient provides direct access to a sandbox's execution API. It does
// not exchange tokens, poll readiness, or retry operations.
type SandboxClient struct {
	api *sandboxapi.Client
}

// NewSandboxClient constructs a client without making a network request.
func NewSandboxClient(opts SandboxClientOptions) (*SandboxClient, error) {
	if opts.DeferAuth && opts.Token != "" {
		return nil, fmt.Errorf("Token and DeferAuth are mutually exclusive")
	}
	if opts.Token == "" && !opts.DeferAuth {
		return nil, fmt.Errorf("Token is required")
	}
	baseURL := strings.TrimRight(opts.BaseURL, "/")
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("BaseURL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
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
	return &SandboxClient{api: &sandboxapi.Client{
		BaseURL: baseURL, HTTPClient: httpClient, Headers: headers,
	}}, nil
}

// API returns the generated execution client. Its generated surface may change
// between versions. Raw responses belong to the caller, who must close Body.
func (c *SandboxClient) API() *sandboxapi.Client {
	return c.api
}
