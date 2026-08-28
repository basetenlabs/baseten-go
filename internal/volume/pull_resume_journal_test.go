//go:build linux || darwin

package volume

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPullCheckpointJournalIsDurableAndStrict(t *testing.T) {
	fixture := newPullFixture(t)
	parent := t.TempDir()
	identity, err := newPullIdentity(
		fixture.manifestDigest,
		filepath.Join(parent, "output"),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	first := completedPullChunk{Path: "first", Length: 1, Digest: testDigest(0x61)}
	second := completedPullChunk{Path: "second", Length: 1, Digest: testDigest(0x62)}
	expected := map[completedPullChunk]struct{}{first: {}, second: {}}
	resume, err := fixture.client.openPullResume(parent, identity, expected, false)
	if err != nil {
		t.Fatal(err)
	}
	checkpointBefore, err := os.ReadFile(resume.checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := resume.markCompleted(first); err != nil {
		t.Fatal(err)
	}
	if err := resume.markCompleted(second); err != nil {
		t.Fatal(err)
	}
	checkpointAfter, err := os.ReadFile(resume.checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(checkpointAfter, checkpointBefore) {
		t.Fatal("per-chunk completion rewrote the checkpoint snapshot")
	}
	journalPath := resume.journalPath
	if err := resume.close(); err != nil {
		t.Fatal(err)
	}
	compacted, err := fixture.client.openPullResume(parent, identity, expected, false)
	if err != nil {
		t.Fatal(err)
	}
	if !compacted.contains(first) || !compacted.contains(second) {
		t.Fatal("journal compaction lost completed chunks")
	}
	if err := compacted.close(); err != nil {
		t.Fatal(err)
	}
	journal, err := os.OpenFile(journalPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.WriteString("{\n"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := fixture.client.openPullResume(parent, identity, expected, false)
	if reopened != nil {
		reopened.close()
	}
	if err == nil || !IsCode(err, ErrorIntegrity) {
		t.Fatalf("corrupt journal error = %v, want %s", err, ErrorIntegrity)
	}
}

func TestPullJournalCompactionCrashPointsAreIdempotent(t *testing.T) {
	stages := []string{
		"before-checkpoint-replace",
		"checkpoint-replaced",
		"before-journal-reset",
		"journal-truncated",
		"journal-reset",
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			fixture := newPullFixture(t)
			parent := t.TempDir()
			identity, err := newPullIdentity(
				fixture.manifestDigest,
				filepath.Join(parent, "output"),
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			chunk := completedPullChunk{
				Path: "file", Length: 1, Digest: testDigest(0x79),
			}
			expected := map[completedPullChunk]struct{}{chunk: {}}
			resume, err := fixture.client.openPullResume(parent, identity, expected, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := resume.markCompleted(chunk); err != nil {
				t.Fatal(err)
			}
			journalPath := resume.journalPath
			if err := resume.close(); err != nil {
				t.Fatal(err)
			}

			fired := false
			fixture.client.filesystemHooks = &filesystemTestHooks{
				duringJournalCompaction: func(current string) error {
					if current == stage && !fired {
						fired = true
						return errors.New("injected crash")
					}
					return nil
				},
			}
			crashed, err := fixture.client.openPullResume(parent, identity, expected, false)
			if crashed != nil {
				crashed.close()
			}
			if err == nil || !IsCode(err, ErrorFilesystem) || !fired {
				t.Fatalf("compaction crash = fired %t, error %v", fired, err)
			}

			fixture.client.filesystemHooks = nil
			for attempt := range 3 {
				reopened, err := fixture.client.openPullResume(
					parent,
					identity,
					expected,
					false,
				)
				if err != nil {
					t.Fatalf("reopen %d: %v", attempt, err)
				}
				if !reopened.contains(chunk) {
					reopened.close()
					t.Fatalf("reopen %d lost checkpointed chunk", attempt)
				}
				if err := reopened.close(); err != nil {
					t.Fatalf("close reopen %d: %v", attempt, err)
				}
			}
			info, err := os.Stat(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() != 0 {
				t.Fatalf("compacted journal size = %d, want 0", info.Size())
			}
		})
	}
}

func TestPullJournalStreamingBoundsAndCanonicalRecords(t *testing.T) {
	writeJournal := func(t *testing.T, body []byte) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), pullJournalName)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	long := completedPullChunk{
		Path:   strings.Repeat("p", defaultMaxPortablePathBytes),
		Length: 1,
		Digest: testDigest(0x71),
	}
	longLine, err := json.Marshal(long)
	if err != nil {
		t.Fatal(err)
	}
	longLine = append(longLine, '\n')
	completed := make(map[completedPullChunk]struct{})
	if err := readPullJournal(
		writeJournal(t, longLine),
		uint64(len(longLine)),
		map[completedPullChunk]struct{}{long: {}},
		completed,
	); err != nil {
		t.Fatal(err)
	}
	if _, ok := completed[long]; !ok {
		t.Fatal("large journal record was not restored")
	}
	t.Run("overlong path", func(t *testing.T) {
		overlong := long
		overlong.Path = strings.Repeat("p", defaultMaxPortablePathBytes+1)
		body, err := json.Marshal(overlong)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, '\n')
		err = readPullJournal(
			writeJournal(t, body),
			uint64(len(body)),
			map[completedPullChunk]struct{}{overlong: {}},
			make(map[completedPullChunk]struct{}),
		)
		if err == nil || !IsCode(err, ErrorIntegrity) {
			t.Fatalf("overlong journal path error = %v, want %s", err, ErrorIntegrity)
		}
	})

	chunk := completedPullChunk{Path: "file", Length: 1, Digest: testDigest(0x72)}
	canonical, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	canonical = append(canonical, '\n')
	variants := map[string][]byte{
		"newline dense": bytes.Repeat([]byte{'\n'}, 1<<20),
		"invalid UTF-8": append([]byte{
			0xff,
		}, canonical...),
		"duplicate key": []byte(strings.Replace(
			string(canonical),
			`"path":"file"`,
			`"path":"file","path":"file"`,
			1,
		)),
		"unknown key": []byte(strings.Replace(
			string(canonical),
			`"path":"file"`,
			`"path":"file","unknown":true`,
			1,
		)),
		"reordered fields": []byte(
			`{"offset":0,"path":"file","length":1,"digest":"` +
				chunk.Digest.String() + `"}` + "\n",
		),
		"trailing record": append(append([]byte(nil), canonical...), canonical...),
		"malformed":       []byte("{\n"),
	}
	for name, body := range variants {
		t.Run(name, func(t *testing.T) {
			err := readPullJournal(
				writeJournal(t, body),
				uint64(len(body)),
				map[completedPullChunk]struct{}{chunk: {}},
				make(map[completedPullChunk]struct{}),
			)
			if err == nil || !IsCode(err, ErrorIntegrity) {
				t.Fatalf("journal variant error = %v, want %s", err, ErrorIntegrity)
			}
		})
	}
	t.Run("unterminated tail is ignored", func(t *testing.T) {
		completed := make(map[completedPullChunk]struct{})
		if err := readPullJournal(
			writeJournal(t, canonical[:len(canonical)-1]),
			uint64(len(canonical)),
			map[completedPullChunk]struct{}{chunk: {}},
			completed,
		); err != nil {
			t.Fatal(err)
		}
		if len(completed) != 0 {
			t.Fatalf("unterminated record was replayed: %+v", completed)
		}
	})
	t.Run("checkpoint conflict is rejected", func(t *testing.T) {
		conflict := chunk
		conflict.Digest = testDigest(0x73)
		body, err := json.Marshal(conflict)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, '\n')
		err = readPullJournal(
			writeJournal(t, body),
			uint64(len(body)),
			map[completedPullChunk]struct{}{chunk: {}, conflict: {}},
			map[completedPullChunk]struct{}{chunk: {}},
		)
		if err == nil || !IsCode(err, ErrorIntegrity) {
			t.Fatalf("conflicting journal error = %v, want %s", err, ErrorIntegrity)
		}
	})
	t.Run("unexpected chunk is rejected", func(t *testing.T) {
		unexpected := chunk
		unexpected.Path = "unexpected"
		body, err := json.Marshal(unexpected)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, '\n')
		err = readPullJournal(
			writeJournal(t, body),
			uint64(len(body)),
			map[completedPullChunk]struct{}{chunk: {}},
			make(map[completedPullChunk]struct{}),
		)
		if err == nil || !IsCode(err, ErrorIntegrity) {
			t.Fatalf("unexpected journal error = %v, want %s", err, ErrorIntegrity)
		}
	})
}
