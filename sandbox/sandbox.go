package sandbox

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/sandboxapi"
)

// SandboxOptions configures one sandbox reached directly at its own URL.
type SandboxOptions struct {
	// Name of the sandbox.
	Name string

	// URL is the sandbox's execution API base URL, exactly as reported in
	// SandboxInfo.URL. Required; sandbox hosts must not be reconstructed from
	// the sandbox name.
	URL string

	// Token is a static bearer token for the sandbox APIs. The caller
	// manages its expiry. TokenProvider takes precedence.
	Token string

	// TokenProvider returns the bearer token for each request, instead of a
	// static token or one minted from an API key.
	TokenProvider TokenProvider

	// HTTPClient overrides the default client. When nil, http.DefaultClient
	// is used.
	HTTPClient HTTPDoer

	// Headers are sent on every request. The map is cloned at construction.
	Headers http.Header
}

// sandboxOptions is the internal constructor input: a record plus a token
// source shared with the SandboxesClient the record came from, so a token
// revoked here is dropped from that client's cache too.
type sandboxOptions struct {
	info       SandboxInfo
	tokens     *tokenSource
	httpClient HTTPDoer
	headers    http.Header
}

// Sandbox is one sandbox, reached directly at its own URL.
//
// Get one from SandboxesClient.Create, SandboxesClient.Get, or
// SandboxesClient.SandboxFromInfo, or construct one directly with NewSandbox
// when the URL and a token are already known.
type Sandbox struct {
	info    SandboxInfo
	options SandboxOptions
	process *SandboxProcess
	api     *sandboxapi.Client
}

// NewSandbox constructs a Sandbox without making a network request.
func NewSandbox(opts SandboxOptions) (*Sandbox, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("sandbox %s has no URL", opts.Name)
	}
	var tokens *tokenSource
	switch {
	case opts.TokenProvider != nil:
		if opts.Token != "" {
			return nil, fmt.Errorf("Token must be empty when TokenProvider is set")
		}
		tokens = &tokenSource{provider: opts.TokenProvider}
	case opts.Token != "":
		token := opts.Token
		tokens = &tokenSource{provider: func(context.Context) (string, error) {
			return token, nil
		}}
	default:
		// No token and no provider is the advanced opt-out of Authorization.
		tokens = &tokenSource{}
	}
	return newSandbox(sandboxOptions{
		info:       SandboxInfo{Name: opts.Name, URL: opts.URL},
		tokens:     tokens,
		httpClient: opts.HTTPClient,
		headers:    opts.Headers,
	}), nil
}

func newSandbox(opts sandboxOptions) *Sandbox {
	httpClient := opts.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	headers := opts.headers.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	client.ApplyUserAgentHeader(headers)
	url := strings.TrimRight(opts.info.URL, "/")
	return &Sandbox{
		info:    opts.info,
		options: SandboxOptions{Name: opts.info.Name, URL: url},
		api: &sandboxapi.Client{
			BaseURL:    url,
			HTTPClient: &tokenAuthClient{inner: httpClient, tokens: opts.tokens},
			Headers:    headers,
		},
	}
}

// Info is the sandbox's record as of when this Sandbox was built. It does not
// refresh.
func (s *Sandbox) Info() SandboxInfo {
	return s.info
}

// Name of the sandbox.
func (s *Sandbox) Name() string {
	return s.info.Name
}

// URL is the sandbox's execution API base URL.
func (s *Sandbox) URL() string {
	return s.info.URL
}

// Process runs and inspects processes in the sandbox.
func (s *Sandbox) Process() *SandboxProcess {
	if s.process == nil {
		s.process = &SandboxProcess{api: s.api}
	}
	return s.process
}

// RawAPI returns the generated execution client for this sandbox, for
// operations this package does not wrap. Its generated surface may change
// between versions. Raw responses belong to the caller, who must close Body.
func (s *Sandbox) RawAPI() *sandboxapi.Client {
	return s.api
}

// Options returns the options this sandbox was constructed with.
func (s *Sandbox) Options() SandboxOptions {
	return s.options
}
