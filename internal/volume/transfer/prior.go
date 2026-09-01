package transfer

import (
	"context"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
)

// priorVersion is the previous version of the volume, read so that files whose
// bytes have not changed can skip the wire.
//
// Everything about it is optional. Reading it needs credentials for object
// storage that a push does not otherwise use, and permission to resolve a
// volume a push-only token may not have. When any of that is missing, or the
// lookup simply fails, the push uploads everything: slower, never wrong.
type priorVersion struct {
	files     map[string]volume.FileEntry
	org       string
	namespace string
	origin    bdn.Origin
}

// loadPriorVersion resolves the volume's current head and indexes its files by
// path. It returns nil for every failure, including a volume that has no
// version yet.
func (p *pusher) loadPriorVersion(ctx context.Context) *priorVersion {
	if p.opts.DownloadObject == nil || p.opts.Decompress == nil {
		return nil
	}
	// Asking for a resolve the token plainly cannot do would be a denial in
	// the server's audit log for no benefit, since the answer is already
	// known.
	if !p.grants.PermitsResolve(p.opts.Namespace, p.opts.Volume) {
		return nil
	}

	// The ref is built through Ref rather than concatenated, because the
	// server requires the bdn:// scheme and rejects a bare "namespace/volume"
	// outright. Getting that wrong is invisible from here: every failure on
	// this path is soft, so the reuse would simply never happen and nothing
	// would say so.
	ref := bdn.Ref{Namespace: p.opts.Namespace, Volume: p.opts.Volume}
	resolved, err := p.client.Resolve(ctx, ref.String())
	if err != nil {
		return nil
	}
	prior := &priorVersion{org: resolved.Resolved.OrgID, namespace: p.opts.Namespace, origin: resolved.Origin}

	body, err := volume.FetchObject(ctx, p.opts.DownloadObject, p.opts.Decompress,
		prior.request(resolved.Resolved.Target, 0), volume.MaxManifestBytes)
	if err != nil {
		return nil
	}
	manifest, err := volume.DecodeManifest(body)
	if err != nil {
		return nil
	}

	prior.files = make(map[string]volume.FileEntry, len(manifest.Files))
	for _, file := range manifest.Files {
		prior.files[file.Path] = file
	}
	return prior
}

// request builds an object read against the leased credentials.
func (v *priorVersion) request(target volume.Target, size int64) volume.ObjectDownload {
	return volume.ObjectDownload{
		Endpoint: v.origin.Endpoint,
		Region:   v.origin.Region,
		Bucket:   v.origin.Bucket,
		Key:      volume.ObjectKey(v.org, v.namespace, target),
		Credentials: volume.Credentials{
			AccessKeyID:     v.origin.AccessKeyID,
			SecretAccessKey: v.origin.SecretAccessKey,
			SessionToken:    v.origin.SessionToken,
		},
		ExpectedSize: size,
	}
}

// priorChunks returns the previous version's entry for a file and the chunks
// it was made of, or nothing when there is no usable comparison.
//
// A file of a different size is not compared at all: the chunk boundaries
// would line up only by accident, and a per-chunk comparison against bytes
// that moved finds nothing while costing a chunkmap read.
func (p *pusher) priorChunks(ctx context.Context, file volume.SourceFile) (*volume.FileEntry, []volume.ChunkRef) {
	if p.prior == nil {
		return nil, nil
	}
	entry, ok := p.prior.files[file.Path]
	if !ok || entry.Size != file.Size {
		return nil, nil
	}

	switch entry.Kind {
	case volume.FileKindChunk:
		return &entry, []volume.ChunkRef{entry.Chunk}
	case volume.FileKindChunkmap:
		body, err := volume.FetchObject(ctx, p.opts.DownloadObject, p.opts.Decompress,
			p.prior.request(entry.Target, 0), volume.MaxChunkmapBytes)
		if err != nil {
			return nil, nil
		}
		// Same gap as on the read path, with a worse consequence: these chunk
		// digests are copied into the manifest this push commits, so a
		// substituted chunkmap corrupts what gets published rather than only
		// what one caller reads. Reuse is an optimisation, so a chunkmap that
		// does not verify is simply not reused.
		if err := verifyBody(p.opts.NewHasher, body, entry.Digest, "chunkmap"); err != nil {
			return nil, nil
		}
		chunkmap, err := volume.DecodeChunkmap(body)
		if err != nil {
			return nil, nil
		}
		return &entry, chunkmap.Chunks
	default:
		// A slabmap packs several files into one chunk, so its chunks are not
		// this file's to reuse.
		return nil, nil
	}
}
