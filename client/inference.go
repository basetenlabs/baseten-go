package client

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/basetenlabs/baseten-go/client/inferenceapi"
)

// InferenceClientOptions configures an InferenceClient.
type InferenceClientOptions struct {
	// APIKey is the Baseten API key used for authentication. Required.
	APIKey string

	// ModelID is the model to target. Mutually exclusive with ChainID.
	ModelID string

	// ChainID is the chain to target. Mutually exclusive with ModelID.
	ChainID string

	// Environment is the regional environment name (e.g. "prod-us"). When set,
	// the environment is embedded in the hostname rather than the path.
	Environment string

	// BaseURL overrides the computed inference API base URL. Mutually exclusive
	// with ModelID, ChainID, and Environment.
	BaseURL string

	// HTTPClient overrides the default HTTP client. If nil, http.DefaultClient
	// is used.
	HTTPClient interface {
		Do(*http.Request) (*http.Response, error)
	}
}

// InferenceClient provides access to the Baseten inference API for a specific
// model or chain.
type InferenceClient struct {
	api *inferenceapi.Client
}

// InferenceClientDefaultBaseURL computes the default inference API base URL from the given
// options. Exactly one of modelID or chainID must be non-empty.
func InferenceClientDefaultBaseURL(modelID, chainID, environment string) (string, error) {
	if modelID == "" && chainID == "" {
		return "", fmt.Errorf("one of model ID or chain ID must be set")
	}
	if modelID != "" && chainID != "" {
		return "", fmt.Errorf("model ID and chain ID are mutually exclusive")
	}
	if modelID != "" {
		if environment != "" {
			return fmt.Sprintf("https://model-%s-%s.api.baseten.co", modelID, environment), nil
		}
		return fmt.Sprintf("https://model-%s.api.baseten.co", modelID), nil
	}
	if environment != "" {
		return fmt.Sprintf("https://chain-%s-%s.api.baseten.co", chainID, environment), nil
	}
	return fmt.Sprintf("https://chain-%s.api.baseten.co", chainID), nil
}

// NewInferenceClient creates a new InferenceClient.
func NewInferenceClient(opts InferenceClientOptions) (*InferenceClient, error) {
	if opts.APIKey == "" {
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

	return &InferenceClient{api: &inferenceapi.Client{
		BaseURL:    baseURL,
		HTTPClient: httpClient,
		Headers:    http.Header{"Authorization": {"Api-Key " + opts.APIKey}},
	}}, nil
}

// API returns the underlying generated inference API client. The generated
// API surface is not covered by Go compatibility guarantees and may change
// between versions.
func (c *InferenceClient) API() *inferenceapi.Client {
	return c.api
}
