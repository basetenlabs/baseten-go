package sandbox

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"strings"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/managementapi"
)

// ClientOptions configures a [Client].
type ClientOptions struct {
	// APIKey is the Baseten API key used for authentication. Required unless
	// one of the advanced ManagementClient or TokenProvider options is set.
	APIKey string

	// TeamID is the team to act in. Empty uses the caller's only team, and
	// is an error for callers in more than one team.
	TeamID string

	// BaseURL overrides the default management API base URL. If empty,
	// "https://api.baseten.co" is used.
	BaseURL string

	// HTTPClient overrides the default HTTP client. If nil,
	// http.DefaultClient is used.
	HTTPClient interface {
		Do(*http.Request) (*http.Response, error)
	}

	// Headers are added to every request. The map is cloned at construction,
	// so later mutations by the caller do not affect the live client.
	Headers http.Header

	// Retries sets how many times calls to a sandbox are retried on dropped
	// connections and gateway errors.
	Retries RetryOptions

	// ManagementClient is an advanced alternative to APIKey: the management
	// client that sandbox tokens are requested through, with its own base
	// URL and authentication. For callers that authenticate the management
	// API other than with an API key. APIKey and TokenProvider must then be
	// empty.
	ManagementClient *client.ManagementClient

	// TokenProvider is an advanced alternative to APIKey: it returns the
	// bearer token for each request, for callers that obtain sandbox tokens
	// themselves. APIKey and ManagementClient must then be empty.
	TokenProvider TokenProvider
}

// Client works with Baseten sandboxes: creating, finding, updating, and
// deleting them, and getting a [Sandbox] to work in one.
type Client struct {
	options    ClientOptions
	tokens     *tokenSource
	httpClient httpDoer
	headers    http.Header
	retries    RetryOptions
	api        *managementapi.Client
	images     *ImageClient
}

// NewClient creates a Client without making a network request.
func NewClient(opts ClientOptions) (*Client, error) {
	credentials := 0
	for _, set := range []bool{opts.APIKey != "", opts.ManagementClient != nil, opts.TokenProvider != nil} {
		if set {
			credentials++
		}
	}
	if credentials == 0 {
		return nil, errors.New("APIKey is required")
	} else if credentials > 1 {
		return nil, errors.New("only one of APIKey, ManagementClient, and TokenProvider may be set")
	}

	baseURL := strings.TrimRight(opts.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.baseten.co"
	}

	var httpClient httpDoer = http.DefaultClient
	if opts.HTTPClient != nil {
		httpClient = opts.HTTPClient
	}

	headers := opts.Headers.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	client.ApplyUserAgentHeader(headers)

	tokens := &tokenSource{provider: opts.TokenProvider, management: opts.ManagementClient}
	if opts.APIKey != "" {
		management, err := client.NewManagementClient(client.ManagementClientOptions{
			APIKey:     opts.APIKey,
			BaseURL:    baseURL,
			HTTPClient: httpClient,
			Headers:    opts.Headers,
		})
		if err != nil {
			return nil, err
		}
		tokens.management = management
	}

	c := &Client{
		options:    opts,
		tokens:     tokens,
		httpClient: httpClient,
		headers:    headers,
		retries:    opts.Retries.resolved(),
		api: &managementapi.Client{
			BaseURL:    baseURL,
			HTTPClient: &authenticatedClient{inner: httpClient, tokens: tokens},
			Headers:    headers,
		},
	}
	c.images = &ImageClient{client: c}
	return c, nil
}

// API returns the underlying generated management API client, authenticated
// for sandboxes, for operations this package does not wrap. The generated API
// surface is not covered by Go compatibility guarantees and may change
// between versions.
func (c *Client) API() *managementapi.Client {
	return c.api
}

// CreateOptions are the options for [Client.Create]. Unset fields are omitted,
// so the server applies its defaults.
type CreateOptions struct {
	// Name is the sandbox's unique name. The server assigns one when empty.
	Name string

	// CreateIfNotExists returns the existing sandbox with this name instead
	// of failing when one exists. Requires Name.
	CreateIfNotExists bool

	// Image is the image reference, including its tag. Defaults to
	// "baseten/base-image:latest".
	Image string

	// Memory is the memory allocation in megabytes, which also sets the CPU
	// allocation. Defaults to 4096.
	Memory int

	// Region is where the sandbox runs. Empty uses the closest region.
	Region string

	// Envs are the environment variables injected into the sandbox, by name.
	Envs map[string]EnvValue

	// Labels are the key-value pairs for organizing and filtering.
	Labels map[string]string

	// ExternalID is a caller-owned identifier for external lookups.
	ExternalID string

	// Lifecycle is when the sandbox expires.
	Lifecycle *Lifecycle

	// Ports are the ports the sandbox exposes.
	Ports []Port

	// Network is the sandbox's network configuration. It cannot be updated
	// later.
	Network *Network
}

// CreateResult is a created sandbox: a [Sandbox] to work in, plus its record
// as of creation.
type CreateResult struct {
	*Sandbox

	// Info is the sandbox's record as of creation. The sandbox becomes ready
	// asynchronously, so its status is usually still deploying.
	Info Info
}

// Create creates a sandbox.
func (c *Client) Create(ctx context.Context, opts CreateOptions) (*CreateResult, error) {
	lifecycle, err := lifecycleToAPI(opts.Lifecycle)
	if err != nil {
		return nil, err
	}
	created, err := c.api.CreateSandbox(ctx, managementapi.CreateSandboxParams{TeamId: c.teamID()}, managementapi.CreateSandboxRequest{
		Name:              optional(opts.Name),
		CreateIfNotExists: optional(opts.CreateIfNotExists),
		Image:             optional(opts.Image),
		Memory:            optional(opts.Memory),
		Region:            optional(opts.Region),
		Envs:              envsToAPI(opts.Envs),
		Labels:            labelsToAPI(opts.Labels),
		ExternalId:        optional(opts.ExternalID),
		Lifecycle:         lifecycle,
		Ports:             portsToAPI(opts.Ports),
		Network:           networkToAPI(opts.Network),
	})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	info, err := infoFromAPI(created)
	if err != nil {
		return nil, err
	}
	if info.URL == "" {
		return nil, fmt.Errorf("sandbox %s was created but has no URL yet; get it again later", info.Name)
	}
	return &CreateResult{Sandbox: c.newSandbox(info.Name, info.URL), Info: info}, nil
}

// GetInfoOptions are the options for [Client.GetInfo].
type GetInfoOptions struct {
	// Name is the sandbox's name. Required.
	Name string

	// ShowSecrets returns environment variable values unmasked. Requires the
	// workspace admin role.
	ShowSecrets bool
}

// GetInfo gets a sandbox's record.
func (c *Client) GetInfo(ctx context.Context, opts GetInfoOptions) (*Info, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	record, err := c.api.GetSandbox(ctx, opts.Name, managementapi.GetSandboxParams{
		TeamId:      c.teamID(),
		ShowSecrets: optional(opts.ShowSecrets),
	})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	info, err := infoFromAPI(record)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// GetOptions are the options for [Client.Get].
type GetOptions struct {
	// Name is the sandbox's name. Required.
	Name string
}

// Get gets a [Sandbox] to work in, by getting the sandbox's record for its
// URL.
func (c *Client) Get(ctx context.Context, opts GetOptions) (*Sandbox, error) {
	info, err := c.GetInfo(ctx, GetInfoOptions{Name: opts.Name})
	if err != nil {
		return nil, err
	}
	if info.URL == "" {
		return nil, fmt.Errorf("sandbox %s has no URL yet", info.Name)
	}
	return c.newSandbox(info.Name, info.URL), nil
}

// ListOptions are the options for [Client.List].
type ListOptions struct {
	// Query searches sandbox names and labels.
	Query string

	// Statuses keeps only sandboxes with one of these statuses. Cannot be
	// combined with ExternalID.
	Statuses []Status

	// ExternalID keeps only sandboxes with this external identifier. Cannot
	// be combined with Statuses.
	ExternalID string

	// PageSize is how many sandboxes each underlying request fetches. Zero
	// uses the server's default.
	PageSize int
}

// List lists sandbox records, fetching further pages as iteration reaches
// them. An error is yielded as the second value and ends the iteration.
func (c *Client) List(ctx context.Context, opts ListOptions) iter.Seq2[*Info, error] {
	return func(yield func(*Info, error) bool) {
		params := managementapi.ListSandboxesParams{
			TeamId:     c.teamID(),
			Q:          optional(opts.Query),
			ExternalId: optional(opts.ExternalID),
			Limit:      optional(opts.PageSize),
		}
		if opts.Statuses != nil {
			statuses := make([]string, 0, len(opts.Statuses))
			for _, status := range opts.Statuses {
				statuses = append(statuses, string(status))
			}
			params.Status = &statuses
		}
		paginate(yield, "sandbox list", func(cursor *string) ([]managementapi.Sandbox, managementapi.SandboxApiPagination, error) {
			params.Cursor = cursor
			page, err := c.api.ListSandboxes(ctx, params)
			if err != nil {
				return nil, managementapi.SandboxApiPagination{}, err
			}
			return page.Items, page.Pagination, nil
		}, infoFromAPI)
	}
}

// paginate yields every item of a cursor-paged listing, fetching each page as
// iteration reaches it. what names the listing in errors.
func paginate[T, R any](
	yield func(*R, error) bool,
	what string,
	fetch func(cursor *string) ([]T, managementapi.SandboxApiPagination, error),
	convert func(*T) (R, error),
) {
	// A server repeating a cursor would otherwise page forever.
	seenCursors := map[string]bool{}
	var cursor *string
	for {
		items, pagination, err := fetch(cursor)
		if err != nil {
			yield(nil, controlPlaneError(err))
			return
		}
		for i := range items {
			item, err := convert(&items[i])
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(&item, nil) {
				return
			}
		}
		next := deref(pagination.Cursor)
		if !pagination.HasMore || next == "" {
			return
		}
		if seenCursors[next] {
			yield(nil, fmt.Errorf("%s returned a cursor it already returned", what))
			return
		}
		seenCursors[next] = true
		cursor = &next
	}
}

// UpdateOptions are the options for [Client.Update]. Unset fields stay
// unchanged, and set ones replace their previous values, including set but
// empty maps and slices. A field cannot be cleared back to unset.
type UpdateOptions struct {
	// Name is the sandbox's name. Required.
	Name string

	// Enabled false cuts the sandbox off from every connection until set
	// back to true. Nil leaves it unchanged.
	Enabled *bool

	// Lifecycle replaces when the sandbox expires.
	Lifecycle *Lifecycle

	// Region moves the sandbox.
	Region string

	// Envs replace the sandbox's environment variables. Values returned
	// masked in [Info.Envs] overwrite the real values if sent back.
	Envs map[string]EnvValue

	// Image is the image reference, including its tag.
	Image string

	// Ports replace the ports the sandbox exposes.
	Ports []Port

	// ExternalID is a caller-owned identifier for external lookups.
	ExternalID string

	// Labels replace the sandbox's labels.
	Labels map[string]string
}

// Update updates a sandbox and returns its new record.
func (c *Client) Update(ctx context.Context, opts UpdateOptions) (*Info, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	lifecycle, err := lifecycleToAPI(opts.Lifecycle)
	if err != nil {
		return nil, err
	}
	updated, err := c.api.UpdateSandbox(ctx, opts.Name, managementapi.UpdateSandboxParams{TeamId: c.teamID()}, managementapi.UpdateSandboxRequest{
		Enabled:    opts.Enabled,
		Lifecycle:  lifecycle,
		Region:     optional(opts.Region),
		Envs:       envsToAPI(opts.Envs),
		Image:      optional(opts.Image),
		Ports:      portsToAPI(opts.Ports),
		ExternalId: optional(opts.ExternalID),
		Labels:     labelsToAPI(opts.Labels),
	})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	info, err := infoFromAPI(updated)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// DeleteOptions are the options for [Client.Delete].
type DeleteOptions struct {
	// Name is the sandbox's name. Required.
	Name string
}

// Delete deletes a sandbox and returns its record. Deletion continues after
// this returns, so the record usually shows the sandbox as deleting.
func (c *Client) Delete(ctx context.Context, opts DeleteOptions) (*Info, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	deleted, err := c.api.DeleteSandbox(ctx, opts.Name, managementapi.DeleteSandboxParams{TeamId: c.teamID()})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	info, err := infoFromAPI(deleted)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// SandboxFromURLOptions are the options for [Client.SandboxFromURL].
type SandboxFromURLOptions struct {
	// URL is the base URL of the sandbox's execution API, as in [Info.URL].
	// Required.
	URL string
}

// SandboxFromURL returns a [Sandbox] for a sandbox whose URL is already
// known, without a call. Prefer [Client.Get] or [Client.Create], which take
// the URL from the sandbox's record. The returned Sandbox has no name.
func (c *Client) SandboxFromURL(opts SandboxFromURLOptions) (*Sandbox, error) {
	if opts.URL == "" {
		return nil, errors.New("URL is required")
	}
	return c.newSandbox("", opts.URL), nil
}

// teamID returns the team_id query value, nil to leave it unset.
func (c *Client) teamID() *string {
	if c.options.TeamID == "" {
		return nil
	}
	return &c.options.TeamID
}
