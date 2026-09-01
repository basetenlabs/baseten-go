package separatemoduletests_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// platformMode maps the mode a unix tree carries to the mode this platform's
// filesystem reports for it — after a scan or after materialization; the two
// see the same thing. On unix it is the identity, so every assertion built on
// it keeps its full strength there. On Windows there are no unix permission
// bits: the platform reports every directory as 0777 and a file as 0666, or
// 0444 when the read-only flag is set — which is the one bit chmod can move
// there, so a recorded mode without owner-write reads back 0444. The scanner
// records what the platform reports (entryMode takes Perm() verbatim), so
// asserting these values on Windows is asserting the documented contract,
// not loosening the unix one.
func platformMode(recorded uint16, dir bool) uint16 {
	if runtime.GOOS != "windows" {
		return recorded
	}
	if dir {
		return 0o777
	}
	if recorded&0o200 == 0 {
		return 0o444
	}
	return 0o666
}

// platformModeString renders platformMode the way treeDescription prints
// modes, for building expected tree listings.
func platformModeString(recorded uint16, dir bool) string {
	s := strconv.FormatUint(uint64(platformMode(recorded, dir)), 8)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}

// requireExpressibleModes probes whether this filesystem can express the
// modes the captured fixture records, by setting one and reading it back.
// The capture's byte identity holds only where modes round-trip; where they
// cannot, the premise of comparing against unix-recorded bytes does not
// exist, and the skip states a measured inapplicability rather than a guess.
// The decode-only capture tests carry the platform's coverage either way.
func requireExpressibleModes(t *testing.T) {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("m"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(probe, 0o600); err != nil {
		t.Skipf("cannot chmod a probe file: %v", err)
	}
	info, err := os.Stat(probe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Skipf("this filesystem cannot express the fixture's modes: chmod 0600 read back %04o", info.Mode().Perm())
	}
}
