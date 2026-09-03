package volume

// PROBE BRANCH ONLY — never merges. Two-arm platform discrimination for the
// Windows scan-instability mechanism behind "Read each entry's metadata
// live, not from the walk's cached copy".
//
// PREMISE UNDER TEST, stated before any run: "a live Lstat is stable across
// reads on Windows". The OLD arm reads metadata the pre-fix way, through
// DirEntry.Info — on Windows the directory enumeration's cached copy, which
// the mechanism says can change between enumerations of an untouched tree.
// The NEW arm reads through os.Lstat, the fix's path.
//
// BASIS, precisely: the read-path mechanism is SOURCE-VERIFIED (Windows
// dirEntry.Info returns the enumeration's cached fileStat with no fresh
// syscall; unix's is a live lstat outside an os.Root with known dirent
// types). The timing and width of the staleness window are UNMEASURED
// ASSUMPTIONS — nobody here has measured them — so the schedule below writes
// the tree fresh and scans immediately, back-to-back once and then with
// sleeps, to give an assumed post-write window several chances without
// claiming to know its shape.
//
// EXPECTED on windows-latest: OLD arm RED (the cache instability observed by
// CI), NEW arm GREEN. On unix both arms are the same call and both green.
// Both arms are CANARIES in nature: the OLD arm fails only when the lazy
// update lands inside its window, so its red is a measurement and its green
// is silence, never safety.
//
// PRE-COMMITTED NULL READING: if BOTH arms stay green on Windows, the
// mechanism did not reproduce under this harness and the result is
// UNMEASURED — it is never read as "fix confirmed". A green suite here
// proves nothing the premise needs; only the OLD arm going red while the
// NEW arm stays green measures the premise. Each arm also logs its own
// reading at runtime, so the meaning sits beside the result in the CI log.
//
// DESIGN: one tree, both readers, interleaved round by round — each round
// reads the same tree through Info and through Lstat adjacently, so the only
// difference between the two arms' observations is the read call, not tree
// or timing. The arms are then evaluated separately over the collected
// rounds. The symlink is optional: its creation failing (likely on Windows
// without Developer Mode) drops it from the tree WITH A LOG and the probe
// runs on — mtime stability of files and directories is the subject, and a
// skipped probe is a non-measurement wearing green: a canary that never
// entered the mine reads identically to one that came back fine. The tree's shape is
// guarded by exact entry counts so a tree that is not what the probe thinks
// fails loudly instead of quietly measuring less.

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// probeRounds is the number of read rounds after the baseline: the first
// follows back-to-back with no sleep (an assumed-narrow window), the rest
// after short sleeps (an assumed-wider one).
const probeRounds = 6

// probeTree writes a fresh natural-times tree: five directories, five files
// including a large one and an empty one, and — where the platform allows —
// one symlink. It returns the root and the exact entry count a walk must
// see, root directory included.
func probeTree(t *testing.T) (string, int) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"a/b", "deep/nest/ed"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(i)
	}
	for path, body := range map[string][]byte{
		"a/one.txt":        []byte("one"),
		"a/b/two.txt":      []byte("two"),
		"deep/nest/ed/big": big,
		"top.txt":          []byte("top"),
		"empty.txt":        nil,
	} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries := 1 + 5 + 5 // the root, five directories, five files
	// The symlink is optional on purpose: it is not the subject, and a
	// platform refusing it must not skip the probe.
	if err := os.Symlink("top.txt", filepath.Join(root, "link")); err != nil {
		t.Logf("probe tree has NO symlink (creation failed: %v); files and directories are still measured", err)
	} else {
		entries++
	}
	return root, entries
}

// readTimes walks the tree once, recording every entry's mtime through the
// given reader.
func readTimes(t *testing.T, root string, read func(abs string, d fs.DirEntry) (fs.FileInfo, error)) map[string]string {
	t.Helper()
	times := map[string]string{}
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := read(abs, d)
		if err != nil {
			return err
		}
		times[abs] = info.ModTime().UTC().Format(time.RFC3339Nano)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return times
}

func viaInfo(_ string, d fs.DirEntry) (fs.FileInfo, error)    { return d.Info() }
func viaLstat(abs string, _ fs.DirEntry) (fs.FileInfo, error) { return os.Lstat(abs) }

// TestProbeArms runs the controlled experiment: one tree, both readers,
// interleaved round by round, evaluated per arm.
func TestProbeArms(t *testing.T) {
	root, wantEntries := probeTree(t)

	type round struct{ old, new map[string]string }
	rounds := make([]round, 0, probeRounds+1)
	// Baseline immediately after the writes — the assumed window opens here.
	rounds = append(rounds, round{readTimes(t, root, viaInfo), readTimes(t, root, viaLstat)})
	for i := 0; i < probeRounds; i++ {
		if i > 0 {
			time.Sleep(150 * time.Millisecond) // round 1 is back-to-back, no sleep
		}
		rounds = append(rounds, round{readTimes(t, root, viaInfo), readTimes(t, root, viaLstat)})
	}

	// The tree-shape guard: a probe measuring fewer entries than it built is
	// a quiet under-measurement, which is the failure mode this probe's own
	// review flagged.
	if got := len(rounds[0].old); got != wantEntries {
		t.Fatalf("the probe walked %d entries, the tree holds %d — measuring the wrong tree", got, wantEntries)
	}
	if got := len(rounds[0].new); got != wantEntries {
		t.Fatalf("the Lstat walk saw %d entries, the tree holds %d", got, wantEntries)
	}

	evaluate := func(t *testing.T, pick func(round) map[string]string, arm string) bool {
		t.Helper()
		stable := true
		base := pick(rounds[0])
		for i := 1; i < len(rounds); i++ {
			for path, want := range base {
				if got := pick(rounds[i])[path]; got != want {
					stable = false
					t.Errorf("round %d: %s read %s then %s across reads of an untouched tree [%s arm]", i, path, want, got, arm)
				}
			}
		}
		return stable
	}

	t.Run("old-arm-enumeration-cache", func(t *testing.T) {
		if evaluate(t, func(r round) map[string]string { return r.old }, "Info") {
			t.Log("OLD arm observed no instability. On Windows this means the " +
				"mechanism did not reproduce under this harness and the probe is " +
				"UNMEASURED — this green is silence, never a confirmation of the fix.")
		} else {
			t.Log("OLD arm red: the enumeration-cache instability reproduced. " +
				"This failure IS the measurement.")
		}
	})
	t.Run("new-arm-live-lstat", func(t *testing.T) {
		if evaluate(t, func(r round) map[string]string { return r.new }, "Lstat") {
			t.Log("NEW arm stable across all rounds. Read alongside the OLD arm: " +
				"old-red/new-green measures the premise; both-green is UNMEASURED.")
		} else {
			t.Log("NEW arm red: a live Lstat was NOT stable across reads — the " +
				"premise under test is false on this platform and the fix needs " +
				"the deterministic-times fallback.")
		}
	})
}

// TestProbeEncodeStably runs the product path end to end on the same
// question: repeated full scans of one untouched tree must encode to
// identical manifest bytes through the fixed scanner, file records included
// — the file entries are built from the scan so their mtimes are inside the
// compared bytes, which is exactly the class CI broke on.
func TestProbeEncodeStably(t *testing.T) {
	root, _ := probeTree(t)

	encode := func(src *Source) string {
		files := make([]FileEntry, 0, len(src.Files))
		for _, f := range src.Files {
			files = append(files, FileEntry{
				Path: f.Path, Mode: f.Mode, Size: f.Size, MTime: f.MTime,
				Kind: FileKindChunk, Chunk: ChunkRef{Length: f.Size},
			})
		}
		return string(EncodeManifest(NewManifest(src, "file:///probe", files)))
	}

	first, err := ScanSource(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Files) != 5 {
		t.Fatalf("the scan found %d files, the tree holds 5 — the encoded comparison would under-measure", len(first.Files))
	}
	baseline := encode(first)
	for i := 0; i < probeRounds; i++ {
		if i > 0 {
			time.Sleep(150 * time.Millisecond)
		}
		next, err := ScanSource(root)
		if err != nil {
			t.Fatal(err)
		}
		if encode(next) != baseline {
			t.Errorf("round %d: the untouched tree encoded to different bytes", i+1)
		}
	}
}
