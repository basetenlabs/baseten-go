//go:build unix

package volume

import (
	"io/fs"
	"syscall"
)

// FileIdentity reports the device and inode behind info, when the platform
// exposes them. It is the identity half of the scan's one Lstat: the same
// call that records the modification time also records which inode it
// described, so the push can later tell "this file changed" from "this path
// now names a different file".
func FileIdentity(info fs.FileInfo) (dev, ino uint64, ok bool) {
	st, isUnix := info.Sys().(*syscall.Stat_t)
	if !isUnix {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}
