//go:build !linux && !darwin

package volume

import (
	"errors"
	"os"
)

var errAtomicPullUnsupported = errors.New("atomic volume pull is unsupported on this platform")

func platformSupportsAtomicPull() bool {
	return false
}

func closePullStateLock(file *os.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func lockOpenedPullState(_ *os.File) error {
	return errPullLockUnsupported
}

func availableDestinationSpace(_ string) (uint64, error) {
	return 0, errDestinationSpaceUnsupported
}

func atomicPublishDirectory(
	_ *rootedDirectory,
	_ string,
	_ rootedFileStat,
	_ *rootedDirectory,
	_ string,
	_ bool,
	_ rootedFileStat,
) (bool, error) {
	return false, errAtomicPullUnsupported
}
