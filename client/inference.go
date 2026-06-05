package client

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/basetenlabs/baseten-go/client/inferenceapi"
)

// InferenceClientOptions configures an InferenceClient.
type InferenceClientOptions struct {
	// APIKey is the Baseten API key used for authentication. Required
	// unless DeferAuth is true.
	APIKey string

	// ModelID is the model to target. Mutually exclusive with ChainID.
	ModelID string

	// ChainID is the chain to target. Mutually exclusive with ModelID.
	ChainID string

	// Environment is the optional regional environment slug (e.g. "prod-us")
	// that selects a regional deployment. Leave empty for the default region.
	// See https://docs.baseten.co/deployment/environments#regional-environments.
	Environment string

	// BaseURL overrides the computed inference API base URL. Mutually exclusive
	// with ModelID, ChainID, and Environment.
	BaseURL string

	// HTTPClient overrides the default HTTP client. If nil, http.DefaultClient
	// is used.
	HTTPClient interface {
		Do(*http.Request) (*http.Response, error)
	}

	// DeferAuth, when true, skips the APIKey requirement and does not set
	// any Authorization header. The caller is expected to provide an
	// HTTPClient that injects the appropriate auth header.
	DeferAuth bool

	// Headers are added to every request. The map is cloned at construction,
	// so later mutations by the caller do not affect the live client.
	Headers http.Header
}

// InferenceClient provides access to the Baseten inference API for a specific
// model or chain.
type InferenceClient struct {
	api *inferenceapi.Client
}

// InferenceClientHost computes the inference API host (no scheme) from the
// given options. Exactly one of modelID or chainID must be non-empty.
// environment is the optional regional environment slug (e.g. "prod-us")
// that selects a regional deployment; leave empty for the default region.
// See https://docs.baseten.co/deployment/environments#regional-environments.
// rootApiHost defaults to "api.baseten.co" when empty.
func InferenceClientHost(modelID, chainID, environment, rootApiHost string) (string, error) {
	if modelID == "" && chainID == "" {
		return "", fmt.Errorf("one of model ID or chain ID must be set")
	}
	if modelID != "" && chainID != "" {
		return "", fmt.Errorf("model ID and chain ID are mutually exclusive")
	}
	if rootApiHost == "" {
		rootApiHost = "api.baseten.co"
	}
	entity := "model-" + modelID
	if chainID != "" {
		entity = "chain-" + chainID
	}
	if environment != "" {
		entity += "-" + environment
	}
	return entity + "." + rootApiHost, nil
}

// InferenceClientDefaultBaseURL computes the default inference API base URL
// from the given options. Exactly one of modelID or chainID must be non-empty.
func InferenceClientDefaultBaseURL(modelID, chainID, environment string) (string, error) {
	host, err := InferenceClientHost(modelID, chainID, environment, "")
	if err != nil {
		return "", err
	}
	return "https://" + host, nil
}

// NewInferenceClient creates a new InferenceClient.
func NewInferenceClient(opts InferenceClientOptions) (*InferenceClient, error) {
	if opts.DeferAuth && opts.APIKey != "" {
		return nil, fmt.Errorf("APIKey and DeferAuth are mutually exclusive")
	}
	if opts.APIKey == "" && !opts.DeferAuth {
		return nil, fmt.Errorf("APIKey is required")
	}

	var baseURL string
	if opts.BaseURL != "" {
		if opts.ModelID != "" || opts.ChainID != "" || opts.Environment != "" {
			return nil, fmt.Errorf("BaseURL is mutually exclusive with ModelID, ChainID, and Environment")
		}
		baseURL = strings.TrimRight(opts.BaseURL, "/")
	} else {
		var err error
		baseURL, err = InferenceClientDefaultBaseURL(opts.ModelID, opts.ChainID, opts.Environment)
		if err != nil {
			return nil, err
		}
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	headers := opts.Headers.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	if opts.APIKey != "" {
		headers.Set("Authorization", "Bearer "+opts.APIKey)
	}
	ApplyUserAgentHeader(headers)

	return &InferenceClient{api: &inferenceapi.Client{
		BaseURL:    baseURL,
		HTTPClient: httpClient,
		Headers:    headers,
	}}, nil
}

// API returns the underlying generated inference API client. The generated
// API surface is not covered by Go compatibility guarantees and may change
// between versions.
func (c *InferenceClient) API() *inferenceapi.Client {
	return c.api
}
