//go:build linux || darwin

package volume

import (
	"errors"
	"math"
	"os"
	"syscall"
)

func platformSupportsAtomicPull() bool {
	return true
}

func closePullStateLock(file *os.File) error {
	if file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func lockOpenedPullState(file *os.File) error {
	if err := validatePrivateRegularFile(file, 0o600); err != nil {
		return err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return errPullStateLocked
		}
		if errors.Is(err, syscall.ENOSYS) ||
			errors.Is(err, syscall.ENOTSUP) ||
			errors.Is(err, syscall.EOPNOTSUPP) ||
			errors.Is(err, syscall.ENOLCK) {
			return errPullLockUnsupported
		}
		return err
	}
	return nil
}

func validatePrivateRegularFile(file *os.File, mode uint32) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok ||
		!info.Mode().IsRegular() ||
		stat.Uid != uint32(os.Geteuid()) ||
		stat.Nlink != 1 ||
		uint32(stat.Mode)&0o7777 != mode {
		return syscall.EPERM
	}
	return nil
}

func availableDestinationSpace(path string) (uint64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return 0, err
	}
	availableBlocks := uint64(stats.Bavail)
	blockSize := uint64(stats.Bsize)
	if blockSize != 0 && availableBlocks > math.MaxUint64/blockSize {
		return 0, errDestinationSpaceOverflow
	}
	return availableBlocks * blockSize, nil
}
