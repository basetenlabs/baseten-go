//go:build linux || darwin

package volume

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPullResumeInitializationIsCrashAtomic(t *testing.T) {
	t.Run("failure before rename leaves no deterministic state", func(t *testing.T) {
		fixture := newPullFixture(t)
		parent := t.TempDir()
		destination := filepath.Join(parent, "output")
		fixture.client.filesystemHooks = &filesystemTestHooks{
			duringResumeInitialize: func(stage string) error {
				if stage == "scaffolding-synced" {
					return errors.New("injected initialization failure")
				}
				return nil
			},
		}
		_, err := fixture.client.Pull(t.Context(), fixture.options(destination))
		if err == nil || !IsCode(err, ErrorFilesystem) {
			t.Fatalf("initialization failure = %v, want %s", err, ErrorFilesystem)
		}
		if entries := mustReadDir(t, parent); len(entries) != 0 {
			t.Fatalf("failed initialization retained state: %+v", entries)
		}
		fixture.client.filesystemHooks = nil
		if _, err := fixture.client.Pull(t.Context(), fixture.options(destination)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("legacy incomplete deterministic state is recovered", func(t *testing.T) {
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
		incomplete := filepath.Join(parent, pullStagingPrefix+key+pullStagingSuffix)
		if err := os.Mkdir(incomplete, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.client.Pull(t.Context(), fixture.options(destination)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("orphan temporary sibling does not poison default pull", func(t *testing.T) {
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
		orphan := filepath.Join(
			parent,
			pullInitPrefix+key+"-00000000000000000000000000000000"+pullInitSuffix,
		)
		if err := os.Mkdir(orphan, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.client.Pull(t.Context(), fixture.options(destination)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("stale orphan temporary sibling is cleaned within bounds", func(t *testing.T) {
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
		orphan := filepath.Join(
			parent,
			pullInitPrefix+key+"-11111111111111111111111111111111"+pullInitSuffix,
		)
		if !validPullInitName(filepath.Base(orphan)) {
			t.Fatalf("test orphan name is not a valid pull initialization name")
		}
		if err := os.Mkdir(orphan, 0o700); err != nil {
			t.Fatal(err)
		}
		stale := time.Now().Add(-pullStaleAfter - time.Hour)
		if err := os.Chtimes(orphan, stale, stale); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.client.Pull(t.Context(), fixture.options(destination)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(orphan); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stale temporary pull state remains: %v", err)
		}
	})
}
