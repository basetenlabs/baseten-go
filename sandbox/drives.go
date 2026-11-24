package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/basetenlabs/baseten-go/client/sandboxapi"
)

// DriveService mounts drives in a running sandbox. Drives are created and
// managed with [client.DriveClient]. Get it from [Sandbox.Drives].
//
// Drives are in pre-release and may change.
type DriveService struct {
	sandbox *Sandbox
}

func newDriveService(sandbox *Sandbox) *DriveService {
	return &DriveService{sandbox: sandbox}
}

// DriveMount is a drive mounted in a sandbox.
type DriveMount struct {
	// DriveName is the name of the mounted drive.
	DriveName string

	// DrivePath is the folder of the drive that is mounted, "/" for the whole
	// drive.
	DrivePath string

	// MountPath is where the drive is mounted in the sandbox.
	MountPath string

	// ReadOnly is whether the mount is read-only.
	ReadOnly bool
}

// DriveMountOptions are the options for [DriveService.Mount].
type DriveMountOptions struct {
	// Name is the name of the drive to mount, in the sandbox's region.
	// Required.
	Name string

	// MountPath is the absolute path in the sandbox to mount the drive at,
	// such as "/mnt/data". Required.
	MountPath string

	// DrivePath is the folder of the drive to mount. Empty mounts the whole
	// drive.
	DrivePath string

	// ReadOnly mounts the drive read-only.
	ReadOnly bool
}

// Mount mounts a drive in the sandbox. The mount lasts until it is unmounted,
// and survives standby.
func (d *DriveService) Mount(ctx context.Context, opts DriveMountOptions) (*DriveMount, error) {
	if opts.Name == "" {
		return nil, errors.New("Name is required")
	}
	if opts.MountPath == "" {
		return nil, errors.New("MountPath is required")
	}
	// Not retried, since a repeat of a mount whose response was lost would
	// fail for a path that is already mounted.
	mounted, err := d.sandbox.api.PostDrivesMount(ctx, sandboxapi.DriveMountRequest{
		DriveName: opts.Name,
		MountPath: opts.MountPath,
		DrivePath: optional(opts.DrivePath),
		ReadOnly:  optional(opts.ReadOnly),
	})
	if err != nil {
		return nil, sandboxError(err)
	}
	if mounted.Success != nil && !*mounted.Success {
		return nil, fmt.Errorf("mounting drive %s at %s failed: %s", opts.Name, opts.MountPath, deref(mounted.Message))
	}
	return &DriveMount{
		DriveName: deref(mounted.DriveName),
		DrivePath: deref(mounted.DrivePath),
		MountPath: deref(mounted.MountPath),
		ReadOnly:  deref(mounted.ReadOnly),
	}, nil
}

// DriveUnmountOptions are the options for [DriveService.Unmount].
type DriveUnmountOptions struct {
	// MountPath is the path in the sandbox the drive is mounted at. Required.
	MountPath string
}

// Unmount unmounts a drive from the sandbox. The drive and its data are kept.
func (d *DriveService) Unmount(ctx context.Context, opts DriveUnmountOptions) error {
	if opts.MountPath == "" {
		return errors.New("MountPath is required")
	}
	// Not retried, since a repeat of an unmount whose response was lost would
	// fail for a path that is no longer mounted.
	unmounted, err := d.sandbox.api.DeleteDrivesMount(ctx, opts.MountPath)
	if err != nil {
		return sandboxError(err)
	}
	if unmounted.Success != nil && !*unmounted.Success {
		return fmt.Errorf("unmounting the drive at %s failed: %s", opts.MountPath, deref(unmounted.Message))
	}
	return nil
}

// ListMounts lists the drives mounted in the sandbox. The sandbox's record
// does not report its mounts, so this is the only way to find them.
func (d *DriveService) ListMounts(ctx context.Context) ([]DriveMount, error) {
	return retryIdempotent(ctx, d.sandbox.retries.ReadMaxRetries, d.sandbox.retries.GatewayMaxRetries,
		func() ([]DriveMount, error) {
			listed, err := d.sandbox.api.GetDrivesMount(ctx)
			if err != nil {
				return nil, sandboxError(err)
			}
			mounts := make([]DriveMount, 0, len(deref(listed.Mounts)))
			for _, mount := range deref(listed.Mounts) {
				mounts = append(mounts, DriveMount{
					DriveName: deref(mount.DriveName),
					DrivePath: deref(mount.DrivePath),
					MountPath: deref(mount.MountPath),
					ReadOnly:  deref(mount.ReadOnly),
				})
			}
			return mounts, nil
		})
}
