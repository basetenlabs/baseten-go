package sandbox

import (
	"context"
	"errors"
	"iter"
	"time"

	"github.com/basetenlabs/baseten-go/client/managementapi"
)

// PreviewService manages a sandbox's previews, which expose a sandbox port on
// an HTTPS URL. Get it from [Sandbox.Previews]. Previews need the sandbox's
// name, so every call fails on a Sandbox from [Client.SandboxFromURL].
type PreviewService struct {
	sandbox *Sandbox
}

func newPreviewService(sandbox *Sandbox) *PreviewService {
	return &PreviewService{sandbox: sandbox}
}

// PreviewStatus is the status of a preview. Other values may be added, so do
// not treat the constants as exhaustive.
type PreviewStatus string

// Values for PreviewStatus.
const (
	PreviewStatusDeploying  PreviewStatus = "DEPLOYING"
	PreviewStatusDeployed   PreviewStatus = "DEPLOYED"
	PreviewStatusFailed     PreviewStatus = "FAILED"
	PreviewStatusDeleting   PreviewStatus = "DELETING"
	PreviewStatusTerminated PreviewStatus = "TERMINATED"
)

// PreviewInfo is a preview's record.
type PreviewInfo struct {
	// Name is the preview's name, unique within the sandbox.
	Name string

	// Port is the sandbox port the preview exposes.
	Port int

	// URL is the HTTPS URL serving the port. Requests to a private preview
	// need a token from [PreviewService.CreateToken].
	URL string

	// Status is the preview's status.
	Status PreviewStatus

	// Public is whether the URL is reachable without a token.
	Public bool

	// ResponseHeaders are added to every response the preview serves.
	ResponseHeaders map[string]string

	// RequestHeaders are added to every request forwarded to the sandbox.
	// Values come back as "****" unless revealed with
	// [PreviewGetInfoOptions.ShowSecrets].
	RequestHeaders map[string]string

	// ExpiresAt is when the preview is deleted, zero when it does not expire.
	ExpiresAt time.Time

	// Region is the region serving the preview, the sandbox's region.
	Region string

	// CreatedAt is when the preview was created.
	CreatedAt time.Time

	// UpdatedAt is when the preview was last updated, zero when it never was.
	UpdatedAt time.Time

	// CreatedBy is who created the preview.
	CreatedBy string

	// UpdatedBy is who last updated the preview.
	UpdatedBy string
}

// PreviewTokenInfo is an access token for a private preview.
type PreviewTokenInfo struct {
	// Name is the token's name.
	Name string

	// Token is the value granting access. Append it to the preview URL as
	// ?preview_token=<token>; the first request sets a cookie so a browser
	// can keep navigating without it.
	Token string

	// ExpiresAt is when the token stops working.
	ExpiresAt time.Time

	// Expired is whether ExpiresAt has passed.
	Expired bool
}

// PreviewCreateOptions are the options for [PreviewService.Create].
type PreviewCreateOptions struct {
	// Port is the sandbox port to expose. Required.
	Port int

	// Name is the preview's name, unique within the sandbox. Empty lets the
	// server generate one.
	Name string

	// CreateIfNotExists returns the existing preview with this name, unchanged,
	// instead of failing.
	CreateIfNotExists bool

	// Public makes the URL reachable without a token. False, the default, makes
	// the preview private.
	Public bool

	// ResponseHeaders are added to every response the preview serves, for
	// example CORS headers.
	ResponseHeaders map[string]string

	// RequestHeaders are added to every request forwarded to the sandbox, for
	// example an Authorization header the application expects.
	RequestHeaders map[string]string

	// TTL deletes the preview this long after the request. Cannot be combined
	// with ExpiresAt.
	TTL time.Duration

	// ExpiresAt deletes the preview at this time. Cannot be combined with TTL.
	ExpiresAt time.Time
}

// Create creates a preview exposing a sandbox port.
func (s *PreviewService) Create(ctx context.Context, opts PreviewCreateOptions) (*PreviewInfo, error) {
	if err := s.checkName(); err != nil {
		return nil, err
	}
	if opts.Port == 0 {
		return nil, errors.New("Port is required")
	}
	c := s.sandbox.client
	created, err := c.api.CreateSandboxPreview(ctx, s.sandbox.name, managementapi.CreateSandboxPreviewParams{TeamId: c.teamID()}, managementapi.CreateSandboxPreviewRequest{
		Port:              opts.Port,
		Name:              optional(opts.Name),
		CreateIfNotExists: optional(opts.CreateIfNotExists),
		Public:            optional(opts.Public),
		ResponseHeaders:   previewHeadersToAPI(opts.ResponseHeaders),
		RequestHeaders:    previewHeadersToAPI(opts.RequestHeaders),
		Ttl:               previewTTLToAPI(opts.TTL),
		ExpiresAt:         optional(opts.ExpiresAt),
	})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	return previewInfoResult(created)
}

// PreviewGetInfoOptions are the options for [PreviewService.GetInfo].
type PreviewGetInfoOptions struct {
	// Name is the preview's name. Required.
	Name string

	// ShowSecrets returns request header values unmasked. Requires the
	// workspace admin role.
	ShowSecrets bool
}

// GetInfo gets a preview's record.
func (s *PreviewService) GetInfo(ctx context.Context, opts PreviewGetInfoOptions) (*PreviewInfo, error) {
	if err := s.checkName(); err != nil {
		return nil, err
	}
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	c := s.sandbox.client
	record, err := c.api.GetSandboxPreview(ctx, s.sandbox.name, opts.Name, managementapi.GetSandboxPreviewParams{
		TeamId:      c.teamID(),
		ShowSecrets: optional(opts.ShowSecrets),
	})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	return previewInfoResult(record)
}

// PreviewListOptions are the options for [PreviewService.List].
type PreviewListOptions struct {
	// PageSize is how many previews each underlying request fetches. Zero uses
	// the server's default.
	PageSize int
}

// List lists the sandbox's preview records, fetching further pages as
// iteration reaches them. An error is yielded as the second value and ends
// the iteration.
func (s *PreviewService) List(ctx context.Context, opts PreviewListOptions) iter.Seq2[*PreviewInfo, error] {
	return func(yield func(*PreviewInfo, error) bool) {
		if err := s.checkName(); err != nil {
			yield(nil, err)
			return
		}
		c := s.sandbox.client
		params := managementapi.ListSandboxPreviewsParams{
			TeamId: c.teamID(),
			Limit:  optional(opts.PageSize),
		}
		paginate(yield, "preview list", func(cursor *string) ([]managementapi.SandboxPreview, managementapi.SandboxApiPagination, error) {
			params.Cursor = cursor
			page, err := c.api.ListSandboxPreviews(ctx, s.sandbox.name, params)
			if err != nil {
				return nil, managementapi.SandboxApiPagination{}, err
			}
			return page.Items, page.Pagination, nil
		}, previewInfoFromAPI)
	}
}

// PreviewUpdateOptions are the options for [PreviewService.Update]. Unset
// fields stay unchanged, and set ones replace their previous values, including
// set but empty maps. The name and port cannot be changed.
type PreviewUpdateOptions struct {
	// Name is the preview's name. Required.
	Name string

	// Public sets whether the URL is reachable without a token. Nil leaves it
	// unchanged.
	Public *bool

	// ResponseHeaders replace the headers added to every response. Nil leaves
	// them unchanged. A non-nil map replaces the whole set, so headers left out
	// are removed, and an empty map removes them all.
	ResponseHeaders map[string]string

	// RequestHeaders replace the headers added to every forwarded request. Nil
	// leaves them unchanged. A non-nil map replaces the whole set, so headers
	// left out are removed, and an empty map removes them all. Sending back a
	// "****" value from [PreviewInfo.RequestHeaders] is rejected, so to change
	// one header, get the others with [PreviewGetInfoOptions.ShowSecrets] and
	// send them all.
	RequestHeaders map[string]string

	// TTL deletes the preview this long after the request, replacing any
	// previous expiration. Zero leaves the expiration unchanged. Cannot be
	// combined with ExpiresAt.
	TTL time.Duration

	// ExpiresAt deletes the preview at this time, replacing any previous
	// expiration. Zero leaves the expiration unchanged. Cannot be combined with
	// TTL.
	ExpiresAt time.Time
}

// Update updates a preview and returns its new record. The URL does not
// change.
func (s *PreviewService) Update(ctx context.Context, opts PreviewUpdateOptions) (*PreviewInfo, error) {
	if err := s.checkName(); err != nil {
		return nil, err
	}
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	c := s.sandbox.client
	updated, err := c.api.UpdateSandboxPreview(ctx, s.sandbox.name, opts.Name, managementapi.UpdateSandboxPreviewParams{TeamId: c.teamID()}, managementapi.UpdateSandboxPreviewRequest{
		Public:          opts.Public,
		ResponseHeaders: previewHeadersToAPI(opts.ResponseHeaders),
		RequestHeaders:  previewHeadersToAPI(opts.RequestHeaders),
		Ttl:             previewTTLToAPI(opts.TTL),
		ExpiresAt:       optional(opts.ExpiresAt),
	})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	return previewInfoResult(updated)
}

// PreviewDeleteOptions are the options for [PreviewService.Delete].
type PreviewDeleteOptions struct {
	// Name is the preview's name. Required.
	Name string
}

// Delete deletes a preview and its tokens, and returns its record. The URL
// stops serving at once, but the record may show the preview as deleting
// while the server finishes; this does not wait for that.
func (s *PreviewService) Delete(ctx context.Context, opts PreviewDeleteOptions) (*PreviewInfo, error) {
	if err := s.checkName(); err != nil {
		return nil, err
	}
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	c := s.sandbox.client
	deleted, err := c.api.DeleteSandboxPreview(ctx, s.sandbox.name, opts.Name, managementapi.DeleteSandboxPreviewParams{TeamId: c.teamID()})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	return previewInfoResult(deleted)
}

// PreviewCreateTokenOptions are the options for [PreviewService.CreateToken].
type PreviewCreateTokenOptions struct {
	// PreviewName is the private preview's name. Required.
	PreviewName string

	// ExpiresAt is when the token stops working. Required.
	ExpiresAt time.Time

	// CreateIfNotExists returns an existing unexpired token of the preview
	// that stays valid at least until ExpiresAt, instead of creating one. A
	// token created moments before may not be found yet, so a new one may be
	// created anyway.
	CreateIfNotExists bool
}

// CreateToken creates a token granting access to a private preview.
func (s *PreviewService) CreateToken(ctx context.Context, opts PreviewCreateTokenOptions) (*PreviewTokenInfo, error) {
	if err := s.checkName(); err != nil {
		return nil, err
	}
	if opts.PreviewName == "" {
		return nil, errors.New("PreviewName is required")
	}
	if opts.ExpiresAt.IsZero() {
		return nil, errors.New("ExpiresAt is required")
	}
	c := s.sandbox.client
	created, err := c.api.CreateSandboxPreviewToken(ctx, s.sandbox.name, opts.PreviewName, managementapi.CreateSandboxPreviewTokenParams{TeamId: c.teamID()}, managementapi.CreateSandboxPreviewTokenRequest{
		ExpiresAt:         opts.ExpiresAt,
		CreateIfNotExists: optional(opts.CreateIfNotExists),
	})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	info := previewTokenInfoFromAPI(created)
	return &info, nil
}

// PreviewListTokensOptions are the options for [PreviewService.ListTokens].
type PreviewListTokensOptions struct {
	// PreviewName is the private preview's name. Required.
	PreviewName string

	// PageSize is how many tokens each underlying request fetches. Zero uses
	// the server's default.
	PageSize int
}

// ListTokens lists a private preview's tokens, including expired ones until
// the server removes them, fetching further pages as iteration reaches them.
// A public preview has no tokens, and the server rejects the call. An error is
// yielded as the second value and ends the iteration.
func (s *PreviewService) ListTokens(ctx context.Context, opts PreviewListTokensOptions) iter.Seq2[*PreviewTokenInfo, error] {
	return func(yield func(*PreviewTokenInfo, error) bool) {
		if err := s.checkName(); err != nil {
			yield(nil, err)
			return
		}
		if opts.PreviewName == "" {
			yield(nil, errors.New("PreviewName is required"))
			return
		}
		c := s.sandbox.client
		params := managementapi.ListSandboxPreviewTokensParams{
			TeamId: c.teamID(),
			Limit:  optional(opts.PageSize),
		}
		paginate(yield, "preview token list", func(cursor *string) ([]managementapi.SandboxPreviewToken, managementapi.SandboxApiPagination, error) {
			params.Cursor = cursor
			page, err := c.api.ListSandboxPreviewTokens(ctx, s.sandbox.name, opts.PreviewName, params)
			if err != nil {
				return nil, managementapi.SandboxApiPagination{}, err
			}
			return page.Items, page.Pagination, nil
		}, func(token *managementapi.SandboxPreviewToken) (PreviewTokenInfo, error) {
			return previewTokenInfoFromAPI(token), nil
		})
	}
}

// PreviewDeleteTokenOptions are the options for [PreviewService.DeleteToken].
type PreviewDeleteTokenOptions struct {
	// PreviewName is the private preview's name. Required.
	PreviewName string

	// TokenName is the token's name. Required.
	TokenName string
}

// DeleteToken revokes a preview token. Requests using it are rejected at
// once.
func (s *PreviewService) DeleteToken(ctx context.Context, opts PreviewDeleteTokenOptions) error {
	if err := s.checkName(); err != nil {
		return err
	}
	if opts.PreviewName == "" {
		return errors.New("PreviewName is required")
	}
	if opts.TokenName == "" {
		return errors.New("TokenName is required")
	}
	c := s.sandbox.client
	if err := c.api.DeleteSandboxPreviewToken(ctx, s.sandbox.name, opts.PreviewName, opts.TokenName, managementapi.DeleteSandboxPreviewTokenParams{TeamId: c.teamID()}); err != nil {
		return controlPlaneError(err)
	}
	return nil
}

// checkName fails for a Sandbox without a name, one from SandboxFromURL.
func (s *PreviewService) checkName() error {
	if s.sandbox.name == "" {
		return errors.New("previews need the sandbox's name, which a Sandbox from SandboxFromURL does not have; use Client.Get")
	}
	return nil
}

func previewInfoResult(preview *managementapi.SandboxPreview) (*PreviewInfo, error) {
	info, err := previewInfoFromAPI(preview)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

func previewInfoFromAPI(preview *managementapi.SandboxPreview) (PreviewInfo, error) {
	return PreviewInfo{
		Name:            deref(preview.Name),
		Port:            deref(preview.Port),
		URL:             deref(preview.Url),
		Status:          PreviewStatus(deref(preview.Status)),
		Public:          preview.Public,
		ResponseHeaders: deref(preview.ResponseHeaders),
		RequestHeaders:  deref(preview.RequestHeaders),
		ExpiresAt:       deref(preview.ExpiresAt),
		Region:          deref(preview.Region),
		CreatedAt:       deref(preview.CreatedAt),
		UpdatedAt:       deref(preview.UpdatedAt),
		CreatedBy:       deref(preview.CreatedBy),
		UpdatedBy:       deref(preview.UpdatedBy),
	}, nil
}

func previewTokenInfoFromAPI(token *managementapi.SandboxPreviewToken) PreviewTokenInfo {
	return PreviewTokenInfo{
		Name:      token.Name,
		Token:     token.Token,
		ExpiresAt: token.ExpiresAt,
		Expired:   token.Expired,
	}
}

func previewHeadersToAPI(headers map[string]string) *managementapi.SandboxPreviewHeaders {
	if headers == nil {
		return nil
	}
	result := managementapi.SandboxPreviewHeaders(headers)
	return &result
}

func previewTTLToAPI(ttl time.Duration) *managementapi.SandboxPreviewTtl {
	if ttl == 0 {
		return nil
	}
	result := formatDuration(ttl)
	return &result
}
