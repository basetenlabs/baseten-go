//go:build linux

package volume

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	linuxATRemovedir     = 0x200
	linuxATNoFollow      = 0x100
	linuxRenameNoReplace = 1
	linuxRenameExchange  = 2
)

func linuxFchmodat2Number() uintptr {
	switch runtime.GOARCH {
	case "mips", "mipsle":
		return 4452
	case "mips64", "mips64le":
		return 5452
	default:
		return 452
	}
}

func linuxAtSyscallNumbers() (fstatat, renameat2 uintptr) {
	// syscall intentionally stopped adding wrappers; keep the two missing
	// descriptor-relative calls local instead of adding a root-module dependency.
	switch runtime.GOARCH {
	case "386":
		return 300, 353
	case "amd64":
		return 262, 316
	case "arm":
		return 327, 382
	case "arm64", "loong64", "riscv64":
		return 79, 276
	case "mips", "mipsle":
		return 4293, 4351
	case "mips64", "mips64le":
		return 5252, 5311
	case "ppc64", "ppc64le":
		return 291, 357
	case "s390x":
		return 293, 347
	case "sparc64":
		return 289, 345
	default:
		return 0, 0
	}
}

func openFileAt(parent *os.File, name string, flags int, perm os.FileMode) (*os.File, error) {
	var (
		fd  int
		err error
	)
	for {
		fd, err = syscall.Openat(int(parent.Fd()), name, flags, uint32(perm.Perm()))
		if err != syscall.EINTR {
			break
		}
	}
	runtime.KeepAlive(parent)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func exchangeAt(
	oldParent *os.File,
	oldName string,
	newParent *os.File,
	newName string,
) error {
	_, renameat2 := linuxAtSyscallNumbers()
	if renameat2 == 0 {
		return syscall.ENOSYS
	}
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
			renameat2,
			oldParent.Fd(),
			uintptr(unsafe.Pointer(oldPointer)),
			newParent.Fd(),
			uintptr(unsafe.Pointer(newPointer)),
			linuxRenameExchange,
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

func statAt(parent *os.File, name string) (rootedFileStat, error) {
	fstatat, _ := linuxAtSyscallNumbers()
	if fstatat == 0 {
		return rootedFileStat{}, syscall.ENOSYS
	}
	namePointer, err := syscall.BytePtrFromString(name)
	if err != nil {
		return rootedFileStat{}, err
	}
	var raw syscall.Stat_t
	var errno syscall.Errno
	for {
		_, _, errno = syscall.Syscall6(
			fstatat,
			parent.Fd(),
			uintptr(unsafe.Pointer(namePointer)),
			uintptr(unsafe.Pointer(&raw)),
			linuxATNoFollow,
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
				syscall.SYS_READLINKAT,
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
	var err error
	for {
		err = syscall.Mkdirat(int(parent.Fd()), name, uint32(perm.Perm()))
		if err != syscall.EINTR {
			break
		}
	}
	runtime.KeepAlive(parent)
	return err
}

var linuxFchmodat2NoFollow = callLinuxFchmodat2NoFollow

func chmodAtNoFollow(
	parent *os.File,
	name string,
	mode os.FileMode,
	identity rootedFileStat,
) error {
	if err := linuxFchmodat2NoFollow(parent, name, mode); err == nil {
		return nil
	}
	return chmodOpenedAtNoFollow(parent, name, mode, identity)
}

func callLinuxFchmodat2NoFollow(parent *os.File, name string, mode os.FileMode) error {
	namePointer, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	var errno syscall.Errno
	for {
		_, _, errno = syscall.Syscall6(
			linuxFchmodat2Number(),
			parent.Fd(),
			uintptr(unsafe.Pointer(namePointer)),
			uintptr(mode.Perm()),
			linuxATNoFollow,
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
			syscall.SYS_SYMLINKAT,
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
		flags = linuxATRemovedir
	}
	var errno syscall.Errno
	for {
		_, _, errno = syscall.Syscall6(
			syscall.SYS_UNLINKAT,
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
	if !noReplace {
		var err error
		for {
			err = syscall.Renameat(
				int(oldParent.Fd()),
				oldName,
				int(newParent.Fd()),
				newName,
			)
			if err != syscall.EINTR {
				break
			}
		}
		runtime.KeepAlive(oldParent)
		runtime.KeepAlive(newParent)
		return err
	}
	_, renameat2 := linuxAtSyscallNumbers()
	if renameat2 == 0 {
		return syscall.ENOSYS
	}
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
			renameat2,
			oldParent.Fd(),
			uintptr(unsafe.Pointer(oldPointer)),
			newParent.Fd(),
			uintptr(unsafe.Pointer(newPointer)),
			linuxRenameNoReplace,
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
