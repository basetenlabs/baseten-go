package transfer

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
)

// PullOptions describes a download.
type PullOptions struct {
	// Ref names what to download: "namespace/volume", optionally with ":tag"
	// or "@digest". Whatever it names is resolved once and pinned, so a tag
	// that moves partway through cannot produce a tree assembled from two
	// versions.
	Ref string

	// DestDir is where the tree ends up.
	DestDir string

	// Overwrite writes into DestDir in place rather than staging the tree
	// beside it and moving it into position when it is complete. In place, a
	// failed download leaves a partly written directory; staged, it leaves
	// DestDir untouched. Files already in DestDir that the volume does not
	// describe are left alone either way.
	Overwrite bool

	// Include narrows the download to named entries. Each is an exact path or
	// a directory whose contents are wanted; one that matches nothing is an
	// error. Empty downloads everything.
	Include []string

	// Restart discards a partly downloaded tree from an earlier attempt
	// instead of continuing it.
	Restart bool

	// NewHasher returns a fresh unkeyed BLAKE3 hash with a 32-byte digest.
	// Required: every chunk is verified against the digest the manifest
	// records, and continuing an interrupted download works by hashing what is
	// already on disk.
	NewHasher func() hash.Hash

	// Decompress and DownloadObject reach the object store. Both are required:
	// a download is nothing but object reads.
	Decompress     volume.Decompressor
	DownloadObject volume.ObjectDownloader

	Progress    volume.ProgressFunc
	Concurrency volume.Concurrency

	// Limiter governs how many object reads run at once. Nil adapts the limit
	// to what the origin will bear, unless Concurrency.ChunkOperations pins
	// it, in which case that number is honoured exactly.
	Limiter volume.Limiter
}

// Validate reports whether the options describe a download that can be
// attempted.
func (o PullOptions) Validate() error {
	switch {
	case o.Ref == "":
		return errors.New("Ref is required")
	case o.DestDir == "":
		return errors.New("DestDir is required")
	case o.NewHasher == nil:
		return errors.New("NewHasher is required")
	case o.Decompress == nil:
		return errors.New("Decompress is required")
	case o.DownloadObject == nil:
		return errors.New("DownloadObject is required")
	case o.Overwrite && o.Restart:
		// Restart means "discard the partly downloaded tree", which in
		// Overwrite mode would be the caller's own directory, including the
		// files this mode promises to leave alone. It would also buy nothing:
		// writing in place already verifies every chunk against its digest and
		// refetches whatever does not match, which is the whole of what
		// starting over would achieve.
		return errors.New("Restart applies to staged downloads; Overwrite already refetches whatever does not verify")
	}
	if _, err := bdn.ParseRef(o.Ref); err != nil {
		return err
	}
	return nil
}

// PullResult is what a download produced.
type PullResult struct {
	// VersionRef is the ref pinned to the digest that was downloaded, which is
	// the form to quote to get this exact tree again.
	VersionRef     string
	ManifestDigest volume.Digest

	Files int64
	Bytes int64

	// SelectedFiles and TotalFiles report what a subset download narrowed to.
	// They are equal when the whole volume was downloaded.
	SelectedFiles int64
	TotalFiles    int64

	// ChunksFetched and ChunksReused partition the chunks that carry bytes.
	// Reused were already on disk with the right contents, from an earlier
	// attempt. An empty file's zero-length chunk is in neither: sizing the
	// file produces it.
	ChunksFetched int64
	ChunksReused  int64

	// Warnings are the containment findings that did not stop the download:
	// a dangling link, a link through a file, an entry whose parent has no
	// record. Manifests predating the containment rule carry these; they are
	// written out faithfully and reported here rather than silently. They
	// are judged on the whole manifest, so a subset download still reports
	// what the version carries.
	Warnings []volume.ContainmentWarning
}

// Pull downloads a version of a volume into a directory.
//
// Every chunk is checked against the digest the manifest records before it is
// written, so a truncated or corrupted read fails the download rather than
// producing a file that looks complete.
func Pull(ctx context.Context, client *bdn.Client, opts PullOptions) (*PullResult, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if err := volume.CheckHasher(opts.NewHasher); err != nil {
		return nil, err
	}

	progress := volume.NewProgressReporter(opts.Progress)
	plan, err := resolvePlan(ctx, client, opts, progress)
	if err != nil {
		return nil, err
	}

	dest := filepath.Clean(opts.DestDir)
	workDir, staged, err := prepareDestination(dest, plan.digest, opts)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(workDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	limits := opts.Concurrency.WithDefaults()
	limiter := opts.Limiter
	if limiter == nil {
		// From the caller's own Concurrency rather than the defaulted copy: a
		// pinned operation count and an unset one choose different limiters,
		// and WithDefaults has already erased the difference.
		limiter = defaultLimiter(opts.Concurrency)
	}
	p := &puller{
		opts:     opts,
		origin:   plan.origin,
		root:     root,
		limits:   limits,
		limiter:  limiter,
		bytes:    volume.NewByteGate(limits.MaxBytesInFlight),
		progress: progress,
	}

	progress.SetPhase(volume.PhaseDownload, int64(len(plan.manifest.Files)), int64(plan.manifest.TotalSize()))
	if err := p.materialize(ctx, plan.manifest); err != nil {
		return nil, err
	}
	// A cancellation that arrives once every download has already returned
	// leaves materialize with nothing left to abandon, so it reports success
	// and the destination would be swapped into place for a caller who asked
	// to stop. Publishing is the only step here that outlives the call — a
	// push cannot reach the same state, because its last step is an HTTP
	// commit that a cancelled context fails on its own. The stage is left
	// where it is, which is what a later attempt resumes from.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.publish(plan.manifest, root, workDir, dest, plan.digest, staged); err != nil {
		return nil, err
	}

	return &PullResult{
		Warnings:       plan.warnings,
		VersionRef:     plan.pinned.String(),
		ManifestDigest: plan.digest,
		Files:          int64(len(plan.manifest.Files)),
		Bytes:          int64(plan.manifest.TotalSize()),
		SelectedFiles:  int64(len(plan.manifest.Files)),
		TotalFiles:     plan.totalFiles,
		ChunksFetched:  p.stats.fetched.Load(),
		ChunksReused:   p.stats.reused.Load(),
	}, nil
}

// pullPlan is everything settled before anything is written: which version,
// where to read it from, and which of its entries this download covers.
type pullPlan struct {
	digest   volume.Digest
	pinned   bdn.Ref
	origin   *origin
	manifest *volume.Manifest

	// totalFiles is the whole version's file count, which a subset download
	// reports alongside the smaller number it selected.
	totalFiles int64

	// warnings are the plan check's non-fatal containment findings, carried
	// through to the result.
	warnings []volume.ContainmentWarning
}

// resolvePlan pins the ref to one version, reads its manifest, and narrows it
// to what was asked for.
func resolvePlan(
	ctx context.Context,
	client *bdn.Client,
	opts PullOptions,
	progress *volume.ProgressReporter,
) (*pullPlan, error) {
	ref, err := bdn.ParseRef(opts.Ref)
	if err != nil {
		return nil, err
	}
	progress.SetPhase(volume.PhaseResolve, 0, 0)

	resolved, err := client.Resolve(ctx, ref.String())
	if err != nil {
		return nil, err
	}
	plan := &pullPlan{digest: resolved.Resolved.OriginDigest}
	plan.pinned = ref.Pinned(plan.digest.String())
	plan.origin = newOrigin(client, plan.pinned, resolved.Resolved.OrgID, resolved.Origin)

	body, err := plan.origin.fetch(ctx, opts.DownloadObject, opts.Decompress,
		resolved.Resolved.Target, 0, volume.MaxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", plan.digest, err)
	}
	// Everything a download trusts comes out of these bytes: which files
	// exist, and the digest each chunk is checked against. Verifying every
	// chunk against a manifest nobody verified would authenticate the leaves
	// of the tree against a root taken on faith, so the root is checked first.
	// The digest covers the content, not the stored object, so it is the
	// decompressed body that is hashed.
	if err := verifyBody(opts.NewHasher, body, plan.digest, "manifest"); err != nil {
		return nil, err
	}
	manifest, err := volume.DecodeManifest(body)
	if err != nil {
		return nil, err
	}

	// Containment is judged on the WHOLE manifest, before any narrowing. A
	// subset that leaves out part of a link's chain must not soften the
	// verdict: subset pulls into one destination compose, and two pulls
	// neither of which would permit an escape on its own can assemble one
	// together. The manifest is the unit the rule speaks about.
	containmentWarnings, err := volume.CheckManifestContainment(manifest)
	if err != nil {
		return nil, err
	}
	plan.warnings = containmentWarnings

	plan.totalFiles = int64(len(manifest.Files))
	if plan.manifest, err = volume.SelectEntries(manifest, opts.Include); err != nil {
		return nil, err
	}
	// Checked before anything is created, so a tree that cannot be reproduced
	// faithfully fails with nothing written rather than halfway through.
	dest := filepath.Clean(opts.DestDir)
	if err := volume.CheckPlan(plan.manifest, caseSensitiveFilesystem(probeDirectory(dest, opts.Overwrite))); err != nil {
		return nil, err
	}
	return plan, nil
}

// publish puts the finished tree in its place.
func (p *puller) publish(
	manifest *volume.Manifest,
	root *os.Root,
	workDir, dest string,
	digest volume.Digest,
	staged bool,
) error {
	p.progress.SetPhase(volume.PhasePublish, 0, 0)

	if staged {
		// The stage may hold files from an earlier attempt that selected more
		// than this one does. Publishing it as it stands would put entries
		// into the tree that this download did not ask for.
		if err := p.prune(manifest); err != nil {
			return err
		}
	}
	if err := p.applyDirectoryModes(manifest); err != nil {
		return err
	}

	// Closed here rather than left to the deferred close: Windows will not
	// rename a directory while a handle on it is open, so publishing below
	// would fail with the stage complete and correct. The defer stays for the
	// error paths above, and closing twice is harmless.
	root.Close()

	if staged {
		if err := publishStage(workDir, dest); err != nil {
			return err
		}
		removeStaleStages(dest, digest)
	}
	return nil
}

// puller holds the state one download shares across its files.
type puller struct {
	opts     PullOptions
	origin   *origin
	root     *os.Root
	limits   volume.Concurrency
	limiter  volume.Limiter
	bytes    *volume.ByteGate
	progress *volume.ProgressReporter
	stats    pullStats
}

type pullStats struct {
	fetched atomic.Int64
	reused  atomic.Int64
}

// materialize writes the tree: directories, then symlinks and files.
func (p *puller) materialize(ctx context.Context, manifest *volume.Manifest) error {
	// Directories are created permissive and given their recorded modes at the
	// very end. A directory recorded read-only is a real thing — a frozen
	// asset tree — and applying that mode now would lock out its own contents.
	for _, dir := range manifest.Directories {
		name := filepath.FromSlash(dir.Path)
		if err := p.root.MkdirAll(name, 0o755); err != nil {
			return err
		}
		// A directory left over from a previous attempt already carries its
		// recorded mode, which may forbid writing into it. Its real mode goes
		// back on at the end, along with everything else's.
		if info, err := p.root.Lstat(name); err == nil && info.Mode().Perm()&0o700 != 0o700 {
			if err := p.root.Chmod(name, info.Mode().Perm()|0o700); err != nil {
				return err
			}
		}
	}
	for _, link := range manifest.Symlinks {
		if err := p.writeSymlink(link); err != nil {
			return err
		}
	}
	return p.writeFiles(ctx, manifest.Files)
}

// writeSymlink recreates one link, replacing whatever is in its place. The
// recorded target may be volume-root-absolute; what is created on disk is
// the relative rendering, because relative is the only encoding a kernel
// resolves inside the tree wherever the tree ends up.
//
// A link's recorded modification time is not restored: os.Root has no
// lutimes, and Chtimes on the link's path would follow it and stamp the
// target instead. The time stays in the manifest for readers that can use
// it; the link on disk keeps its creation time.
func (p *puller) writeSymlink(link volume.SymlinkEntry) error {
	name := filepath.FromSlash(link.Path)
	target := link.Target
	if strings.HasPrefix(target, "/") {
		target = volume.RelativeLinkTarget(link.Path, target)
	}
	if err := p.root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	if existing, err := p.root.Readlink(name); err == nil && existing == target {
		return nil
	}
	if err := p.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := p.root.Symlink(target, name); err != nil {
		// Creating a symlink is a privileged operation on Windows unless
		// developer mode is on. The tree cannot be reproduced without it, and
		// a download that quietly skipped links would hand back something that
		// is not what was published.
		return fmt.Errorf("create symlink %s -> %s: %w", link.Path, link.Target, err)
	}
	return nil
}

// writeFiles materializes every file, several at a time.
func (p *puller) writeFiles(ctx context.Context, files []volume.FileEntry) error {
	return forEach(ctx, p.limits.FileJobs, files,
		func(ctx context.Context, _ int, file volume.FileEntry) error {
			if err := p.writeFile(ctx, file); err != nil {
				return fmt.Errorf("write %s: %w", file.Path, err)
			}
			return nil
		})
}

// writeFile materializes one file's contents and applies its mode.
func (p *puller) writeFile(ctx context.Context, entry volume.FileEntry) error {
	chunks, err := p.chunksOf(ctx, entry)
	if err != nil {
		return err
	}

	name := filepath.FromSlash(entry.Path)
	if err := p.root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}

	// A file already the right length may hold the right bytes, from an
	// interrupted attempt or an earlier version of the same tree. Hashing what
	// is there is far cheaper than downloading it again.
	resumable := false
	if info, err := p.root.Lstat(name); err == nil {
		switch {
		case info.Mode().IsRegular():
			resumable = uint64(info.Size()) == entry.Size
		case info.IsDir():
			// Left in place so the open below fails loudly. Removing a whole
			// directory because the manifest wants a file there would destroy
			// far more than this download is entitled to.
		default:
			// Anything else in the way of a regular file: usually a symlink,
			// where an earlier version of this volume had one, but a socket,
			// device node, or fifo left in the destination lands here too and
			// wants the same answer.
			//
			// A symlink is the one that silently corrupts. Opening it writes
			// straight through to whatever it points at — the containment root
			// permits a link that stays inside it — so the bytes and the mode
			// would land on an unrelated file, the link would survive, and the
			// download would report success having published a tree that does
			// not match the manifest. The others would merely fail strangely.
			if err := p.root.Remove(name); err != nil {
				return err
			}
		}
	}

	handle, err := p.root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
	if errors.Is(err, fs.ErrPermission) {
		// A file left from a previous attempt already carries its recorded
		// mode, and a read-only file cannot be opened for writing. Its real
		// mode is applied again below.
		if p.root.Chmod(name, 0o600) == nil {
			handle, err = p.root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
		}
	}
	if err != nil {
		return err
	}
	defer handle.Close()

	// Sizing the file up front lets the chunks be written at their offsets in
	// any order, which is what makes one large file download in parallel.
	if err := handle.Truncate(int64(entry.Size)); err != nil {
		return err
	}

	if err := p.writeChunks(ctx, handle, chunks, resumable); err != nil {
		return err
	}
	if err := handle.Close(); err != nil {
		return err
	}

	// The mode is applied after the contents, because the mode a file is
	// created with is masked by the umask and cannot carry the setuid, setgid,
	// or sticky bits at all.
	if err := p.root.Chmod(name, volume.ModeFromManifest(entry.Mode)); err != nil {
		return err
	}
	// The recorded modification time comes back after the mode, for files
	// that carry one; an entry without a recorded time keeps its write time
	// rather than gaining an invented one.
	if !entry.MTime.IsZero() {
		if err := p.root.Chtimes(name, entry.MTime, entry.MTime); err != nil {
			return err
		}
	}
	p.progress.Add(1, int64(entry.Size))
	return nil
}

// chunksOf lists the chunks a file is made of, reading its chunkmap when it
// has one.
func (p *puller) chunksOf(ctx context.Context, entry volume.FileEntry) ([]volume.ChunkRef, error) {
	switch entry.Kind {
	case volume.FileKindChunk:
		return []volume.ChunkRef{entry.Chunk}, nil
	case volume.FileKindChunkmap:
		// Metadata takes a slot but no share of the byte budget, and
		// contributes no latency sample: it is a different size and shape from
		// a chunk, and would move the baseline a limiter measures against.
		permit, err := p.limiter.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		body, err := p.origin.fetch(ctx, p.opts.DownloadObject, p.opts.Decompress,
			entry.Target, 0, volume.MaxChunkmapBytes)
		if err != nil {
			permit.CompleteUntimed(failureOutcome(ctx, err))
			return nil, err
		}
		permit.CompleteUntimed(volume.Success)

		// The per-chunk digests come out of this object, so a substituted
		// chunkmap makes every wrong chunk verify perfectly against it. The
		// size check below stays, but it is a consistency check rather than
		// an authentication.
		if err := verifyBody(p.opts.NewHasher, body, entry.Digest, "chunkmap"); err != nil {
			return nil, err
		}
		chunkmap, err := volume.DecodeChunkmap(body)
		if err != nil {
			return nil, err
		}
		if chunkmap.FileSize != entry.Size {
			return nil, fmt.Errorf("chunkmap describes %d bytes, the manifest says %d", chunkmap.FileSize, entry.Size)
		}
		return chunkmap.Chunks, nil
	default:
		return nil, fmt.Errorf("%w: kind %q", volume.ErrUnsupportedEntry, entry.Kind)
	}
}

// writeChunks fills the file, spawning one task per chunk once that chunk
// holds both a slot and its bytes of the budget.
func (p *puller) writeChunks(ctx context.Context, handle *os.File, chunks []volume.ChunkRef, resumable bool) error {
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

	for _, chunk := range chunks {
		if chunk.Length == 0 {
			// The empty file's chunk. Sizing the file already produced it, so
			// it was neither fetched nor found already correct — counting it
			// as either would misreport a virgin download.
			continue
		}
		permit, err := p.limiter.Acquire(ctx)
		if err != nil {
			fail(err)
			break
		}
		if err := p.bytes.Acquire(ctx, int64(chunk.Length)); err != nil {
			permit.CompleteUntimed(volume.Neutral)
			fail(err)
			break
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer p.bytes.Release(int64(chunk.Length))

			if err := p.writeChunk(ctx, handle, chunk, resumable, permit); err != nil {
				fail(err)
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// writeChunk puts one chunk in place, downloading it only if what is already
// there is not it.
func (p *puller) writeChunk(
	ctx context.Context,
	handle *os.File,
	chunk volume.ChunkRef,
	resumable bool,
	permit *volume.Permit,
) error {
	if resumable {
		// The check's own buffer: filled from what is already on disk, dead
		// after the rehash. A fresh download allocates nothing here.
		buffer := make([]byte, chunk.Length)
		if _, err := handle.ReadAt(buffer, int64(chunk.Offset)); err == nil {
			digest, err := volume.HashBytes(p.opts.NewHasher, buffer)
			if err == nil && digest == chunk.Digest {
				permit.CompleteUntimed(volume.Neutral)
				p.stats.reused.Add(1)
				return nil
			}
		}
	}

	req, err := p.origin.request(ctx, chunk.Target, int64(chunk.Length))
	if err != nil {
		permit.CompleteUntimed(volume.Neutral)
		return err
	}
	body, err := volume.FetchObject(ctx, p.opts.DownloadObject, p.opts.Decompress, req, int64(chunk.Length))
	if err != nil {
		permit.CompleteUntimed(failureOutcome(ctx, err))
		return err
	}
	permit.Complete(volume.Success)

	// The digest is checked before the bytes reach the file. Writing first and
	// checking after would leave a wrong file on disk for as long as it took
	// to notice, and on a failed download, forever.
	if uint64(len(body)) != chunk.Length {
		return fmt.Errorf("chunk %s is %d bytes, the manifest says %d", chunk.Digest, len(body), chunk.Length)
	}
	digest, err := volume.HashBytes(p.opts.NewHasher, body)
	if err != nil {
		return err
	}
	if digest != chunk.Digest {
		return fmt.Errorf("chunk at offset %d hashes to %s, the manifest says %s", chunk.Offset, digest, chunk.Digest)
	}
	if _, err := handle.WriteAt(body, int64(chunk.Offset)); err != nil {
		return err
	}
	p.stats.fetched.Add(1)
	return nil
}

// applyDirectoryModes sets the recorded mode — and, when one was recorded,
// the modification time — on every directory, deepest first.
//
// Depth order matters twice over: a parent made read-only before its children
// would stop them being touched at all, and changing a child's mode needs
// search permission on every directory above it. The times ride the same
// loop for a reason of their own: it runs after every write into the tree,
// and writing an entry into a directory is what moves the directory's
// mtime — a time stamped before the contents would be stamped over.
func (p *puller) applyDirectoryModes(manifest *volume.Manifest) error {
	dirs := slices.Clone(manifest.Directories)
	// Reverse path order puts a child before its parent, since a child's path
	// is its parent's plus more.
	slices.SortFunc(dirs, func(a, b volume.DirectoryEntry) int { return strings.Compare(b.Path, a.Path) })

	for _, dir := range dirs {
		if err := p.root.Chmod(filepath.FromSlash(dir.Path), volume.ModeFromManifest(dir.Mode)); err != nil {
			return err
		}
		if !dir.MTime.IsZero() {
			if err := p.root.Chtimes(filepath.FromSlash(dir.Path), dir.MTime, dir.MTime); err != nil {
				return err
			}
		}
	}
	return nil
}

// prune removes anything in the staged tree that the plan does not describe.
//
// A stage is keyed by the destination and the version, not by which entries
// were asked for, so an interrupted download of the whole volume leaves a
// stage that a later download of one directory would otherwise publish whole.
func (p *puller) prune(manifest *volume.Manifest) error {
	planned := map[string]bool{".": true}
	for _, dir := range manifest.Directories {
		planned[dir.Path] = true
	}
	for _, file := range manifest.Files {
		planned[file.Path] = true
	}
	for _, link := range manifest.Symlinks {
		planned[link.Path] = true
	}

	var unplanned []string
	err := fs.WalkDir(p.root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if planned[name] {
			return nil
		}
		unplanned = append(unplanned, name)
		if d.IsDir() {
			// Nothing below an unplanned directory can be planned, and the
			// directory is about to go.
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return err
	}

	for _, name := range unplanned {
		if err := p.root.RemoveAll(filepath.FromSlash(name)); err != nil {
			return err
		}
	}
	return nil
}

// caseSensitiveFilesystem reports whether the filesystem holding dir tells
// apart two names differing only in case.
//
// It is measured rather than assumed from the operating system: macOS is
// case-insensitive by default but can be formatted either way, Linux is
// usually case-sensitive but not on every mount, and a volume pushed from one
// is routinely pulled onto the other. The probe writes one file and looks for
// it under a different case.
//
// An unwritable directory answers false, which is the cautious direction: it
// refuses a tree it could have written rather than overwriting a file it
// should not have.
func caseSensitiveFilesystem(dir string) bool {
	probe, err := os.CreateTemp(dir, "volume-case-probe-x*")
	if err != nil {
		return false
	}
	name := probe.Name()
	probe.Close()
	defer os.Remove(name)

	base := filepath.Base(name)
	flipped := filepath.Join(dir, strings.ToUpper(base))
	if flipped == name {
		return false
	}
	// The probe name is lowercase, so the uppercase form exists only if the
	// filesystem folds case.
	_, err = os.Lstat(flipped)
	return errors.Is(err, fs.ErrNotExist)
}

// probeDirectory picks where to run the case probe: the directory the writes
// will actually land in.
//
// For a staged download that is the destination's parent, since the stage is a
// sibling — probing the destination itself would answer for the wrong
// filesystem whenever it is an empty mount point, and the rename that
// publishes the stage would not have worked across that boundary anyway.
func probeDirectory(dest string, overwrite bool) string {
	if overwrite {
		if info, err := os.Stat(dest); err == nil && info.IsDir() {
			return dest
		}
	}
	parent := filepath.Dir(dest)
	if parent == "" {
		return "."
	}
	return parent
}

// stageSuffix separates a destination from the version being staged for it.
const stageSuffix = ".tmp-b3-"

// stagePath is where a download assembles a tree before moving it into place.
//
// The name is derived from the destination and the version, so it is the same
// on every attempt: an interrupted download leaves a stage that the next
// attempt of the same version finds and continues, while a different version
// lands in a stage of its own rather than mixing with it.
//
// Two downloads of the same version into the same destination share the stage
// and will interfere with each other. That is not supported.
func stagePath(dest string, digest volume.Digest) string {
	return dest + stageSuffix + digest.Hex()[:12]
}

// prepareDestination decides where the tree is assembled and makes sure it can
// be. It reports whether the tree is staged, and so needs publishing.
func prepareDestination(dest string, digest volume.Digest, opts PullOptions) (string, bool, error) {
	if opts.Overwrite {
		// Nothing is removed here. The directory is the caller's, and what is
		// already in it either belongs to this version and verifies, or gets
		// overwritten, or is not ours to touch. Validate refuses the Restart
		// combination that would have meant deleting it.
		return dest, false, os.MkdirAll(dest, 0o755)
	}

	if err := checkFreshDestination(dest); err != nil {
		return "", false, err
	}
	stage := stagePath(dest, digest)
	if opts.Restart {
		if err := os.RemoveAll(stage); err != nil {
			return "", false, err
		}
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return "", false, err
	}
	return stage, true, nil
}

// checkFreshDestination refuses a destination that already holds something,
// since publishing over it would replace whatever is there wholesale.
func checkFreshDestination(dest string) error {
	entries, err := os.ReadDir(dest)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty; set Overwrite to write into it", dest)
	}
	return nil
}

// publishStage moves a completed stage into place.
func publishStage(stage, dest string) error {
	// An empty destination directory is in the way of the rename but holds
	// nothing to lose. checkFreshDestination has already refused a destination
	// with anything in it.
	if err := os.Remove(dest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(stage, dest); err != nil {
		return fmt.Errorf("publish %s: %w", dest, err)
	}
	return nil
}

// removeStaleStages deletes stages beside the destination that belong to other
// versions, which are attempts nothing is going to continue now that a version
// has been published here. Failures are ignored: this is tidying, and the
// download has already succeeded.
func removeStaleStages(dest string, published volume.Digest) {
	parent, base := filepath.Split(dest)
	if parent == "" {
		parent = "."
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	keep := filepath.Base(stagePath(dest, published))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() && strings.HasPrefix(name, base+stageSuffix) && name != keep {
			_ = os.RemoveAll(filepath.Join(parent, name))
		}
	}
}
