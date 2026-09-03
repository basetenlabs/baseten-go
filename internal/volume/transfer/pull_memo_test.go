package transfer

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureParentMemoizesSuccessesOnly pins the memo's three rules: a
// remembered parent short-circuits the walk, only successful creates are
// remembered, and a failure retries rather than being cached as done.
func TestEnsureParentMemoizesSuccessesOnly(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	p := &puller{root: root}
	p.madeDirs.Store(".", struct{}{})

	// A top-level file's parent is ".", seeded: no create happens and none
	// is needed.
	if err := p.ensureParent("top.txt"); err != nil {
		t.Fatal(err)
	}

	// An implicit parent is created once and remembered.
	name := filepath.Join("a", "b", "file.bin")
	if err := p.ensureParent(name); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.madeDirs.Load(filepath.Dir(name)); !ok {
		t.Fatal("a successful create was not remembered")
	}
	// The memo short-circuits: with the directory gone, a remembered parent
	// returns nil without walking — which is the memo's contract, proven by
	// the directory staying absent.
	if err := os.RemoveAll(filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	if err := p.ensureParent(name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Fatal("the memoized path walked anyway")
	}

	// A failed create is NOT remembered: a file squatting on the parent path
	// fails the create, and once it is gone the retry succeeds.
	if err := os.WriteFile(filepath.Join(dir, "c"), []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join("c", "x")
	if err := p.ensureParent(blocked); err == nil {
		t.Fatal("creating a directory over a file should fail")
	}
	if _, ok := p.madeDirs.Load("c"); ok {
		t.Fatal("a FAILED create was remembered as done")
	}
	if err := os.Remove(filepath.Join(dir, "c")); err != nil {
		t.Fatal(err)
	}
	if err := p.ensureParent(blocked); err != nil {
		t.Fatalf("the retry after the obstacle was removed still failed: %v", err)
	}
}
