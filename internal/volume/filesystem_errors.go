package volume

import "errors"

var (
	errPullStateLocked               = errors.New("pull resume state is locked")
	errPullLockUnsupported           = errors.New("safe pull resume locking is unsupported")
	errDestinationSpaceUnsupported   = errors.New("destination free-space inspection is unsupported")
	errDestinationSpaceOverflow      = errors.New("destination free-space value overflows")
	errStagingPublishIdentityChanged = errors.New("publish path identity changed")
)
