// Package volume implements internal content-addressed volume transfers.
//
// It owns traversal, canonical records, hashing, validation, resume, extraction,
// and atomic publication. Callers inject upload sessions, object reads, BLAKE3,
// and bounded decompression; API clients, credentials, endpoints, and object
// storage details remain outside this package.
//
// Push preserves source symlinks on Linux and macOS without following them.
// Push and pull fail closed as unsupported on other platforms because their
// mutation-detection and atomic-publication guarantees are unavailable there.
// File and directory publication intentionally preserves only permission bits
// 0777; setuid, setgid, sticky, and other special bits are discarded because
// they are neither portable nor safe to materialize from untrusted metadata.
//
// Pull rewalks the exact descriptor-rooted staging tree, rehashes file content,
// and revalidates entry identities, modes, and symlink targets immediately
// before atomic publication. This narrows the verification-to-rename window but
// is not a kernel-enforced lease: code running as the same effective user (or
// with greater privilege) can still race in-place mutation. Such code is inside
// the local trust boundary.
//
// Data-path transfers use an AIMD request gate with latency-gradient soft cuts
// and a separate byte gate for resident transfer and decoder memory. Injected
// adapters report hidden retries and storage-neutral stall signals through
// TransferObservation on results and AdapterError on failures, so retries
// cannot masquerade as clean AIMD successes.
//
// UploadSession is intentionally only an already-created publication session:
// API resolution, session creation and renewal, heartbeat, credentials,
// endpoints, and retry policy remain in caller-owned adapters. Publication
// outcomes are explicit because Unknown must be reconciled before retry.
package volume

const (
	// ChunkSize is the canonical logical chunk size.
	ChunkSize = 8 << 20

	// MaxMissingDigests is the largest object-presence inventory request.
	MaxMissingDigests = 4096

	defaultMaxManifestBytes          = 64 << 20
	defaultMaxFiles                  = 50_000
	defaultMaxPortablePathBytes      = 4 << 10
	maximumPortablePathComponents    = 256
	defaultMaxPortablePathComponents = maximumPortablePathComponents
	defaultMaxConcurrency            = 64
	defaultMaxBytesInFlight          = 256 << 20
	defaultDestinationReserveBytes   = 512 << 20
	metadataEncodingOverhead         = 1 << 20
	minZstdResourceBytes             = 1 << 20
	defaultManifestSchema            = "v1"
	defaultSourceFingerprint         = "local"
	defaultFingerprintType           = "local_push"
	defaultLocalSourceURI            = "local://push"
	chunkmapFanoutBudgetBytes        = 128
	contentGraphChunkBudgetBytes     = 64
)
