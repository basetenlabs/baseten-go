//go:build linux

package volume

import (
	"syscall"
	"testing"
	"time"
)

func TestRootedFileStatFromRawPreservesLinuxTimestamps(t *testing.T) {
	stat := &syscall.Stat_t{
		Mode: syscall.S_IFREG | 0o640,
		Size: 17,
	}
	stat.Mtim.Sec = -42
	stat.Mtim.Nsec = 123_456_789
	stat.Ctim.Sec = -17
	stat.Ctim.Nsec = 987_654_321

	got := rootedFileStatFromRaw(stat)
	if got.modified != time.Unix(-42, 123_456_789).UnixNano() {
		t.Fatalf(
			"modified timestamp = %d, want %d",
			got.modified,
			time.Unix(-42, 123_456_789).UnixNano(),
		)
	}
	if got.identity.changedSeconds != -17 || got.identity.changedNanos != 987_654_321 {
		t.Fatalf("change timestamp = %+v", got.identity)
	}
}
