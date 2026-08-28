//go:build darwin

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
		changedSeconds: stat.Ctimespec.Sec,
		changedNanos:   stat.Ctimespec.Nsec,
	}
}

func rootedFileStatFromRaw(stat *syscall.Stat_t) rootedFileStat {
	return rootedFileStat{
		mode:     unixFileMode(uint32(stat.Mode)),
		size:     stat.Size,
		modified: time.Unix(stat.Mtimespec.Sec, stat.Mtimespec.Nsec).UnixNano(),
		identity: sourceIdentity{
			available:      true,
			device:         uint64(stat.Dev),
			inode:          stat.Ino,
			changedSeconds: stat.Ctimespec.Sec,
			changedNanos:   stat.Ctimespec.Nsec,
		},
		uid:   stat.Uid,
		nlink: uint64(stat.Nlink),
	}
}
