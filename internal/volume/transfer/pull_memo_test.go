package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
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
	p.rememberDir(".")

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

// TestEnsureParentSharesOneWalkAcrossWaiters pins the memo's concurrency
// contract: callers that arrive together share one MkdirAll through the
// entry's Once, and every one of them reads that single walk's outcome. The
// proof uses the error side, where sharing is observable: a consumed entry
// carrying an error is handed back verbatim, without a walk of its own —
// shown by the directory staying absent even though a walk would create it.
func TestEnsureParentSharesOneWalkAcrossWaiters(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	p := &puller{root: root}

	sentinel := errors.New("the one shared walk's outcome")
	consumed := &dirWalk{}
	consumed.once.Do(func() { consumed.err = sentinel })
	p.madeDirs.Store("a", consumed)

	if err := p.ensureParent(filepath.Join("a", "file.bin")); !errors.Is(err, sentinel) {
		t.Fatalf("a waiter got %v, want the stored walk's own error", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Fatal("the waiter walked anyway: the directory exists")
	}

	// The failed entry was dropped, so the next caller is a fresh walk that
	// succeeds — the failure was propagated, never remembered as done.
	if err := p.ensureParent(filepath.Join("a", "file.bin")); err != nil {
		t.Fatalf("the retry after the dropped failure still failed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "a")); err != nil {
		t.Fatalf("the retry did not create the directory: %v", err)
	}

	// And under real concurrency: many first-files under one new parent all
	// succeed, exercising the LoadOrStore/Once pair under the race detector.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.ensureParent(filepath.Join("b", "c", "file.bin")); err != nil {
				t.Errorf("concurrent ensureParent failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if _, err := os.Lstat(filepath.Join(dir, "b", "c")); err != nil {
		t.Fatalf("the shared parent was not created: %v", err)
	}
}
