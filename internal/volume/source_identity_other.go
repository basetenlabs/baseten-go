//go:build !unix

package volume

import "io/fs"

// FileIdentity reports nothing here: the identity pin is a unix-only soft
// strengthening, matching the producers' own unix-only form, and the size
// re-check is what holds everywhere.
func FileIdentity(fs.FileInfo) (dev, ino uint64, ok bool) {
	return 0, 0, false
}
