package transfer

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
)

// ManifestOptions describes a manifest read.
type ManifestOptions struct {
	// Ref names the version whose manifest to read, resolved once and pinned
	// like a pull's.
	Ref bdn.ResolveRequest

	// EntryFilter keeps the entries it returns true for and drops the rest.
	// Nil keeps everything. It sees entries in the order the manifest carried
	// them, so it must not depend on entries it has not seen yet.
	//
	// ctx is the one driving the read, so it carries the caller's values and
	// is cancelled with it.
	EntryFilter func(ctx context.Context, entry volume.Entry) bool

	// NewHasher returns a fresh unkeyed BLAKE3 hash with a 32-byte digest.
	// Required: the manifest is verified against the digest that named it.
	NewHasher func() hash.Hash

	// Decompress and DownloadObject reach the object store. Both are required:
	// a manifest is one object read.
	Decompress     volume.Decompressor
	DownloadObject volume.ObjectDownloader
}

// Validate reports whether the options describe a read that can be attempted.
func (o ManifestOptions) Validate() error {
	switch {
	case o.Ref.Namespace == "":
		return errors.New("Ref.Namespace is required")
	case o.Ref.Volume == "":
		return errors.New("Ref.Volume is required")
	case o.NewHasher == nil:
		return errors.New("NewHasher is required")
	case o.Decompress == nil:
		return errors.New("Decompress is required")
	case o.DownloadObject == nil:
		return errors.New("DownloadObject is required")
	}
	return nil
}

// ManifestResult is one version's manifest as read.
type ManifestResult struct {
	// ManifestDigest is the version the manifest was read at, which is what
	// the ref resolved to. The caller pairs it with the ref it asked for to
	// name that version again.
	ManifestDigest volume.Digest

	// EntryCount and TotalSize are the manifest header's own accounting of
	// the whole version, which is why they are carried rather than left to
	// the caller to sum: they are the numbers BEFORE the filter, and nothing
	// downstream of a filtered read can re-derive them.
	EntryCount uint64
	TotalSize  uint64

	// Manifest holds the entries that survived the filter, canonically
	// ordered.
	Manifest *volume.Manifest
}

// FetchManifest resolves a ref and reads the manifest of the version it names.
//
// The document is streamed rather than held whole, so a filter that keeps a
// subtree of a very large volume holds only that subtree. The bytes are read
// and hashed either way: a manifest's digest covers the whole object, so
// stopping early would leave the digest of a prefix, which matches nothing.
//
// Nothing about the tree is trusted before that digest is checked. Verifying
// entries against a manifest nobody verified would authenticate the leaves
// against a root taken on faith, which is the same reason a pull checks the
// manifest before any chunk (see resolvePlan).
func FetchManifest(ctx context.Context, client *bdn.Client, opts ManifestOptions) (*ManifestResult, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if err := volume.CheckHasher(opts.NewHasher); err != nil {
		return nil, err
	}

	resolved, err := client.Resolve(ctx, opts.Ref)
	if err != nil {
		return nil, err
	}
	digest := resolved.Resolved.OriginDigest
	pinned := opts.Ref.Pinned(digest.String())
	origin := newOrigin(client, pinned, resolved.Resolved.OrgID, resolved.Origin)

	manifest, totals, err := streamManifest(ctx, origin, opts, resolved.Resolved.Target, digest)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", digest, err)
	}
	return &ManifestResult{
		ManifestDigest: digest,
		EntryCount:     totals.EntryCount,
		TotalSize:      totals.TotalSize,
		Manifest:       manifest,
	}, nil
}

// streamManifest reads one manifest object through the hasher and the decoder
// together, then checks the digest.
//
// The digest is checked after decoding rather than before, which is the one
// place this differs from the whole-object path: a streaming decode has read
// the records by the time the last byte arrives, so the check has to come at
// the end. Nothing is trusted in between: the decoder validates the document
// against itself and touches no object the records name, and the result is
// discarded if the check fails.
func streamManifest(
	ctx context.Context,
	origin *origin,
	opts ManifestOptions,
	target volume.Target,
	want volume.Digest,
) (*volume.Manifest, *volume.ManifestTotals, error) {
	body, err := origin.stream(ctx, opts.DownloadObject, opts.Decompress, target, 0)
	if err != nil {
		return nil, nil, err
	}
	defer body.Close()

	// One byte past the bound distinguishes a manifest that just fits from one
	// that does not, the same way the whole-object read does it.
	bounded := &countingReader{r: io.LimitReader(body, volume.MaxManifestBytes+1)}
	hashed, sum, err := volume.HashReader(opts.NewHasher, bounded)
	if err != nil {
		return nil, nil, err
	}

	// The decoder is handed a filter of its own rather than the context: the
	// context belongs to the read, not to the record format.
	var keep func(volume.Entry) bool
	if opts.EntryFilter != nil {
		keep = func(entry volume.Entry) bool { return opts.EntryFilter(ctx, entry) }
	}

	manifest, totals, err := volume.DecodeManifestStream(hashed, keep)
	if err != nil {
		return nil, nil, err
	}
	if bounded.n > volume.MaxManifestBytes {
		return nil, nil, fmt.Errorf("manifest is larger than the %d byte limit", volume.MaxManifestBytes)
	}
	got, err := sum()
	if err != nil {
		return nil, nil, err
	}
	if got != want {
		return nil, nil, fmt.Errorf("manifest hashes to %s, but was fetched as %s", got, want)
	}
	return manifest, &totals, nil
}

// countingReader counts what passes through it, so the bound can be judged on
// bytes read rather than on a document held in memory.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
