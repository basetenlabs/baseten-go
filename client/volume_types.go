package client

import (
	"context"
	"hash"
	"io"
)

// The volume vocabulary — options, results, progress, warnings, the
// concurrency knobs, and the seam a caller fills in — is defined in this file,
// fully and without aliases; the error type lives in volume_errors.go and the
// operations are methods on ManagementClient in management_volume.go. The
// translation between these types and the internal engines is deliberately
// exhaustive and field-by-field, and the parity tests hold the two sides
// together: a field added on either side without its twin is a red test.
// The engines' mechanics — the limiter, its permit, its outcome — are never
// public; their semantics changed twice in one review cycle, and neither
// change would have been API-compatible had they been exported.

// VolumeConcurrencyOptions is a transfer's concurrency limits. The zero value
// means defaults: concurrent files follow the pinned object-operation count
// when one is set and otherwise default to 256, in-flight object operations
// adapt to what the origin will bear, and two gibibytes of chunk data may be
// resident in memory.
//
// A wide fan-out is served best by an injected HTTPClient whose transport
// raises MaxIdleConnsPerHost toward the operation ceiling: the default
// transport parks only two idle connections per host, so most requests in a
// wide wave open a fresh connection instead of reusing one.
type VolumeConcurrencyOptions struct {
	// FileJobs is how many files are processed concurrently on push. Zero
	// means the default.
	FileJobs int

	// ChunkOperations pins how many object operations may be in flight. Zero
	// does not mean "some default": it means the limit is not pinned, and a
	// transfer adapts it to what the origin will bear. Setting it caps the
	// load a transfer places on a shared machine or a metered link, and is
	// honoured exactly.
	ChunkOperations int

	// MaxBytesInFlight caps the chunk data resident in memory. Zero means
	// the two-gibibyte default.
	MaxBytesInFlight int64
}

// VolumePhase names the part of a transfer that progress is being reported
// for.
type VolumePhase string

const (
	VolumePhaseScan     VolumePhase = "scan"
	VolumePhaseUpload   VolumePhase = "upload"
	VolumePhaseCommit   VolumePhase = "commit"
	VolumePhaseResolve  VolumePhase = "resolve"
	VolumePhaseDownload VolumePhase = "download"
	VolumePhasePublish  VolumePhase = "publish"
)

// VolumeProgress is a transfer's state at one moment. Counts within a phase
// only ever increase.
type VolumeProgress struct {
	Phase VolumePhase

	Files      int64
	TotalFiles int64

	Bytes      int64
	TotalBytes int64
}

// VolumeObjectCredentials are the short-lived read-only credentials the
// volume service leases for reading a namespace's objects.
type VolumeObjectCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// VolumeObjectDownload names one object to read from the store.
type VolumeObjectDownload struct {
	// Endpoint is empty for AWS itself and a base URL otherwise. The two
	// address buckets differently, which is why the distinction is passed
	// through rather than resolved here.
	Endpoint string
	Region   string
	Bucket   string
	Key      string

	Credentials VolumeObjectCredentials

	// ExpectedSize is the object's size when it is known, and zero when it
	// is not. A chunk's length comes from the manifest, so it is known; a
	// manifest's own size is not.
	ExpectedSize int64
}

// VolumeObjectResult is an open object.
type VolumeObjectResult struct {
	Body io.ReadCloser

	// ContentType is the stored media type, and is the only thing that says
	// how the bytes are encoded. The service decides at write time whether
	// to compress an object, and the key does not record what it chose, so
	// a reader that guessed from the key would eventually guess wrong.
	ContentType string

	// Size is the stored length, which for a compressed object is the
	// compressed length rather than the object's own.
	Size int64
}

// VolumeObjectStore reads stored objects. The caller supplies it so this
// module needs no cloud SDK or compression library of its own; the two
// methods are one interface because they are only ever usable together — the
// service decides per object whether to compress what it stores, so a reader
// that can download but not decompress will fail on an arbitrary subset of
// objects. aws-sdk-go-v2's GetObject and github.com/klauspost/compress/zstd
// fill the two methods.
//
// DownloadObject owns its own retrying: an error returned from it has
// already exhausted whatever budget the implementation has, and ends the
// operation that asked for the object.
type VolumeObjectStore interface {
	// DownloadObject opens one object for reading.
	DownloadObject(ctx context.Context, req VolumeObjectDownload) (*VolumeObjectResult, error)

	// Decompressor wraps a reader of zstd-compressed bytes in one that
	// yields the original bytes.
	Decompressor(r io.Reader) (io.ReadCloser, error)
}

// VolumeWarningKind names what a download's containment check found, so a
// caller can branch without parsing prose.
type VolumeWarningKind uint8

const (
	// VolumeWarningKindDanglingLink: a symlink's target resolves to a path
	// the volume has nothing at.
	VolumeWarningKindDanglingLink VolumeWarningKind = iota + 1
	// VolumeWarningKindLinkThroughFile: a symlink resolves through an entry
	// recorded as a file, which a real filesystem answers with ENOTDIR.
	VolumeWarningKindLinkThroughFile
	// VolumeWarningKindParentUnrecorded: an entry's ancestors up to the
	// nearest recorded one are all implicit — nothing records the parent
	// directory.
	VolumeWarningKindParentUnrecorded
)

// VolumeWarning is a containment finding that did not stop a download:
// harmless to write out, present in volumes published before the containment
// rule, and worth telling the caller about.
type VolumeWarning struct {
	// Path is the entry the finding is about.
	Path string
	// Kind is the finding, typed.
	Kind VolumeWarningKind
	// Detail says what was found, in prose.
	Detail string
}

// String renders the finding for a human; the typed fields are the API.
func (w VolumeWarning) String() string {
	return w.Path + ": " + w.Detail
}

// PushVolumeOptions configures [ManagementClient.PushVolume].
type PushVolumeOptions struct {
	// Namespace and Volume name where to publish. The volume is created if
	// it does not exist; the namespace must already.
	Namespace string
	Volume    string

	// SourceDir is the directory to push. It is walked without following
	// symlinks, which are recorded as links.
	SourceDir string

	// Tags are applied atomically with the publish, so a tag never points at
	// a version that is only partly uploaded. The reserved name "head" is
	// not a tag; see RequireHeadMove.
	Tags []string

	// RequireHeadMove fails the push before it uploads anything if the
	// credential does not carry permission to move the volume's head.
	// Without it, such a push still publishes the version, and refs without
	// a tag keep resolving to the previous one — reported as HeadMoveDenied.
	//
	// See [PushVolumeResult.HeadMoveDenied]: with a credential from this
	// client's own exchange, the condition this guards against is not
	// expected to arise.
	RequireHeadMove bool

	// Hasher returns an unkeyed BLAKE3 hash with a 32-byte digest. Required —
	// the digest is the whole content-addressing scheme, so getting it wrong
	// produces a volume no other client can read, and it is supplied here so
	// this module carries no hashing library of its own. Both common
	// libraries produce the right hash when told the size explicitly or by
	// default:
	//
	//	github.com/zeebo/blake3:   func() hash.Hash { return blake3.New() }
	//	lukechampine.com/blake3:   func() hash.Hash { return blake3.New(32, nil) }
	//
	// A 64-byte extended output is the mistake to watch for; the hasher is
	// checked against the published test vectors before a transfer starts.
	Hasher func() hash.Hash

	// Store lets the push read the volume's previous version, so a file
	// whose bytes have not changed is not uploaded again. Optional: without
	// it the push uploads everything, which is slower and produces the same
	// version.
	Store VolumeObjectStore

	// Progress is called as the push proceeds, never concurrently with
	// itself. It should return promptly.
	Progress func(VolumeProgress)

	// Concurrency overrides the transfer's concurrency limits. The zero
	// value means defaults.
	Concurrency VolumeConcurrencyOptions

	// SourceURI records where the tree came from, and is inside the bytes
	// the version's digest covers: two pushes of identical trees from
	// different paths are different versions. Empty derives it from
	// SourceDir, which means a push from a different absolute path publishes
	// a new version of an unchanged tree. Set it to a stable string to avoid
	// that.
	SourceURI string
}

// PushVolumeResult is what a push published.
type PushVolumeResult struct {
	// ManifestDigest names the published version: the string "b3:" followed
	// by sixty-four lowercase hex digits — an unkeyed BLAKE3 hash with a
	// 32-byte output over the manifest's content bytes. It is what to pin a
	// download to in order to get this exact tree back.
	ManifestDigest string

	// Sequence is the volume's snapshot sequence at this publish.
	Sequence int64

	// HeadUpdated reports whether this push moved head. It is false when
	// head already pointed at this exact version, which is what re-pushing
	// an unchanged tree does — so false does not mean untagged refs resolve
	// elsewhere, only that nothing had to move.
	HeadUpdated bool

	// HeadMoveDenied reports that the push did not ask to move head, because
	// the credential's grants do not cover it. The version was published and
	// can be reached by digest or tag; what did not happen is head moving to
	// it.
	//
	// Not expected to occur with a credential obtained the way this client
	// obtains one. The exchange grants push and tag together or refuses
	// outright, and the credential it mints covers every volume and tag in
	// the namespaces it names — so a credential that can push can also move
	// head. The field describes a real state of the underlying protocol,
	// which admits credentials minted other ways, and reports what this
	// client actually asked for rather than asserting what the service
	// would allow.
	HeadMoveDenied bool

	TagsApplied []string

	// Files and Bytes are what the source tree held.
	Files int64
	Bytes int64

	// Chunks counts every object the push accounted for, and Unique, Reused,
	// and Existing partition it. Reused never reached the network, because a
	// previous version or an earlier file in the same push already had those
	// bytes; Existing was sent and the service already had it.
	Chunks   int64
	Unique   int64
	Reused   int64
	Existing int64
}

// DownloadVolumeOptions configures [ManagementClient.DownloadVolume].
type DownloadVolumeOptions struct {
	// Ref names what to download: "namespace/volume" for the current
	// version, "namespace/volume:tag", or "namespace/volume@b3:..." for one
	// exact version. Whatever it names is resolved once and pinned, so a tag
	// that moves partway through cannot produce a tree assembled from two
	// versions.
	Ref string

	// DestDir is where the tree ends up. Unless Overwrite is set it must not
	// exist or must be empty, and the tree is assembled beside it and moved
	// into place only once it is complete.
	DestDir string

	// Overwrite writes into an existing DestDir in place. Files already
	// there that the volume does not describe are left alone; a failed
	// download leaves a partly written directory rather than an untouched
	// one.
	Overwrite bool

	// Include narrows the download to named entries. Each is an exact path
	// or a directory whose contents are wanted, matched on slash boundaries.
	// One that matches nothing fails the download rather than silently
	// producing less than was asked for. Empty downloads the whole volume.
	Include []string

	// Restart discards a partly downloaded tree from an earlier attempt
	// instead of continuing it.
	Restart bool

	// Hasher returns an unkeyed BLAKE3 hash with a 32-byte digest. Required:
	// every chunk is verified against the digest recorded for it, and
	// continuing an interrupted download works by hashing what is already on
	// disk. It is supplied here so this module carries no hashing library of
	// its own, and getting it wrong fails every verification. Both common
	// libraries produce the right hash when told the size explicitly or by
	// default:
	//
	//	github.com/zeebo/blake3:   func() hash.Hash { return blake3.New() }
	//	lukechampine.com/blake3:   func() hash.Hash { return blake3.New(32, nil) }
	//
	// A 64-byte extended output is the mistake to watch for; the hasher is
	// checked against the published test vectors before a transfer starts.
	Hasher func() hash.Hash

	// Store reads the volume's objects. Required: the service compresses
	// what it stores at its own discretion, so a reader has to be able to
	// download and decompress whatever comes back.
	Store VolumeObjectStore

	// Progress is called as the download proceeds, never concurrently with
	// itself.
	Progress func(VolumeProgress)

	// Concurrency overrides the transfer's concurrency limits. The zero
	// value means defaults.
	Concurrency VolumeConcurrencyOptions
}

// DownloadVolumeResult is what a download produced.
type DownloadVolumeResult struct {
	// VersionRef is the ref pinned to the version that was downloaded, which
	// is the form to quote to get this exact tree again.
	VersionRef string

	// ManifestDigest names the version that was downloaded: the string "b3:"
	// followed by sixty-four lowercase hex digits — an unkeyed BLAKE3 hash
	// with a 32-byte output over the manifest's content bytes.
	ManifestDigest string

	// Files and Bytes are what was written.
	Files int64
	Bytes int64

	// SelectedFiles and TotalFiles report what Include narrowed to. They are
	// equal when the whole volume was downloaded.
	SelectedFiles int64
	TotalFiles    int64

	// ChunksFetched and ChunksReused partition the chunks. Reused were
	// already on disk with the right contents, from an earlier attempt.
	ChunksFetched int64
	ChunksReused  int64

	// Warnings are the containment findings that did not stop the download —
	// a dangling link, a link through a file, an entry whose parent has no
	// record. Volumes published before the containment rule carry these;
	// they are written out faithfully and reported here rather than
	// silently.
	Warnings []VolumeWarning
}
