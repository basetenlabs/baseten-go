package client

import (
	"context"
	"errors"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/internal/tokenauth"
)

// DriveClient works with drives: shared storage that outlives sandboxes and
// that many sandboxes can mount at once. Mount one in a running sandbox with
// the sandbox package. Get it from [ManagementClient.Drives].
//
// Drives are in pre-release and may change.
type DriveClient struct {
	api *managementapi.Client
}

// Drives works with drives.
func (c *ManagementClient) Drives() *DriveClient {
	return c.drives
}

// DriveStatus is the status of a drive. Other values may be added, so do not
// treat the constants as exhaustive.
type DriveStatus string

// Values for DriveStatus.
const (
	DriveStatusDeploying DriveStatus = "DEPLOYING"
	DriveStatusDeployed  DriveStatus = "DEPLOYED"
	DriveStatusFailed    DriveStatus = "FAILED"
	DriveStatusDeleting  DriveStatus = "DELETING"
)

// DriveInfo is a drive's record.
type DriveInfo struct {
	// Name is the drive's unique name in its team.
	Name string

	// Region is where the drive is. Only sandboxes in the same region can
	// mount it.
	Region string

	// Status is the drive's status.
	Status DriveStatus

	// Labels are the key-value pairs for organizing and filtering.
	Labels map[string]string

	// CreatedAt is when the drive was created, zero when unknown.
	CreatedAt time.Time

	// UpdatedAt is when the drive was last updated, zero when unknown.
	UpdatedAt time.Time

	// CreatedBy is who created the drive.
	CreatedBy string

	// UpdatedBy is who last updated the drive.
	UpdatedBy string
}

// DriveCreateOptions are the options for [DriveClient.Create]. Unset fields
// are omitted, so the server applies its defaults.
type DriveCreateOptions struct {
	// TeamID is the team to create the drive in. Empty uses the caller's only
	// team, and is an error for callers in more than one team.
	TeamID string

	// Name is the drive's unique name in its team. The server assigns one
	// when empty.
	Name string

	// Region is where the drive is. Empty uses the default sandbox region.
	// Only sandboxes in the same region can mount it.
	Region string

	// Labels are the key-value pairs for organizing and filtering.
	Labels map[string]string
}

// Create creates a drive.
func (c *DriveClient) Create(ctx context.Context, opts DriveCreateOptions) (*DriveInfo, error) {
	drive, err := c.api.CreateDrive(ctx, managementapi.CreateDriveParams{TeamId: optional(opts.TeamID)}, managementapi.CreateSandboxDriveRequest{
		Name:   optional(opts.Name),
		Region: optional(opts.Region),
		Labels: driveLabelsToAPI(opts.Labels),
	})
	if err != nil {
		return nil, err
	}
	info := driveInfoFromAPI(drive)
	return &info, nil
}

// DriveGetInfoOptions are the options for [DriveClient.GetInfo].
type DriveGetInfoOptions struct {
	// TeamID is the team the drive is in. Empty uses the caller's only team,
	// and is an error for callers in more than one team.
	TeamID string

	// Name is the drive's name. Required.
	Name string
}

// GetInfo gets a drive's record.
func (c *DriveClient) GetInfo(ctx context.Context, opts DriveGetInfoOptions) (*DriveInfo, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	drive, err := c.api.GetDrive(ctx, opts.Name, managementapi.GetDriveParams{TeamId: optional(opts.TeamID)})
	if err != nil {
		return nil, err
	}
	info := driveInfoFromAPI(drive)
	return &info, nil
}

// DriveListOptions are the options for [DriveClient.List].
type DriveListOptions struct {
	// TeamID is the team to list drives in. Empty uses the caller's only
	// team, and is an error for callers in more than one team.
	TeamID string

	// Query searches drive names.
	Query string

	// Statuses keeps only drives with one of these statuses.
	Statuses []DriveStatus

	// PageSize is how many drives each underlying request fetches. Zero uses
	// the server's default.
	PageSize int
}

// List lists drive records, newest first, fetching further pages as iteration
// reaches them. An error is yielded as the second value and ends the
// iteration.
func (c *DriveClient) List(ctx context.Context, opts DriveListOptions) iter.Seq2[*DriveInfo, error] {
	return func(yield func(*DriveInfo, error) bool) {
		params := managementapi.ListDrivesParams{
			TeamId: optional(opts.TeamID),
			Q:      optional(opts.Query),
			Limit:  optional(opts.PageSize),
		}
		if opts.Statuses != nil {
			statuses := make([]managementapi.SandboxDriveStatus, 0, len(opts.Statuses))
			for _, status := range opts.Statuses {
				statuses = append(statuses, managementapi.SandboxDriveStatus(status))
			}
			params.Status = &statuses
		}
		// A server repeating a cursor would otherwise page forever.
		seenCursors := map[string]bool{}
		for {
			page, err := c.api.ListDrives(ctx, params)
			if err != nil {
				yield(nil, err)
				return
			}
			for i := range page.Items {
				info := driveInfoFromAPI(&page.Items[i])
				if !yield(&info, nil) {
					return
				}
			}
			next := deref(page.Pagination.Cursor)
			if !page.Pagination.HasMore || next == "" {
				return
			}
			if seenCursors[next] {
				yield(nil, errors.New("drive list returned a cursor it already returned"))
				return
			}
			seenCursors[next] = true
			params.Cursor = &next
		}
	}
}

// DriveUpdateOptions are the options for [DriveClient.Update]. The name and
// region of a drive cannot be changed.
type DriveUpdateOptions struct {
	// TeamID is the team the drive is in. Empty uses the caller's only team,
	// and is an error for callers in more than one team.
	TeamID string

	// Name is the drive's name. Required.
	Name string

	// Labels replace the drive's labels. A non-nil empty map removes them
	// all.
	Labels map[string]string
}

// Update updates a drive and returns its new record.
func (c *DriveClient) Update(ctx context.Context, opts DriveUpdateOptions) (*DriveInfo, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	drive, err := c.api.UpdateDrive(ctx, opts.Name, managementapi.UpdateDriveParams{TeamId: optional(opts.TeamID)}, managementapi.UpdateSandboxDriveRequest{
		Labels: driveLabelsToAPI(opts.Labels),
	})
	if err != nil {
		return nil, err
	}
	info := driveInfoFromAPI(drive)
	return &info, nil
}

// DriveDeleteOptions are the options for [DriveClient.Delete].
type DriveDeleteOptions struct {
	// TeamID is the team the drive is in. Empty uses the caller's only team,
	// and is an error for callers in more than one team.
	TeamID string

	// Name is the drive's name. Required.
	Name string
}

// Delete deletes a drive and its data, and returns its record. Deletion
// continues after this returns, so the record usually shows the drive as
// deleting.
func (c *DriveClient) Delete(ctx context.Context, opts DriveDeleteOptions) (*DriveInfo, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	drive, err := c.api.DeleteDrive(ctx, opts.Name, managementapi.DeleteDriveParams{TeamId: optional(opts.TeamID)})
	if err != nil {
		return nil, err
	}
	info := driveInfoFromAPI(drive)
	return &info, nil
}

// newDriveClient returns a DriveClient that calls the drive routes of the
// management API with tokens minted through management.
func newDriveClient(management *managementapi.Client) *DriveClient {
	return &DriveClient{api: &managementapi.Client{
		BaseURL:    management.BaseURL,
		HTTPClient: &drivePathRewriteDoer{inner: newDriveTokenDoer(management)},
		Headers:    management.Headers,
	}}
}

func driveInfoFromAPI(drive *managementapi.SandboxDrive) DriveInfo {
	return DriveInfo{
		Name:      deref(drive.Name),
		Region:    drive.Region,
		Status:    DriveStatus(drive.Status),
		Labels:    deref(drive.Labels),
		CreatedAt: deref(drive.CreatedAt),
		UpdatedAt: deref(drive.UpdatedAt),
		CreatedBy: deref(drive.CreatedBy),
		UpdatedBy: deref(drive.UpdatedBy),
	}
}

func driveLabelsToAPI(labels map[string]string) *managementapi.SandboxMetadataLabels {
	if labels == nil {
		return nil
	}
	result := managementapi.SandboxMetadataLabels(labels)
	return &result
}

// The drive routes do not accept API keys directly, so their requests carry a
// token minted through the management API. This mirrors the token source in
// the sandbox package; keep the two in sync.

// Refresh this long before the token's stated expiry, to allow for clock skew
// between this machine and the server, and for time spent in flight. Capped at
// half the token's lifetime.
const driveTokenExpiryLeeway = 60 * time.Second

// A few retries cover a new token being invalidated in the same event as the
// old one. Past that, the rejection is returned, since tokens that keep
// getting rejected point at something a new token cannot fix.
const driveTokenInvalidationMaxRetries = 2

// Revocation rejects every token issued before a cutoff, the time of the
// revoking event rounded up to the next whole second. A token minted right
// after the event can fall before the cutoff and be rejected too, so after
// the first resend, each one waits this long to be minted past it. A variable
// so tests can shorten it.
var driveTokenRevokedRetryDelay = time.Second

// driveTokenMintTimeout bounds one token exchange, which no caller's
// cancellation reaches. A variable so tests can shorten it.
var driveTokenMintTimeout = 30 * time.Second

// driveTokenDoer sends every request with a token minted through a management
// client and cached until shortly before it expires, and sends it again with a
// new one when the server rejects the token as revoked.
type driveTokenDoer struct {
	inner httpDoer
	cache *tokenauth.Cache
}

// newDriveTokenDoer returns a doer that mints tokens through management and
// sends requests with management's HTTP client.
func newDriveTokenDoer(management *managementapi.Client) *driveTokenDoer {
	return &driveTokenDoer{
		inner: management.HTTPClient,
		cache: tokenauth.NewCache(tokenauth.CacheOptions{
			Mint: func(ctx context.Context) (tokenauth.Token, error) {
				minted, err := management.PostToken(ctx, managementapi.CreateTokenRequest{
					Scopes: []managementapi.TokenScope{managementapi.TokenScope_sandboxes},
				})
				if err != nil {
					return tokenauth.Token{}, err
				}
				return tokenauth.Token{Value: minted.Token, ExpiresAt: minted.ExpiresAt}, nil
			},
			RefreshLeeway: driveTokenExpiryLeeway,
			MintTimeout:   driveTokenMintTimeout,
		}),
	}
}

func (d *driveTokenDoer) Do(req *http.Request) (*http.Response, error) {
	// A body that cannot be rebuilt can be sent only once, so such a request
	// is never sent again. The SDK itself never sends one; only raw API calls
	// can.
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	for retry := 0; ; retry++ {
		token, err := d.cache.Token(req.Context())
		if err != nil {
			// Do closes the request body even on error, as http.Client does.
			if req.Body != nil {
				req.Body.Close()
			}
			return nil, err
		}
		// Replaces the API key the management client's headers carry.
		attempt := req.Clone(req.Context())
		attempt.Header.Set("Authorization", "Bearer "+token.Value)
		if retry > 0 && req.GetBody != nil {
			if attempt.Body, err = req.GetBody(); err != nil {
				return nil, err
			}
		}
		resp, err := d.inner.Do(attempt)
		if err != nil {
			return nil, err
		}
		if !isDriveTokenRevoked(resp) {
			return resp, nil
		}
		// Dropped even when the request is not sent again, so later requests
		// do not reuse it.
		d.cache.Invalidate(token.Value)
		if !replayable || retry >= driveTokenInvalidationMaxRetries {
			return resp, nil
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if retry > 0 {
			timer := time.NewTimer(driveTokenRevokedRetryDelay)
			select {
			case <-req.Context().Done():
				timer.Stop()
				return nil, req.Context().Err()
			case <-timer.C:
			}
		}
	}
}

// isDriveTokenRevoked reports whether the server rejected a request's token as
// revoked, which it can do before the token's stated expiry, for example after
// a role change. The request was rejected before any work was done, so sending
// it again with a new token is safe even when it has side effects.
func isDriveTokenRevoked(resp *http.Response) bool {
	return resp.StatusCode == http.StatusUnauthorized && resp.Header.Get("x-blaxel-error-code") == "TOKEN_REVOKED"
}

// drivePathRewriteDoer sends drive requests to /v1/sandboxes/drives, the only
// drive routes served so far.
//
// TODO: Remove before merge, once /v1/drives/instances is served.
type drivePathRewriteDoer struct {
	inner httpDoer
}

func (d *drivePathRewriteDoer) Do(req *http.Request) (*http.Response, error) {
	if rest, ok := strings.CutPrefix(req.URL.Path, "/v1/drives/instances"); ok {
		req = req.Clone(req.Context())
		req.URL.Path = "/v1/sandboxes/drives" + rest
		req.URL.RawPath = ""
	}
	return d.inner.Do(req)
}
