//go:build linux || darwin

package volume

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestStalePullInitializationCleanupValidatesExactScaffold(t *testing.T) {
	type scaffold struct {
		fixture     pullFixture
		parent      string
		destination string
		path        string
	}
	createScaffold := func(t *testing.T, stage int) scaffold {
		t.Helper()
		fixture := newPullFixture(t)
		parent := t.TempDir()
		destination := filepath.Join(parent, "output")
		preflight, err := preflightDestination(destination)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := newPullIdentity(fixture.manifestDigest, preflight.destination, nil)
		preflight.close()
		if err != nil {
			t.Fatal(err)
		}
		key, err := fixture.client.pullStorageKey(identity)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(
			parent,
			pullInitPrefix+key+"-22222222222222222222222222222222"+pullInitSuffix,
		)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if stage >= 1 {
			body, err := json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, pullIdentityName), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if stage >= 2 {
			checkpoint, err := initialPullCheckpoint(identity)
			if err != nil {
				t.Fatal(err)
			}
			staleCheckpointTime := uint64(
				time.Now().Add(-pullStaleAfter - time.Hour).Unix(),
			)
			checkpoint.CreatedAtUnixSeconds = staleCheckpointTime
			checkpoint.UpdatedAtUnixSeconds = staleCheckpointTime
			body, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, pullCheckpointName), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if stage >= 3 {
			if err := os.WriteFile(filepath.Join(path, pullJournalName), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if stage >= 4 {
			if err := os.WriteFile(filepath.Join(path, pullLockName), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if stage >= 5 {
			if err := os.Mkdir(filepath.Join(path, pullDataName), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		return scaffold{
			fixture: fixture, parent: parent, destination: destination, path: path,
		}
	}
	ageScaffold := func(t *testing.T, state scaffold) {
		t.Helper()
		stale := time.Now().Add(-pullStaleAfter - time.Hour)
		entries, err := os.ReadDir(state.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if err := os.Chtimes(filepath.Join(state.path, entry.Name()), stale, stale); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(state.path, stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	pullAndRequirePresent := func(t *testing.T, state scaffold) {
		t.Helper()
		if _, err := state.fixture.client.Pull(
			t.Context(),
			state.fixture.options(state.destination),
		); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(state.path); err != nil {
			t.Fatalf("protected initialization scaffold was removed: %v", err)
		}
	}

	for stage, name := range []string{
		"empty",
		"identity",
		"checkpoint",
		"journal",
		"lock",
		"data",
	} {
		t.Run("removes "+name, func(t *testing.T) {
			state := createScaffold(t, stage)
			ageScaffold(t, state)
			if _, err := state.fixture.client.Pull(
				t.Context(),
				state.fixture.options(state.destination),
			); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(state.path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("stale %s scaffold remains: %v", name, err)
			}
		})
	}

	t.Run("keeps new scaffold", func(t *testing.T) {
		pullAndRequirePresent(t, createScaffold(t, 3))
	})

	t.Run("keeps unexpected scaffold", func(t *testing.T) {
		state := createScaffold(t, 2)
		if err := os.WriteFile(filepath.Join(state.path, "unexpected"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ageScaffold(t, state)
		pullAndRequirePresent(t, state)
	})

	t.Run("keeps unsafe modes", func(t *testing.T) {
		state := createScaffold(t, 1)
		if err := os.Chmod(filepath.Join(state.path, pullIdentityName), 0o644); err != nil {
			t.Fatal(err)
		}
		ageScaffold(t, state)
		pullAndRequirePresent(t, state)
	})

	t.Run("keeps active locked scaffold", func(t *testing.T) {
		state := createScaffold(t, 4)
		ageScaffold(t, state)
		lock, err := os.OpenFile(filepath.Join(state.path, pullLockName), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			lock.Close()
			t.Fatal(err)
		}
		defer func() {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
		}()
		pullAndRequirePresent(t, state)
	})
}
