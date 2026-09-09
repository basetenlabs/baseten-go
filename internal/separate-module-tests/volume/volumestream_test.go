package volume_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/transfer"
)

// streamOptions is pullOptions with the destination replaced by a handler.
func streamOptions(
	f *fakeService, handler func(context.Context, volume.Entry, io.Reader) error,
) transfer.PullOptions {
	opts := pullOptions("", f)
	opts.DestDir = ""
	opts.EntryHandler = handler
	return opts
}

// TestStreamDeliversEveryEntryInOrder is the whole point of handing entries to
// a handler rather than only files: a consumer reproducing the tree somewhere
// that is not a filesystem needs the directories and the symlink too, and
// needs a parent before anything under it.
func TestStreamDeliversEveryEntryInOrder(t *testing.T) {
	_, fake := pushFixture(t)

	var lines []string
	_, err := transfer.Pull(context.Background(), fake.client(t),
		streamOptions(fake, func(_ context.Context, entry volume.Entry, r io.Reader) error {
			body, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			lines = append(lines, fmt.Sprintf("%s %s %d", kindName(entry.Kind), entry.Path, len(body)))
			return nil
		}))
	if err != nil {
		t.Fatal(err)
	}

	// Canonical path order, which is what the wire carried: every entry
	// follows its parent and a subtree is contiguous.
	want := strings.Join([]string{
		"dir assets 0",
		"file assets/read-only.txt 6",
		"file empty.txt 0",
		"dir nested 0",
		"dir nested/cache 0",
		"dir nested/deep 0",
		fmt.Sprintf("file nested/deep/data.bin %d", volume.ChunkSize*2+1024),
		"file nested/dup.txt 12",
		"link nested/link 0",
		"file small.txt 12",
	}, "\n")
	if got := strings.Join(lines, "\n"); got != want {
		t.Errorf("entries differ\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestStreamReleasesItsByteBudget pins the accounting that bounds resident
// chunk data. The budget is one chunk, and the multi-chunk file needs three,
// so every chunk after the first can only be fetched once its predecessor has
// handed the budget back. A leak here does not corrupt anything: it blocks
// forever, which is what the deadline turns into a failure.
func TestStreamReleasesItsByteBudget(t *testing.T) {
	_, fake := pushFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opts := streamOptions(fake, func(_ context.Context, _ volume.Entry, r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	})
	opts.Concurrency = volume.Concurrency{MaxBytesInFlight: volume.ChunkSize}

	result, err := transfer.Pull(ctx, fake.client(t), opts)
	if err != nil {
		t.Fatalf("a budget of one chunk should stream a three-chunk file: %v", err)
	}
	if result.ChunksFetched != 6 {
		t.Errorf("fetched %d chunks, want 6", result.ChunksFetched)
	}
}

// TestStreamReleasesWhenTheHandlerStopsEarly covers the handler that reads
// nothing, or stops partway: the buffer and the budget are the stream's to
// hand back, not the handler's, and there is nothing for a handler to close.
func TestStreamReleasesWhenTheHandlerStopsEarly(t *testing.T) {
	_, fake := pushFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, test := range []struct {
		name string
		read func(io.Reader) error
	}{
		{"reads nothing", func(io.Reader) error { return nil }},
		{"reads one byte", func(r io.Reader) error {
			_, err := r.Read(make([]byte, 1))
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := streamOptions(fake, func(_ context.Context, _ volume.Entry, r io.Reader) error {
				return test.read(r)
			})
			opts.Concurrency = volume.Concurrency{MaxBytesInFlight: volume.ChunkSize}
			if _, err := transfer.Pull(ctx, fake.client(t), opts); err != nil {
				t.Fatalf("an undrained stream should still release: %v", err)
			}
		})
	}
}

// TestStreamReadsEmptyForEntriesWithoutBytes pins the reader a directory and a
// symlink get: not nil, so a handler that copies before switching on the kind
// produces nothing rather than dereferencing nil.
func TestStreamReadsEmptyForEntriesWithoutBytes(t *testing.T) {
	_, fake := pushFixture(t)

	checked := 0
	_, err := transfer.Pull(context.Background(), fake.client(t),
		streamOptions(fake, func(_ context.Context, entry volume.Entry, r io.Reader) error {
			if entry.Kind == volume.EntryKindFile {
				return nil
			}
			if r == nil {
				return fmt.Errorf("%s has a nil reader", entry.Path)
			}
			body, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			if len(body) != 0 {
				return fmt.Errorf("%s yielded %d bytes", entry.Path, len(body))
			}
			checked++
			return nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	// Four directories and the symlink.
	if checked != 5 {
		t.Errorf("checked %d entries without bytes, want 5", checked)
	}
}

// TestStreamHandlerErrorStopsThePull covers the handler refusing an entry,
// which is how a caller that wants exactly one file rejects a ref that named
// a directory.
func TestStreamHandlerErrorStopsThePull(t *testing.T) {
	_, fake := pushFixture(t)

	sentinel := errors.New("not a regular file")
	delivered := 0
	_, err := transfer.Pull(context.Background(), fake.client(t),
		streamOptions(fake, func(_ context.Context, entry volume.Entry, _ io.Reader) error {
			delivered++
			if entry.Kind != volume.EntryKindFile {
				return sentinel
			}
			return nil
		}))
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the handler's own error", err)
	}
	// "assets" is the first entry in canonical order, so nothing else ran.
	if delivered != 1 {
		t.Errorf("delivered %d entries after a refusal, want 1", delivered)
	}
}

// TestStreamEntryPathsBuildRefs pins why a public entry path is
// slash-prefixed: assigning one to a ref's path must produce the same value
// parsing that ref from text would, so a listed entry can be addressed
// without anyone assembling a separator by hand.
func TestStreamEntryPathsBuildRefs(t *testing.T) {
	_, fake := pushFixture(t)
	var exchanges atomic.Int64
	api, err := client.NewManagementClient(client.ManagementClientOptions{
		APIKey:  "api-key",
		BaseURL: newManagementAPI(t, fake, &exchanges),
	})
	if err != nil {
		t.Fatal(err)
	}

	base := client.VolumeRef{Namespace: fakeNamespace, Volume: fakeVolume}
	checked := 0
	_, err = api.PullVolume(context.Background(), client.PullVolumeOptions{
		Ref:    base,
		Hasher: newBlake3,
		Store:  fakePublicStore{download: fake.downloader()},
		EntryHandler: func(_ context.Context, entry client.VolumePulledEntry) error {
			ref := base
			ref.Path = entry.Path

			parsed, err := client.ParseVolumeRef(ref.String())
			if err != nil {
				return fmt.Errorf("%s rendered %q, which does not parse: %w", entry.Path, ref, err)
			}
			if parsed != ref {
				return fmt.Errorf("%s: parsed %#v, assigned %#v", entry.Path, parsed, ref)
			}
			checked++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked != 10 {
		t.Errorf("checked %d entries, want all 10", checked)
	}
}

func kindName(kind volume.EntryKind) string {
	switch kind {
	case volume.EntryKindFile:
		return "file"
	case volume.EntryKindDirectory:
		return "dir"
	case volume.EntryKindSymlink:
		return "link"
	}
	return "unknown"
}
