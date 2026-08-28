//go:build linux

package volume

import (
	"io/fs"
	"syscall"
	"time"
)

func sourceIdentityFor(info fs.FileInfo) sourceIdentity {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return sourceIdentity{}
	}
	return sourceIdentity{
		available:      true,
		device:         uint64(stat.Dev),
		inode:          stat.Ino,
		changedSeconds: int64(stat.Ctim.Sec),
		changedNanos:   int64(stat.Ctim.Nsec),
	}
}

func rootedFileStatFromRaw(stat *syscall.Stat_t) rootedFileStat {
	return rootedFileStat{
		mode:     unixFileMode(stat.Mode),
		size:     stat.Size,
		modified: time.Unix(int64(stat.Mtim.Sec), int64(stat.Mtim.Nsec)).UnixNano(),
		identity: sourceIdentity{
			available:      true,
			device:         uint64(stat.Dev),
			inode:          stat.Ino,
			changedSeconds: int64(stat.Ctim.Sec),
			changedNanos:   int64(stat.Ctim.Nsec),
		},
		uid:   stat.Uid,
		nlink: uint64(stat.Nlink),
	}
}
