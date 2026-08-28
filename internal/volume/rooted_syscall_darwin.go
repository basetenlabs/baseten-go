//go:build darwin

package volume

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	// Darwin exposes these through libc but not the public syscall package.
	// Direct local wrappers preserve descriptor anchoring without a dependency.
	darwinSysOpenat      = 463
	darwinSysRenameat    = 465
	darwinSysFchmodat    = 467
	darwinSysFstatat64   = 470
	darwinSysUnlinkat    = 472
	darwinSysReadlinkat  = 473
	darwinSysSymlinkat   = 474
	darwinSysMkdirat     = 475
	darwinSysRenameatxNP = 488

	darwinATNoFollow        = 0x0020
	darwinATRemovedir       = 0x0080
	darwinRenameExcl        = 0x00000004
	rootedDarwinRenameSwap  = 0x00000002
	darwinRenameNoFollowAny = 0x00000010
)

func openFileAt(parent *os.File, name string, flags int, perm os.FileMode) (*os.File, error) {
	namePointer, err := syscall.BytePtrFromString(name)
	if err != nil {
		return nil, err
	}
	var (
		fd    uintptr
		errno syscall.Errno
	)
	for {
		fd, _, errno = syscall.Syscall6(
			darwinSysOpenat,
			parent.Fd(),
			uintptr(unsafe.Pointer(namePointer)),
			uintptr(flags),
			uintptr(perm.Perm()),
			0,
			0,
		)
		if errno != syscall.EINTR {
			break
		}
	}
	runtime.KeepAlive(parent)
	runtime.KeepAlive(namePointer)
	if errno != 0 {
		return nil, errno
	}
	return os.NewFile(fd, name), nil
}

func statAt(parent *os.File, name string) (rootedFileStat, error) {
	namePointer, err := syscall.BytePtrFromString(name)
	if err != nil {
		return rootedFileStat{}, err
	}
	var raw syscall.Stat_t
	var errno syscall.Errno
	for {
		_, _, errno = syscall.Syscall6(
			darwinSysFstatat64,
			parent.Fd(),
			uintptr(unsafe.Pointer(namePointer)),
			uintptr(unsafe.Pointer(&raw)),
			darwinATNoFollow,
			0,
			0,
		)
		if errno != syscall.EINTR {
			break
		}
	}
	runtime.KeepAlive(parent)
	runtime.KeepAlive(namePointer)
	runtime.KeepAlive(&raw)
	if errno != 0 {
		return rootedFileStat{}, errno
	}
	return rootedFileStatFromRaw(&raw), nil
}

func readlinkAt(parent *os.File, name string) (string, error) {
	namePointer, err := syscall.BytePtrFromString(name)
	if err != nil {
		return "", err
	}
	for size := 128; size <= 1<<20; size *= 2 {
		buffer := make([]byte, size)
		var (
			count uintptr
			errno syscall.Errno
		)
		for {
			count, _, errno = syscall.Syscall6(
				darwinSysReadlinkat,
				parent.Fd(),
				uintptr(unsafe.Pointer(namePointer)),
				uintptr(unsafe.Pointer(&buffer[0])),
				uintptr(len(buffer)),
				0,
				0,
			)
			if errno != syscall.EINTR {
				break
			}
		}
		runtime.KeepAlive(parent)
		runtime.KeepAlive(namePointer)
		runtime.KeepAlive(buffer)
		if errno != 0 {
			return "", errno
		}
		if count < uintptr(len(buffer)) {
			return string(buffer[:count]), nil
		}
	}
	return "", syscall.ENAMETOOLONG
}

func mkdirAt(parent *os.File, name string, perm os.FileMode) error {
	namePointer, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	var errno syscall.Errno
	for {
		_, _, errno = syscall.Syscall6(
			darwinSysMkdirat,
			parent.Fd(),
			uintptr(unsafe.Pointer(namePointer)),
			uintptr(perm.Perm()),
			0,
			0,
			0,
		)
		if errno != syscall.EINTR {
			break
		}
	}
	runtime.KeepAlive(parent)
	runtime.KeepAlive(namePointer)
	if errno != 0 {
		return errno
	}
	return nil
}

func chmodAtNoFollow(
	parent *os.File,
	name string,
	mode os.FileMode,
	identity rootedFileStat,
) error {
	namePointer, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	var errno syscall.Errno
	for {
		_, _, errno = syscall.Syscall6(
			darwinSysFchmodat,
			parent.Fd(),
			uintptr(unsafe.Pointer(namePointer)),
			uintptr(mode.Perm()),
			darwinATNoFollow,
			0,
			0,
		)
		if errno != syscall.EINTR {
			break
		}
	}
	runtime.KeepAlive(parent)
	runtime.KeepAlive(namePointer)
	if errno == 0 {
		return nil
	}
	return chmodOpenedAtNoFollow(parent, name, mode, identity)
}

func symlinkAt(target string, parent *os.File, name string) error {
	targetPointer, err := syscall.BytePtrFromString(target)
	if err != nil {
		return err
	}
	namePointer, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	var errno syscall.Errno
	for {
		_, _, errno = syscall.Syscall6(
			darwinSysSymlinkat,
			uintptr(unsafe.Pointer(targetPointer)),
			parent.Fd(),
			uintptr(unsafe.Pointer(namePointer)),
			0,
			0,
			0,
		)
		if errno != syscall.EINTR {
			break
		}
	}
	runtime.KeepAlive(parent)
	runtime.KeepAlive(targetPointer)
	runtime.KeepAlive(namePointer)
	if errno != 0 {
		return errno
	}
	return nil
}

func unlinkAt(parent *os.File, name string, directory bool) error {
	namePointer, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	flags := uintptr(0)
	if directory {
		flags = darwinATRemovedir
	}
	var errno syscall.Errno
	for {
		_, _, errno = syscall.Syscall6(
			darwinSysUnlinkat,
			parent.Fd(),
			uintptr(unsafe.Pointer(namePointer)),
			flags,
			0,
			0,
			0,
		)
		if errno != syscall.EINTR {
			break
		}
	}
	runtime.KeepAlive(parent)
	runtime.KeepAlive(namePointer)
	if errno != 0 {
		return errno
	}
	return nil
}

func renameAt(
	oldParent *os.File,
	oldName string,
	newParent *os.File,
	newName string,
	noReplace bool,
) error {
	oldPointer, err := syscall.BytePtrFromString(oldName)
	if err != nil {
		return err
	}
	newPointer, err := syscall.BytePtrFromString(newName)
	if err != nil {
		return err
	}
	syscallNumber := uintptr(darwinSysRenameat)
	flags := uintptr(0)
	if noReplace {
		syscallNumber = darwinSysRenameatxNP
		flags = darwinRenameExcl | darwinRenameNoFollowAny
	}
	var errno syscall.Errno
	for {
		_, _, errno = syscall.Syscall6(
			syscallNumber,
			oldParent.Fd(),
			uintptr(unsafe.Pointer(oldPointer)),
			newParent.Fd(),
			uintptr(unsafe.Pointer(newPointer)),
			flags,
			0,
		)
		if errno != syscall.EINTR {
			break
		}
	}
	runtime.KeepAlive(oldParent)
	runtime.KeepAlive(newParent)
	runtime.KeepAlive(oldPointer)
	runtime.KeepAlive(newPointer)
	if errno != 0 {
		return errno
	}
	return nil
}

func exchangeAt(
	oldParent *os.File,
	oldName string,
	newParent *os.File,
	newName string,
) error {
	oldPointer, err := syscall.BytePtrFromString(oldName)
	if err != nil {
		return err
	}
	newPointer, err := syscall.BytePtrFromString(newName)
	if err != nil {
		return err
	}
	var errno syscall.Errno
	for {
		_, _, errno = syscall.Syscall6(
			darwinSysRenameatxNP,
			oldParent.Fd(),
			uintptr(unsafe.Pointer(oldPointer)),
			newParent.Fd(),
			uintptr(unsafe.Pointer(newPointer)),
			rootedDarwinRenameSwap|darwinRenameNoFollowAny,
			0,
		)
		if errno != syscall.EINTR {
			break
		}
	}
	runtime.KeepAlive(oldParent)
	runtime.KeepAlive(newParent)
	runtime.KeepAlive(oldPointer)
	runtime.KeepAlive(newPointer)
	if errno != 0 {
		return errno
	}
	return nil
}
