//go:build unix

package separatemoduletests_test

import (
	"time"

	"golang.org/x/sys/unix"
)

// setSymlinkTime stamps the link itself, which os.Chtimes cannot: it follows
// the link and would stamp the target. The capture's byte identity includes
// the link's own recorded time, so rebuilding the tree needs lutimes.
func setSymlinkTime(path string, mtime time.Time) error {
	tv := unix.NsecToTimeval(mtime.UnixNano())
	return unix.Lutimes(path, []unix.Timeval{tv, tv})
}
