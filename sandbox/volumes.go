package sandbox

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/basetenlabs/baseten-go/client/managementapi"
)

// VolumeClient works with persistent volumes, which keep their data beyond
// the sandboxes they are mounted in. Mount one at sandbox creation with
// [CreateOptions.Volumes]. Get it from [Client.Volumes].
type VolumeClient struct {
	client *Client
}

// Volumes works with persistent volumes.
func (c *Client) Volumes() *VolumeClient {
	return c.volumes
}

// VolumeStatus is the status of a volume. Other values may be added, so do
// not treat the constants as exhaustive.
type VolumeStatus string

// Values for VolumeStatus.
const (
	VolumeStatusDeploying VolumeStatus = "DEPLOYING"
	VolumeStatusDeployed  VolumeStatus = "DEPLOYED"
	VolumeStatusFailed    VolumeStatus = "FAILED"
	VolumeStatusDeleting  VolumeStatus = "DELETING"
)

// VolumeInfo is a volume's record.
type VolumeInfo struct {
	// Name is the volume's unique name.
	Name string

	// Size is the volume's capacity as a size such as "10Gi", when the server
	// returns it in that form. Empty when it returns SizeMegabytes instead.
	Size string

	// SizeMegabytes is the volume's capacity in megabytes, when the server
	// returns it in that form. Zero when it returns Size instead.
	SizeMegabytes int

	// Region is where the volume is. Only sandboxes in the same region can
	// mount it.
	Region string

	// Status is the volume's status.
	Status VolumeStatus

	// AttachedTo is the name of the sandbox the volume is mounted in, empty
	// when it is not mounted.
	AttachedTo string

	// Labels are the key-value pairs for organizing and filtering. Nil when
	// not returned: [VolumeClient.List] omits them.
	Labels map[string]string

	// CreatedAt is when the volume was created, zero when unknown.
	CreatedAt time.Time

	// UpdatedAt is when the volume was last updated, zero when unknown.
	UpdatedAt time.Time

	// CreatedBy is who created the volume.
	CreatedBy string

	// UpdatedBy is who last updated the volume.
	UpdatedBy string
}

// VolumeCreateOptions are the options for [VolumeClient.Create]. Unset fields
// are omitted, so the server applies its defaults.
type VolumeCreateOptions struct {
	// Name is the volume's unique name. The server assigns one when empty.
	Name string

	// Size is the volume's capacity as a size such as "10Gi" or "512Mi". It
	// can grow later but never shrink. One of Size and SizeMegabytes is
	// required, and they cannot be combined.
	Size string

	// SizeMegabytes is the volume's capacity in megabytes. Cannot be combined
	// with Size.
	SizeMegabytes int

	// Region is where the volume is. Empty uses the default sandbox region.
	// Only sandboxes in the same region can mount it.
	Region string

	// Labels are the key-value pairs for organizing and filtering.
	Labels map[string]string
}

// Create creates a volume. The volume becomes ready asynchronously, so its
// status may still be deploying.
func (c *VolumeClient) Create(ctx context.Context, opts VolumeCreateOptions) (*VolumeInfo, error) {
	var size managementapi.CreateSandboxVolumeRequest_Size
	var err error
	switch {
	case opts.Size != "" && opts.SizeMegabytes != 0:
		return nil, errors.New("Size and SizeMegabytes cannot both be set")
	case opts.Size != "":
		err = size.FromCreateSandboxVolumeRequestSize0(opts.Size)
	case opts.SizeMegabytes != 0:
		err = size.FromCreateSandboxVolumeRequestSize1(opts.SizeMegabytes)
	default:
		return nil, errors.New("Size or SizeMegabytes is required")
	}
	if err != nil {
		return nil, fmt.Errorf("encoding volume size: %w", err)
	}
	volume, err := c.client.api.CreateSandboxVolume(ctx, managementapi.CreateSandboxVolumeParams{TeamId: c.client.teamID()}, managementapi.CreateSandboxVolumeRequest{
		Name:   optional(opts.Name),
		Size:   size,
		Region: optional(opts.Region),
		Labels: labelsToAPI(opts.Labels),
	})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	info, err := volumeInfoFromAPI(volume)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// VolumeGetInfoOptions are the options for [VolumeClient.GetInfo].
type VolumeGetInfoOptions struct {
	// Name is the volume's name. Required.
	Name string
}

// GetInfo gets a volume's record.
func (c *VolumeClient) GetInfo(ctx context.Context, opts VolumeGetInfoOptions) (*VolumeInfo, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	volume, err := c.client.api.GetSandboxVolume(ctx, opts.Name, managementapi.GetSandboxVolumeParams{TeamId: c.client.teamID()})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	info, err := volumeInfoFromAPI(volume)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// VolumeListOptions are the options for [VolumeClient.List].
type VolumeListOptions struct {
	// Query searches volume names.
	Query string

	// Statuses keeps only volumes with one of these statuses.
	Statuses []VolumeStatus

	// PageSize is how many volumes each underlying request fetches. Zero uses
	// the server's default.
	PageSize int
}

// List lists volume records, newest first, fetching further pages as
// iteration reaches them. Records omit labels. An error is yielded as the
// second value and ends the iteration.
func (c *VolumeClient) List(ctx context.Context, opts VolumeListOptions) iter.Seq2[*VolumeInfo, error] {
	return func(yield func(*VolumeInfo, error) bool) {
		params := managementapi.ListSandboxVolumesParams{
			TeamId: c.client.teamID(),
			Q:      optional(opts.Query),
			Limit:  optional(opts.PageSize),
		}
		if opts.Statuses != nil {
			statuses := make([]managementapi.SandboxVolumeStatus, 0, len(opts.Statuses))
			for _, status := range opts.Statuses {
				statuses = append(statuses, managementapi.SandboxVolumeStatus(status))
			}
			params.Status = &statuses
		}
		paginate(yield, "volume list", func(cursor *string) ([]managementapi.SandboxVolume, managementapi.SandboxApiPagination, error) {
			params.Cursor = cursor
			page, err := c.client.api.ListSandboxVolumes(ctx, params)
			if err != nil {
				return nil, managementapi.SandboxApiPagination{}, err
			}
			return page.Items, page.Pagination, nil
		}, volumeInfoFromAPI)
	}
}

// VolumeUpdateOptions are the options for [VolumeClient.Update]. Unset fields
// stay unchanged.
type VolumeUpdateOptions struct {
	// Name is the volume's name. Required.
	Name string

	// Size is the volume's new capacity as a size such as "20Gi", no less
	// than its current capacity. Cannot be combined with SizeMegabytes. Both
	// empty leave the capacity unchanged.
	Size string

	// SizeMegabytes is the volume's new capacity in megabytes, no less than
	// its current capacity. Cannot be combined with Size. Both empty leave
	// the capacity unchanged.
	SizeMegabytes int

	// Labels replace the volume's labels. Nil leaves them unchanged; a
	// non-nil empty map removes them all.
	Labels map[string]string
}

// Update updates a volume and returns its new record.
func (c *VolumeClient) Update(ctx context.Context, opts VolumeUpdateOptions) (*VolumeInfo, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	var size *managementapi.UpdateSandboxVolumeRequest_Size
	var err error
	switch {
	case opts.Size != "" && opts.SizeMegabytes != 0:
		return nil, errors.New("Size and SizeMegabytes cannot both be set")
	case opts.Size != "":
		size = &managementapi.UpdateSandboxVolumeRequest_Size{}
		err = size.FromUpdateSandboxVolumeRequestSize0(opts.Size)
	case opts.SizeMegabytes != 0:
		size = &managementapi.UpdateSandboxVolumeRequest_Size{}
		err = size.FromUpdateSandboxVolumeRequestSize1(opts.SizeMegabytes)
	}
	if err != nil {
		return nil, fmt.Errorf("encoding volume size: %w", err)
	}
	volume, err := c.client.api.UpdateSandboxVolume(ctx, opts.Name, managementapi.UpdateSandboxVolumeParams{TeamId: c.client.teamID()}, managementapi.UpdateSandboxVolumeRequest{
		Size:   size,
		Labels: labelsToAPI(opts.Labels),
	})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	info, err := volumeInfoFromAPI(volume)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// VolumeDeleteOptions are the options for [VolumeClient.Delete].
type VolumeDeleteOptions struct {
	// Name is the volume's name. Required.
	Name string
}

// Delete deletes a volume and its data, and returns its record. Fails while a
// sandbox mounts it. Deletion continues after this returns, so the record
// usually shows the volume as deleting.
func (c *VolumeClient) Delete(ctx context.Context, opts VolumeDeleteOptions) (*VolumeInfo, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	volume, err := c.client.api.DeleteSandboxVolume(ctx, opts.Name, managementapi.DeleteSandboxVolumeParams{TeamId: c.client.teamID()})
	if err != nil {
		return nil, controlPlaneError(err)
	}
	info, err := volumeInfoFromAPI(volume)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

func volumeInfoFromAPI(volume *managementapi.SandboxVolume) (VolumeInfo, error) {
	info := VolumeInfo{
		Name:       deref(volume.Name),
		Region:     volume.Region,
		Status:     VolumeStatus(volume.Status),
		AttachedTo: deref(volume.AttachedTo),
		Labels:     deref(volume.Labels),
		CreatedAt:  deref(volume.CreatedAt),
		UpdatedAt:  deref(volume.UpdatedAt),
		CreatedBy:  deref(volume.CreatedBy),
		UpdatedBy:  deref(volume.UpdatedBy),
	}
	if size, err := volume.Size.AsSandboxVolumeSize0(); err == nil {
		info.Size = size
		return info, nil
	}
	megabytes, err := volume.Size.AsSandboxVolumeSize1()
	if err != nil {
		return VolumeInfo{}, fmt.Errorf("volume size is neither a string nor an integer: %w", err)
	}
	info.SizeMegabytes = megabytes
	return info, nil
}
