package volume

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"
)

// HashFactory constructs an unkeyed BLAKE3-256 hash for each digest.
//
// Every call must return a fresh, independent instance that shares no mutable
// state with any other result. The factory must be safe to call concurrently.
// Each hash must consume every byte passed to Write, and Reset must restore the
// empty BLAKE3 state.
type HashFactory func() hash.Hash

// DecodeLimits are mandatory per-object resource limits. MaxMemoryBytes
// includes every decoder-owned allocation, including encoded input copies,
// history, scratch space, and output retained by the decoder.
type DecodeLimits struct {
	MaxEncodedBytes uint64
	MaxDecodedBytes uint64
	MaxWindowBytes  uint64
	MaxMemoryBytes  uint64
}

// Decoder decodes exactly one compressed frame into dst. Implementations must
// consume the complete encoded stream, reject trailing frames or bytes, and
// enforce every supplied limit before allocating beyond it. The package also
// independently bounds bytes read from src and written to dst.
type Decoder interface {
	Decode(context.Context, io.Writer, io.Reader, DecodeLimits) error
}

// DecoderFunc adapts a function to Decoder.
type DecoderFunc func(context.Context, io.Writer, io.Reader, DecodeLimits) error

func (f DecoderFunc) Decode(
	ctx context.Context,
	dst io.Writer,
	src io.Reader,
	limits DecodeLimits,
) error {
	return f(ctx, dst, src, limits)
}

// ObjectEncoding identifies how an object body is encoded at rest.
type ObjectEncoding string

const (
	ObjectEncodingIdentity ObjectEncoding = "identity"
	ObjectEncodingZstd     ObjectEncoding = "zstd"
)

// ObjectRequest identifies content without exposing its backing object key,
// bucket, endpoint, or credentials.
type ObjectRequest struct {
	Digest Digest
	Kind   ObjectKind
}

// Object is one response from a caller-owned content source. Size may be -1
// when the source cannot know it before streaming. Observation may be updated
// synchronously while Body is read; the adapter must finish all updates before
// Body returns EOF or Close returns.
type Object struct {
	Body        io.ReadCloser
	Size        int64
	Kind        ObjectKind
	Encoding    ObjectEncoding
	Observation *TransferObservation
}

// ObjectReader reads objects from a source already scoped and pinned by its
// owner. Credential acquisition, refresh, storage keys, and retries belong in
// the implementation. On error, implementations should return an Object with a
// nil Body and may return AdapterError for a safe classification. The engine
// defensively closes any nonnil Body returned with an error, but adapters
// should not rely on that fallback for normal ownership.
type ObjectReader interface {
	ReadObject(context.Context, ObjectRequest) (Object, error)
}

// ObjectReaderFunc adapts a function to ObjectReader.
type ObjectReaderFunc func(context.Context, ObjectRequest) (Object, error)

func (f ObjectReaderFunc) ReadObject(ctx context.Context, request ObjectRequest) (Object, error) {
	return f(ctx, request)
}

// UploadObject is content handed to a caller-owned object uploader. Body is
// valid only until UploadObject returns and must be consumed synchronously.
type UploadObject struct {
	Digest Digest
	Kind   ObjectKind
	Size   uint64
	Body   io.Reader
}

// UploadObjectResult reports whether the session created new content. A false
// Created value means the operation deduplicated without a data transfer for
// AIMD purposes. Observation must be final when UploadObject returns.
type UploadObjectResult struct {
	Created     bool
	Observation *TransferObservation
}

// ObjectUploader transfers one content-addressed object. Implementations own
// endpoint selection, authentication, retries, and storage details, and may
// return AdapterError for a safe failure classification.
type ObjectUploader interface {
	UploadObject(context.Context, UploadObject) (UploadObjectResult, error)
}

// ObjectUploaderFunc adapts a function to ObjectUploader.
type ObjectUploaderFunc func(context.Context, UploadObject) (UploadObjectResult, error)

func (f ObjectUploaderFunc) UploadObject(
	ctx context.Context,
	object UploadObject,
) (UploadObjectResult, error) {
	return f(ctx, object)
}

// UploadSession is the control boundary for a push. Implementations own session
// creation and renewal, missing-content inventory, publication idempotency, and
// response reconciliation. Publish must follow the PublishResult outcome/error
// contract; in particular, an unknown outcome must be reconciled before retry.
// Boundary failures may use AdapterError. Object transfer is a separate
// ObjectUploader.
type UploadSession interface {
	MissingObjects(context.Context, []Digest) ([]Digest, error)
	Publish(context.Context, Digest) (PublishResult, error)
}

// Operation identifies the requested transfer direction.
type Operation string

const (
	OperationPush Operation = "push"
	OperationPull Operation = "pull"
)

// ProgressPhase is a stable transfer phase.
type ProgressPhase string

const (
	ProgressScan     ProgressPhase = "scan"
	ProgressHash     ProgressPhase = "hash"
	ProgressUpload   ProgressPhase = "upload"
	ProgressValidate ProgressPhase = "validate"
	ProgressDownload ProgressPhase = "download"
	ProgressVerify   ProgressPhase = "verify"
	ProgressPublish  ProgressPhase = "publish"
)

// ProgressEvent reports phase-local progress. Counters reset when Phase
// changes and are monotonic within one phase. It never includes local paths,
// object keys, endpoints, credentials, or content.
type ProgressEvent struct {
	Operation      Operation
	Phase          ProgressPhase
	CompletedItems uint64
	TotalItems     *uint64
	CompletedBytes uint64
	TotalBytes     *uint64
}

// ProgressFunc receives serialized progress events.
type ProgressFunc func(ProgressEvent)

// Options configures transfer behavior.
type Options struct {
	NewHasher HashFactory
	// Decoder is required by Pull and unused by Push.
	Decoder  Decoder
	Progress ProgressFunc

	MaxConcurrency int
	// MaxBytesInFlight bounds resident verified bodies plus decoder memory. A
	// nonzero value must fit one maximally sized configured object decode.
	MaxBytesInFlight uint64
	// MinDestinationFreeBytes is retained after a pull. Zero selects the
	// default reserve.
	MinDestinationFreeBytes uint64
	// MaxManifestBytes also bounds each decoded chunkmap, aggregate decoded
	// chunkmap metadata, and derived graph fanout.
	MaxManifestBytes uint64
	// MaxFiles bounds all explicit entries and synthesized parent directories.
	MaxFiles int
	// MaxPortablePathBytes bounds each manifest path, include selector, and
	// relative symlink target in bytes.
	MaxPortablePathBytes int
	// MaxPortablePathComponents bounds each portable path and symlink expansion.
	// Values above 256 are rejected.
	MaxPortablePathComponents int
}

// PushOptions configures Client.Push.
type PushOptions struct {
	Path     string
	Session  UploadSession
	Uploader ObjectUploader
	Progress ProgressFunc
}

// PushResult describes transferred and published content.
type PushResult struct {
	ManifestDigest Digest
	Publication    PublishResult
	LogicalBytes   uint64
	UploadedBytes  uint64
	ReusedBytes    uint64
	FileCount      uint64
	DirectoryCount uint64
	ContentCreated bool
}

// PullOptions configures Client.Pull. ManifestDigest and Objects must come from
// one pinned resolution; maintaining that invariant belongs to the caller.
type PullOptions struct {
	ManifestDigest Digest
	Objects        ObjectReader
	Destination    string
	Include        []string
	// Restart discards only resumable state matching this manifest, canonical
	// destination, and normalized include set.
	Restart  bool
	Progress ProgressFunc
}

// PullResult describes an atomically published, verified extraction.
type PullResult struct {
	ManifestDigest       Digest
	OutputDirectory      string
	PublicationOutcome   PullPublicationOutcome
	LogicalBytes         uint64
	DownloadedBytes      uint64
	ReusedBytes          uint64
	FileCount            uint64
	DirectoryCount       uint64
	ContentVerified      bool
	VolumeLogicalBytes   *uint64
	VolumeFileCount      *uint64
	VolumeDirectoryCount *uint64
}

// PullPublicationOutcome identifies whether post-rename durability and cleanup
// completed. No PullResult is returned before atomic publication succeeds.
type PullPublicationOutcome string

const (
	PullPublicationComplete PullPublicationOutcome = "complete"
	// PullPublicationIncomplete means verified content is visible, but parent
	// synchronization or private staging-state cleanup did not complete.
	PullPublicationIncomplete PullPublicationOutcome = "published_incomplete"
)

// ErrorCode classifies failures without parsing text.
type ErrorCode string

const (
	ErrorInvalidArgument       ErrorCode = "invalid_argument"
	ErrorPreconditionFailed    ErrorCode = "precondition_failed"
	ErrorUnsupported           ErrorCode = "unsupported"
	ErrorProtocol              ErrorCode = "protocol"
	ErrorIntegrity             ErrorCode = "integrity"
	ErrorTransfer              ErrorCode = "transfer"
	ErrorFilesystem            ErrorCode = "filesystem"
	ErrorCanceled              ErrorCode = "canceled"
	ErrorPublicationFailed     ErrorCode = "publication_failed"
	ErrorPublicationUnknown    ErrorCode = "publication_unknown"
	ErrorPublicationIncomplete ErrorCode = "publication_incomplete"
)

// Error is a detail-safe typed error. Message is authored by this package.
// Arbitrary boundary errors are never rendered or retained. Only canonical
// context errors may be unwrapped; AdapterError is exposed through errors.As.
type Error struct {
	Code      ErrorCode
	Operation string
	Message   string
	cause     error
	adapter   *AdapterError
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := string(e.Code)
	if e.Message != "" {
		message = e.Message
	}
	if e.Operation != "" {
		message = e.Operation + ": " + message
	}
	return message
}

// Format keeps non-message classification state out of alternate formatting.
func (e *Error) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, e.Error())
}

// As exposes a fresh copy of the normalized adapter classification without
// placing adapter-owned errors in the unwrap chain.
func (e *Error) As(target any) bool {
	adapterTarget, ok := target.(**AdapterError)
	if !ok || adapterTarget == nil || e == nil || e.adapter == nil {
		return false
	}
	*adapterTarget = normalizeAdapterError(e.adapter)
	return true
}

// Unwrap preserves only canonical context causes.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// PullPublicationError reports a pull that crossed its atomic publication
// point but did not finish durability or cleanup.
type PullPublicationError struct {
	Result *PullResult

	mu      sync.Mutex
	cleanup func(context.Context) error
	cause   error
}

func (e *PullPublicationError) Error() string {
	if e == nil || e.cause == nil {
		return string(ErrorPublicationIncomplete)
	}
	return e.cause.Error()
}

// Format keeps local result fields out of alternate error formatting.
func (e *PullPublicationError) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, e.Error())
}

func (e *PullPublicationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// RetryCleanup retries only post-publication synchronization and cleanup.
func (e *PullPublicationError) RetryCleanup(ctx context.Context) error {
	if e == nil {
		return nil
	}
	if ctx == nil {
		return invalidError("retry pull cleanup", "context is required")
	}
	if err := ctx.Err(); err != nil {
		return canceledError("retry pull cleanup", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cleanup == nil {
		return nil
	}
	if err := e.cleanup(ctx); err != nil {
		return err
	}
	e.cleanup = nil
	if e.Result != nil {
		e.Result.PublicationOutcome = PullPublicationComplete
	}
	return nil
}

// IsCode reports whether err or a wrapped error has code.
func IsCode(err error, code ErrorCode) bool {
	var typed *Error
	return errors.As(err, &typed) && typed.Code == code
}

func invalidError(operation, message string) error {
	return &Error{Code: ErrorInvalidArgument, Operation: operation, Message: message}
}

func preconditionError(operation, message string) error {
	return &Error{Code: ErrorPreconditionFailed, Operation: operation, Message: message}
}

func unsupportedError(operation, message string) error {
	return &Error{Code: ErrorUnsupported, Operation: operation, Message: message}
}

func protocolError(operation, message string) error {
	return &Error{Code: ErrorProtocol, Operation: operation, Message: message}
}

func integrityError(operation, message string) error {
	return &Error{Code: ErrorIntegrity, Operation: operation, Message: message}
}

func filesystemError(operation, message string) error {
	return &Error{Code: ErrorFilesystem, Operation: operation, Message: message}
}

func transferError(operation string, err error) error {
	classification := classifyBoundaryError(err)
	if classification.contextCause != nil ||
		classification.adapter != nil &&
			classification.adapter.Kind == AdapterErrorKindCancellation {
		return &Error{
			Code:      ErrorCanceled,
			Operation: operation,
			Message:   "operation canceled",
			cause:     classification.contextCause,
			adapter:   classification.adapter,
		}
	}
	return &Error{
		Code:      ErrorTransfer,
		Operation: operation,
		Message:   "caller-supplied transfer operation failed",
		adapter:   classification.adapter,
	}
}

func canceledError(operation string, cause error) error {
	return &Error{
		Code:      ErrorCanceled,
		Operation: operation,
		Message:   "operation canceled",
		cause:     canonicalContextCause(cause),
	}
}

func contextError(operation string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return canceledError(operation, err)
	}
	return nil
}

func readBounded(reader io.Reader, maxBytes int64) ([]byte, error) {
	limited := io.LimitReader(reader, maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, errors.New("input exceeds limit")
	}
	return body, nil
}
