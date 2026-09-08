package volume_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
	"github.com/basetenlabs/baseten-go/internal/volume/transfer"
)

// manifestOptions is what a manifest read of the fixture volume takes, with no
// filter: the seams a caller supplies and nothing else.
func manifestOptions(f *fakeService) transfer.ManifestOptions {
	return transfer.ManifestOptions{
		Ref:            bdn.ResolveRequest{Namespace: fakeNamespace, Volume: fakeVolume},
		NewHasher:      newBlake3,
		Decompress:     newZstdReader,
		DownloadObject: f.downloader(),
	}
}

// entryDescription renders entries the way the stream test renders delivered
// ones, so the two orderings are comparable by eye.
func entryDescription(entries []volume.Entry) string {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, fmt.Sprintf("%s %s %d", kindName(entry.Kind), entry.Path, entry.Size))
	}
	return strings.Join(lines, "\n")
}

// TestFetchManifestReportsEveryEntryInOrder is the read behind listing a
// version: every entry the volume holds, in canonical path order, with the
// header's own totals for the version as a whole.
func TestFetchManifestReportsEveryEntryInOrder(t *testing.T) {
	_, fake := pushFixture(t)

	result, err := transfer.FetchManifest(context.Background(), fake.client(t), manifestOptions(fake))
	if err != nil {
		t.Fatal(err)
	}

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
	if got := entryDescription(result.Manifest.Entries()); got != want {
		t.Errorf("entries\n got:\n%s\nwant:\n%s", got, want)
	}

	// The header's accounting, which is also what the decode checked itself
	// against: ten entries, and the file sizes summed.
	if result.EntryCount != 10 {
		t.Errorf("entry count %d, want 10", result.EntryCount)
	}
	if want := result.Manifest.TotalSize(); result.TotalSize != want {
		t.Errorf("total size %d, want the %d the entries sum to", result.TotalSize, want)
	}
	if result.ManifestDigest.Hex() == "" {
		t.Error("the result names no version")
	}
}

// TestFetchManifestFilterHoldsOnlyWhatItKeeps is what the filter is for: a
// caller reading one subtree of a large volume holds that subtree and not the
// rest. The totals stay the whole version's, since they come from the header
// rather than from what survived, and nothing downstream could recover them.
func TestFetchManifestFilterHoldsOnlyWhatItKeeps(t *testing.T) {
	_, fake := pushFixture(t)

	var seen []string
	opts := manifestOptions(fake)
	opts.EntryFilter = func(_ context.Context, entry volume.Entry) bool {
		seen = append(seen, entry.Path)
		return entry.Path == "nested" || strings.HasPrefix(entry.Path, "nested/")
	}

	result, err := transfer.FetchManifest(context.Background(), fake.client(t), opts)
	if err != nil {
		t.Fatal(err)
	}

	want := strings.Join([]string{
		"dir nested 0",
		"dir nested/cache 0",
		"dir nested/deep 0",
		fmt.Sprintf("file nested/deep/data.bin %d", volume.ChunkSize*2+1024),
		"file nested/dup.txt 12",
		"link nested/link 0",
	}, "\n")
	if got := entryDescription(result.Manifest.Entries()); got != want {
		t.Errorf("kept entries\n got:\n%s\nwant:\n%s", got, want)
	}

	// Every entry was offered exactly once, including the ones dropped: the
	// filter decides what is HELD, never what is read.
	if len(seen) != 10 {
		t.Errorf("the filter saw %d entries (%v), want all 10", len(seen), seen)
	}
	if result.EntryCount != 10 {
		t.Errorf("entry count %d, want the whole version's 10 rather than the 6 kept", result.EntryCount)
	}
	if got := result.Manifest.TotalSize(); result.TotalSize == got {
		t.Errorf("total size %d equals the kept entries' sum, so it is not the header's", result.TotalSize)
	}
}

// TestFetchManifestRefusesAManifestThatDoesNotHash is the check everything
// else rests on. A manifest is the root of the version's tree: entries checked
// against a document nobody verified are leaves authenticated against a root
// taken on faith, so a doctored manifest has to fail the read outright rather
// than produce entries.
func TestFetchManifestRefusesAManifestThatDoesNotHash(t *testing.T) {
	_, fake := pushFixture(t)

	// The stored bytes for one path changed, which the header's own
	// accounting still agrees with: only the digest catches this.
	fake.rewriteManifest(t, func(body []byte) []byte {
		return []byte(strings.Replace(string(body), "small.txt", "smallxtxt", 1))
	})

	result, err := transfer.FetchManifest(context.Background(), fake.client(t), manifestOptions(fake))
	if err == nil {
		t.Fatalf("a doctored manifest read as %d entries", len(result.Manifest.Entries()))
	}
	if !strings.Contains(err.Error(), "hashes to") {
		t.Errorf("error %q does not report the digest mismatch", err)
	}
}

// TestFetchVolumeManifestNarrowsToTheRefPath is the public surface, and the
// one narrowing a caller gets without writing a filter: the path on the ref.
func TestFetchVolumeManifestNarrowsToTheRefPath(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)
	var exchanges atomic.Int64

	api, err := client.NewManagementClient(client.ManagementClientOptions{
		APIKey:  "api-key",
		BaseURL: newManagementAPI(t, fake, &exchanges),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	pushed, err := api.PushVolume(ctx, client.PushVolumeOptions{
		Ref:       client.VolumeRef{Namespace: fakeNamespace, Volume: fakeVolume},
		SourceDir: root,
		SourceURI: "file:///fixture",
		Hasher:    newBlake3,
	})
	if err != nil {
		t.Fatal(err)
	}

	manifest, err := api.FetchVolumeManifest(ctx, client.FetchVolumeManifestOptions{
		Ref: client.VolumeRef{
			Namespace: fakeNamespace, Volume: fakeVolume, Path: "/nested/deep",
		},
		Hasher: newBlake3,
		Store:  fakePublicStore{download: fake.downloader()},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The path itself and what is under it, matched on slash boundaries.
	if len(manifest.Entries) != 2 {
		t.Fatalf("entries %v, want the directory and its one file", manifest.Entries)
	}
	if manifest.Entries[0].Path != "/nested/deep" ||
		manifest.Entries[0].Kind != client.VolumeEntryKindDirectory {
		t.Errorf("first entry %+v, want the /nested/deep directory", manifest.Entries[0])
	}
	if manifest.Entries[1].Path != "/nested/deep/data.bin" ||
		manifest.Entries[1].Kind != client.VolumeEntryKindFile {
		t.Errorf("second entry %+v, want /nested/deep/data.bin", manifest.Entries[1])
	}

	// The ref names the version rather than the path that was asked for, so
	// an entry's path assigned onto it addresses that entry.
	if manifest.VersionRef != pushed.VersionRef {
		t.Errorf("read %s, pushed %s", manifest.VersionRef, pushed.VersionRef)
	}
	pinned := manifest.VersionRef
	pinned.Path = manifest.Entries[1].Path
	if got, want := pinned.String(), fmt.Sprintf(
		"bdn:%s/%s@%s/nested/deep/data.bin", fakeNamespace, fakeVolume, pushed.VersionRef.Digest,
	); got != want {
		t.Errorf("composed ref %q, want %q", got, want)
	}

	if manifest.EntryCount != 10 {
		t.Errorf("entry count %d, want the whole version's 10", manifest.EntryCount)
	}
	if manifest.TotalSize != pushed.Bytes {
		t.Errorf("total size %d, want the %d bytes pushed", manifest.TotalSize, pushed.Bytes)
	}
	if exchanges.Load() != 2 {
		t.Errorf("%d token exchanges, want one per call", exchanges.Load())
	}
}
