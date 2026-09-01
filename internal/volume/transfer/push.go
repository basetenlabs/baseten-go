// Package transfer runs a push or a pull: it drives the protocol client
// through an upload session or a download, applying the concurrency limits and
// reporting progress.
//
// It sits above the two layers it uses. The volume package knows the wire
// format and the filesystem and nothing about the network; the bdn package
// knows the network and nothing about either. This package is where they meet.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
)

// PushOptions describes a push.
type PushOptions struct {
	// Namespace and Volume name where to publish. The volume is created if it
	// does not exist; the namespace is not.
	Namespace string
	Volume    string

	// SourceDir is the directory to push.
	SourceDir string

	// SourceURI records where the tree came from. It is inside the bytes the
	// manifest digest covers, so two pushes of identical trees from different
	// paths are different versions. Empty derives it from SourceDir.
	SourceURI string

	// Tags are applied atomically with the commit.
	Tags []string

	// RequireHeadMove fails the push before it uploads anything if the token
	// could not move the volume's head. Without it, a push that cannot move
	// head still publishes the version — refs without a tag simply keep
	// resolving to the old one.
	RequireHeadMove bool

	// NewHasher returns a fresh unkeyed BLAKE3 hash with a 32-byte digest.
	// Required.
	NewHasher func() hash.Hash

	// DownloadObject and Decompress enable reuse of the previous version's
	// chunks, which live in object storage. Both are optional; without them
	// the push simply uploads everything, which is slower and not wrong.
	DownloadObject volume.ObjectDownloader
	Decompress     volume.Decompressor

	Progress    volume.ProgressFunc
	Concurrency volume.Concurrency

	// Limiter governs how many object uploads run at once. Nil adapts the
	// limit to what the origin will bear, unless Concurrency.ChunkOperations
	// pins it, in which case that number is honoured exactly.
	Limiter volume.Limiter
}

// Validate reports whether the options describe a push that can be attempted.
func (o PushOptions) Validate() error {
	switch {
	case o.Namespace == "":
		return errors.New("Namespace is required")
	case o.Volume == "":
		return errors.New("Volume is required")
	case o.SourceDir == "":
		return errors.New("SourceDir is required")
	case o.NewHasher == nil:
		return errors.New("NewHasher is required")
	}
	for _, tag := range o.Tags {
		if tag == bdn.HeadTag {
			return fmt.Errorf("tag %q is reserved; use RequireHeadMove", bdn.HeadTag)
		}
		if tag == "" {
			return errors.New("Tags must not be empty")
		}
	}
	return nil
}

// PushResult is what a push published.
type PushResult struct {
	ManifestDigest volume.Digest
	Sequence       int64

	// HeadUpdated is the server's word on whether refs without a tag now
	// resolve to this version.
	HeadUpdated bool

	// HeadMoveDenied is this client's reason for not asking: the token's
	// grants did not cover the reserved head tag. The version was still
	// published, and can still be reached by digest or by tag.
	HeadMoveDenied bool

	TagsApplied []string

	Files int64
	Bytes int64

	// Chunks counts every object the push accounted for: chunks, chunkmaps,
	// and the manifest.
	Chunks int64

	// Unique, Reused, and Existing partition the objects. Reused made no
	// request at all, because a previous version or an earlier file in this
	// push already had those bytes. Existing was uploaded and the server
	// already had it.
	Unique   int64
	Reused   int64
	Existing int64
}

// Push uploads a directory and publishes it as a new version of the volume.
//
// Nothing is visible until the commit at the end: an abandoned push leaves
// unreferenced objects for collection and no version at all. The session it
// runs in cannot be extended, so a push that takes longer than the session's
// lifetime fails rather than committing late.
func Push(ctx context.Context, client *bdn.Client, opts PushOptions) (*PushResult, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if err := volume.CheckHasher(opts.NewHasher); err != nil {
		return nil, err
	}
	// The service lowercases namespace and volume at every boundary it has, so
	// a mixed-case name has to be folded here too: it reaches the object keys
	// the prior version is read from, where the wrong case is a miss rather
	// than an error, and delta reuse would silently stop happening.
	opts.Namespace = strings.ToLower(opts.Namespace)
	opts.Volume = strings.ToLower(opts.Volume)

	sourceURI, err := pushSourceURI(opts)
	if err != nil {
		return nil, err
	}

	p, source, err := startPush(ctx, client, opts)
	if err != nil {
		return nil, err
	}

	p.progress.SetPhase(volume.PhaseUpload, int64(len(source.Files)), int64(source.TotalBytes))
	files, err := p.pushFiles(ctx, source)
	if err != nil {
		return nil, err
	}

	manifestDigest, err := p.uploadManifest(ctx, source, sourceURI, files)
	if err != nil {
		return nil, err
	}

	p.progress.SetPhase(volume.PhaseCommit, 0, 0)
	return p.commit(ctx, manifestDigest)
}

// pushSourceURI settles the provenance URI, which is inside the bytes the
// manifest digest covers.
func pushSourceURI(opts PushOptions) (string, error) {
	sourceURI := opts.SourceURI
	if sourceURI == "" {
		var err error
		if sourceURI, err = volume.SourceURIForDir(opts.SourceDir); err != nil {
			return "", err
		}
	}
	// A source directory whose name is not valid UTF-8 produces a URI that is
	// not either, and the URI is inside the digest.
	if err := volume.ValidateSourceURI(sourceURI); err != nil {
		return "", err
	}
	return sourceURI, nil
}

// startPush does everything that has to happen before the first byte is
// uploaded: check what the token allows, scan the tree, open a session, and
// look up the version whose chunks can be reused.
func startPush(ctx context.Context, client *bdn.Client, opts PushOptions) (*pusher, *volume.Source, error) {
	// The token says what it may do, so a push that could never move head can
	// say so before it uploads anything rather than after. The answer comes
	// from the token's own claims and not from the scope list the exchange
	// reported alongside it: that list is the request echoed back, so it
	// cannot narrow anything, and were it ever to start narrowing it would say
	// a capability was held when the minted token did not carry it. The claims
	// describe what was actually minted, which is the only thing that answers
	// the question.
	grants := client.Grants(ctx)
	headAllowed := grants.PermitsHeadMove(opts.Namespace, opts.Volume)
	if opts.RequireHeadMove && !headAllowed {
		return nil, nil, fmt.Errorf("push %s/%s: the token cannot move head", opts.Namespace, opts.Volume)
	}

	progress := volume.NewProgressReporter(opts.Progress)
	progress.SetPhase(volume.PhaseScan, 0, 0)
	source, err := volume.ScanSource(opts.SourceDir)
	if err != nil {
		return nil, nil, err
	}
	// A tree the format refuses should fail here, with the entry named,
	// rather than at the commit gate with the whole upload already spent.
	// Everything is strict on this side, dangling links included: the
	// version being published is immutable, so a missing target can never
	// appear later.
	if err := volume.CheckSourceContainment(source); err != nil {
		return nil, nil, err
	}

	session, err := client.BeginUpload(ctx, bdn.BeginUploadRequest{
		Namespace:       opts.Namespace,
		Volume:          opts.Volume,
		CreateIfMissing: true,
	})
	if err != nil {
		return nil, nil, err
	}

	limits := opts.Concurrency.WithDefaults()
	limiter := opts.Limiter
	if limiter == nil {
		// From the caller's own Concurrency rather than the defaulted copy: a
		// pinned operation count and an unset one choose different limiters,
		// and WithDefaults has already erased the difference.
		limiter = defaultLimiter(opts.Concurrency)
	}
	p := &pusher{
		client:      client,
		opts:        opts,
		grants:      grants,
		headAllowed: headAllowed,
		session:     session,
		limits:      limits,
		limiter:     limiter,
		bytes:       volume.NewByteGate(limits.MaxBytesInFlight),
		progress:    progress,
		sourceDir:   opts.SourceDir,
	}

	// The previous version is an optimization: knowing which chunks a file
	// already had lets an unchanged file skip the wire entirely. Every failure
	// along the way is soft, because being unable to look it up costs
	// bandwidth and nothing else.
	p.prior = p.loadPriorVersion(ctx)
	return p, source, nil
}

// uploadManifest builds the manifest describing the pushed tree and stores it.
// Nothing is visible until the commit that names it.
func (p *pusher) uploadManifest(
	ctx context.Context,
	source *volume.Source,
	sourceURI string,
	files []volume.FileEntry,
) (volume.Digest, error) {
	manifest := volume.NewManifest(source, sourceURI, files)
	body := volume.EncodeManifest(manifest)
	digest, err := volume.HashBytes(p.opts.NewHasher, body)
	if err != nil {
		return volume.Digest{}, err
	}
	if _, err := p.uploadMetadata(ctx, bdn.ContentTypeManifest, digest, body); err != nil {
		return volume.Digest{}, err
	}
	return digest, nil
}

// pusher holds the state one push shares across its files.
type pusher struct {
	client      *bdn.Client
	opts        PushOptions
	grants      bdn.Grants
	headAllowed bool
	session     *bdn.UploadSession
	limits      volume.Concurrency
	limiter     volume.Limiter
	bytes       *volume.ByteGate
	progress    *volume.ProgressReporter
	sourceDir   string

	// prior is the previous version, for chunk reuse. Nil when there is none
	// or it could not be read.
	prior *priorVersion

	// uploaded remembers the objects this push has already sent, keyed by kind
	// and digest, so the same bytes appearing in several files are sent once.
	// Two goroutines can miss at the same moment and both upload; that costs a
	// duplicate request and nothing else, which is cheaper than serializing
	// every lookup behind the upload it guards.
	uploaded sync.Map

	stats stats
}

// stats accumulates the counts a push reports.
type stats struct {
	mu       sync.Mutex
	files    int64
	bytes    int64
	chunks   int64
	unique   int64
	reused   int64
	existing int64
}

func (s *stats) add(chunks, unique, reused, existing int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks += chunks
	s.unique += unique
	s.reused += reused
	s.existing += existing
}

func (s *stats) addFile(bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files++
	s.bytes += bytes
}

// pushFiles uploads every file's chunks and returns the manifest entries.
func (p *pusher) pushFiles(ctx context.Context, source *volume.Source) ([]volume.FileEntry, error) {
	entries := make([]volume.FileEntry, len(source.Files))
	err := forEach(ctx, p.limits.FileJobs, source.Files,
		func(ctx context.Context, i int, file volume.SourceFile) error {
			entry, err := p.pushFile(ctx, file)
			if err != nil {
				return fmt.Errorf("push %s: %w", file.Path, err)
			}
			// Each call owns its own index, so no lock is needed here.
			entries[i] = entry
			return nil
		})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// pushFile uploads one file's chunks and builds its manifest entry.
func (p *pusher) pushFile(ctx context.Context, file volume.SourceFile) (volume.FileEntry, error) {
	prior, priorChunks := p.priorChunks(ctx, file)

	chunks, err := p.pushChunks(ctx, file, priorChunks)
	if err != nil {
		return volume.FileEntry{}, err
	}
	p.stats.addFile(int64(file.Size))
	p.progress.Add(1, int64(file.Size))

	entry := volume.FileEntry{Path: file.Path, Mode: file.Mode, Size: file.Size}

	// Every chunk came from the previous version, so the object describing
	// them is still correct and need not be rebuilt or re-uploaded. The mode
	// is taken from this scan rather than from the previous entry: a file
	// whose permissions changed has the same bytes and a different meaning.
	if prior != nil && allReused(chunks, priorChunks) {
		entry.Kind = prior.Kind
		entry.Chunk = prior.Chunk
		entry.Digest = prior.Digest
		entry.FileDigest = prior.FileDigest
		entry.Target = prior.Target
		// The kept chunkmap is an object the committed manifest references
		// and this push made no request for, which is what the reused
		// partition means. Each chunk was counted as it matched and the
		// manifest is counted when it is sent; this return would otherwise
		// skip the one object between them. A single-chunk entry names its
		// chunk inline, so it has no extra object to count.
		if prior.Kind == volume.FileKindChunkmap {
			p.stats.add(1, 0, 1, 0)
		}
		return entry, nil
	}

	if len(chunks) == 1 {
		// One chunk, named inline. A chunkmap here would be an object whose
		// only content is a pointer to another object.
		entry.Kind = volume.FileKindChunk
		entry.Chunk = chunks[0].ref
		return entry, nil
	}

	chunkmap := &volume.Chunkmap{FileSize: file.Size}
	for _, chunk := range chunks {
		chunkmap.Chunks = append(chunkmap.Chunks, chunk.ref)
	}
	if err := volume.ValidateChunkmap(chunkmap); err != nil {
		return volume.FileEntry{}, err
	}
	body := volume.EncodeChunkmap(chunkmap)
	digest, err := volume.HashBytes(p.opts.NewHasher, body)
	if err != nil {
		return volume.FileEntry{}, err
	}
	target, err := p.uploadMetadata(ctx, bdn.ContentTypeChunkmap, digest, body)
	if err != nil {
		return volume.FileEntry{}, err
	}
	entry.Kind = volume.FileKindChunkmap
	entry.Digest = digest
	entry.Target = target
	return entry, nil
}

// failureOutcome decides what a failed operation tells the limiter.
//
// A transfer that gives up cancels everything still in flight, and those
// siblings then fail for a reason that says nothing about the origin. Counting
// them as pushback would have one genuine failure looking like a wall of them,
// and an adaptive limiter would cut its concurrency on the strength of its own
// cancellation.
func failureOutcome(ctx context.Context, err error) volume.Outcome {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return volume.Neutral
	}
	return volume.Stall
}

// pushedChunk is one chunk's outcome.
type pushedChunk struct {
	ref volume.ChunkRef

	// fromPrior means these exact bytes were already at this offset in the
	// previous version, which is what lets the whole file entry be reused.
	// Distinct from the reused counter in stats, which also counts a chunk
	// deduplicated within this push — that saves a request but says nothing
	// about whether the file is unchanged.
	fromPrior bool
}

// allReused reports whether every chunk was taken from the previous version
// unchanged, which is what makes the previous file entry still correct.
func allReused(chunks []pushedChunk, prior []volume.ChunkRef) bool {
	// Two empty lists match vacuously, which would claim a reuse nothing
	// checked. A file always has at least one chunk, so this cannot happen —
	// but the answer to "was everything reused" is no when nothing was.
	if len(chunks) == 0 || len(chunks) != len(prior) {
		return false
	}
	for _, chunk := range chunks {
		if !chunk.fromPrior {
			return false
		}
	}
	return true
}

// pushChunks uploads a file's chunks, spawning one task per chunk once that
// chunk holds both a slot and its bytes of the budget.
//
// The tasks are not a fixed pool. A pool's size is a ceiling on concurrency
// that the limiter cannot see past; here the limiter alone decides how much
// runs at once, so a limiter that learns the origin can serve more is free to
// let it.
func (p *pusher) pushChunks(ctx context.Context, file volume.SourceFile, prior []volume.ChunkRef) ([]pushedChunk, error) {
	handle, err := os.Open(filepath.Join(p.sourceDir, filepath.FromSlash(file.Path)))
	if err != nil {
		return nil, err
	}
	defer handle.Close()

	ranges := volume.ChunkRanges(file.Size)
	chunks := make([]pushedChunk, volume.ChunkCount(file.Size))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}

	spanned := 0
	for i, span := range ranges {
		spanned++
		// The gates are taken in one order everywhere: a slot, then the bytes
		// it will hold. Consistency is what keeps them from deadlocking, and
		// taking the slot first means a saturated origin stops the file being
		// read rather than filling memory with data nothing can send.
		permit, err := p.limiter.Acquire(ctx)
		if err != nil {
			fail(err)
			break
		}
		if err := p.bytes.Acquire(ctx, int64(span.Length)); err != nil {
			permit.CompleteUntimed(volume.Neutral)
			fail(err)
			break
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer p.bytes.Release(int64(span.Length))

			chunk, err := p.pushChunk(ctx, handle, span, priorAt(prior, i, span), permit)
			if err != nil {
				fail(err)
				return
			}
			chunks[i] = chunk
		}()
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	// The slice was sized from the count and filled by the walk, which are two
	// statements of the same rule. If the count were ever the larger of the
	// two, the tail of this slice would still hold zero values — and a
	// zero-valued chunk is a real digest of no bytes, so it would be committed
	// as a chunk rather than caught as a blank. Nothing downstream can tell
	// the difference, so the disagreement is caught here.
	if spanned != len(chunks) {
		return nil, fmt.Errorf("file %s: split yielded %d chunks, the count said %d",
			file.Path, spanned, len(chunks))
	}
	return chunks, nil
}

// priorAt returns the previous version's chunk covering the same span, if it
// had one. Anything else — a different length, a shifted offset, a shorter
// file — means the comparison would be against different bytes.
func priorAt(prior []volume.ChunkRef, index int, span volume.ChunkRange) *volume.ChunkRef {
	if index >= len(prior) {
		return nil
	}
	if prior[index].Offset != span.Offset || prior[index].Length != span.Length {
		return nil
	}
	return &prior[index]
}

// pushChunk hashes one span of the file and stores it, unless something
// already has. It consumes the permit exactly once, whichever way it goes.
func (p *pusher) pushChunk(
	ctx context.Context,
	handle *os.File,
	span volume.ChunkRange,
	prior *volume.ChunkRef,
	permit *volume.Permit,
) (pushedChunk, error) {
	buffer := make([]byte, span.Length)
	if span.Length > 0 {
		// Reading at an offset rather than seeking lets many tasks read
		// disjoint parts of one large file at once, so a single big file is
		// not limited to what one sequential read can pull.
		if _, err := handle.ReadAt(buffer, int64(span.Offset)); err != nil {
			permit.CompleteUntimed(volume.Neutral)
			return pushedChunk{}, err
		}
	}
	digest, err := volume.HashBytes(p.opts.NewHasher, buffer)
	if err != nil {
		permit.CompleteUntimed(volume.Neutral)
		return pushedChunk{}, err
	}

	ref := volume.ChunkRef{Digest: digest, Length: span.Length, Offset: span.Offset}

	// The previous version had these exact bytes here. Nothing to send, and
	// nothing observed about the origin either way.
	if prior != nil && prior.Digest == digest {
		permit.CompleteUntimed(volume.Neutral)
		ref.Target = prior.Target
		p.stats.add(1, 0, 1, 0)
		return pushedChunk{ref: ref, fromPrior: true}, nil
	}

	if target, ok := p.seen(bdn.ContentTypeChunk, digest); ok {
		permit.CompleteUntimed(volume.Neutral)
		ref.Target = target
		p.stats.add(1, 0, 1, 0)
		return pushedChunk{ref: ref}, nil
	}

	result, err := p.client.UploadObject(ctx, p.session, bdn.ContentTypeChunk, digest, buffer)
	if err != nil {
		permit.CompleteUntimed(failureOutcome(ctx, err))
		return pushedChunk{}, err
	}
	permit.Complete(result.Outcome)

	p.remember(bdn.ContentTypeChunk, digest, result.Target)
	ref.Target = result.Target
	if result.Created {
		p.stats.add(1, 1, 0, 0)
	} else {
		p.stats.add(1, 0, 0, 1)
	}
	return pushedChunk{ref: ref}, nil
}

// uploadMetadata stores a chunkmap or a manifest.
//
// These take a slot but no share of the byte budget, and contribute no latency
// sample. They are a different size and shape from a chunk, and letting them
// into the sample would move the baseline a limiter measures inflation
// against.
func (p *pusher) uploadMetadata(
	ctx context.Context,
	contentType string,
	digest volume.Digest,
	body []byte,
) (volume.Target, error) {
	if target, ok := p.seen(contentType, digest); ok {
		p.stats.add(1, 0, 1, 0)
		return target, nil
	}

	permit, err := p.limiter.Acquire(ctx)
	if err != nil {
		return volume.Target{}, err
	}
	result, err := p.client.UploadObject(ctx, p.session, contentType, digest, body)
	if err != nil {
		permit.CompleteUntimed(failureOutcome(ctx, err))
		return volume.Target{}, err
	}
	permit.CompleteUntimed(result.Outcome)

	p.remember(contentType, digest, result.Target)
	if result.Created {
		p.stats.add(1, 1, 0, 0)
	} else {
		p.stats.add(1, 0, 0, 1)
	}
	return result.Target, nil
}

func (p *pusher) seen(contentType string, digest volume.Digest) (volume.Target, bool) {
	value, ok := p.uploaded.Load(contentType + ":" + digest.String())
	if !ok {
		return volume.Target{}, false
	}
	return value.(volume.Target), true
}

func (p *pusher) remember(contentType string, digest volume.Digest, target volume.Target) {
	p.uploaded.Store(contentType+":"+digest.String(), target)
}

// commit publishes the manifest.
func (p *pusher) commit(ctx context.Context, digest volume.Digest) (*PushResult, error) {
	key, err := bdn.NewIdempotencyKey()
	if err != nil {
		return nil, err
	}

	// Whether to ask for the head move was decided from the token's own
	// claims. Asking without the grant is a certain denial that fails the
	// whole commit, so a push that cannot move head publishes the version
	// anyway and says so.
	//
	// Passing ctx here is load-bearing, not incidental. This is the only step
	// of a push that outlives the call, and a cancelled context fails the
	// request before it is sent — which is the entire reason a cancelled push
	// cannot publish a version, while a cancelled pull needed an explicit
	// guard before its own last step, a local rename that consults nothing.
	// Making this call survive cancellation, or adding a local finalization
	// step after it, would reintroduce that bug on this side, where no test is
	// watching for it.
	result, err := p.client.Commit(ctx, bdn.CommitRequest{
		Namespace:      p.opts.Namespace,
		Volume:         p.opts.Volume,
		UploadID:       p.session.UploadID,
		ManifestDigest: digest,
		UpdateHead:     p.headAllowed,
		Tags:           p.opts.Tags,
		IdempotencyKey: key,
	})
	if err != nil {
		return nil, err
	}

	p.stats.mu.Lock()
	defer p.stats.mu.Unlock()
	push := &PushResult{
		ManifestDigest: digest,
		Sequence:       result.Sequence,
		HeadUpdated:    result.HeadUpdated,
		HeadMoveDenied: !p.headAllowed,
		Files:          p.stats.files,
		Bytes:          p.stats.bytes,
		Chunks:         p.stats.chunks,
		Unique:         p.stats.unique,
		Reused:         p.stats.reused,
		Existing:       p.stats.existing,
	}
	if result.TagApplied {
		push.TagsApplied = p.opts.Tags
	}
	return push, nil
}
