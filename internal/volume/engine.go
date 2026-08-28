package volume

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"
)

// Client is the filesystem and content-addressed transfer engine. All network,
// authentication, endpoint, credential, and storage-key behavior is injected.
type Client struct {
	newHasher               HashFactory
	decoder                 Decoder
	progress                ProgressFunc
	maxConcurrency          int
	maxBytesInFlight        uint64
	destinationReserveBytes uint64
	maxManifestBytes        uint64
	maxFiles                int
	portablePathLimits      portablePathLimits
	availableSpace          func(string) (uint64, error)
}

// New constructs a transfer engine.
func New(options Options) (*Client, error) {
	if err := validateHashFactory(options.NewHasher); err != nil {
		return nil, err
	}

	maxConcurrency := options.MaxConcurrency
	if maxConcurrency == 0 {
		maxConcurrency = defaultMaxConcurrency
	}
	if maxConcurrency < 1 {
		return nil, invalidError("create volume transfer", "MaxConcurrency must be positive")
	}
	destinationReserveBytes := options.MinDestinationFreeBytes
	if destinationReserveBytes == 0 {
		destinationReserveBytes = defaultDestinationReserveBytes
	}
	maxManifestBytes := options.MaxManifestBytes
	if maxManifestBytes == 0 {
		maxManifestBytes = defaultMaxManifestBytes
	}
	if maxManifestBytes > uint64(^uint64(0)>>1)-metadataEncodingOverhead {
		return nil, invalidError("create volume transfer", "MaxManifestBytes is too large")
	}
	largestDecodedObject := max(maxManifestBytes, uint64(ChunkSize))
	decodeLimits := decodeLimits(largestDecodedObject)
	minimumBytesInFlight, overflow := addUint64(
		largestDecodedObject,
		decodeLimits.MaxMemoryBytes,
	)
	if overflow {
		return nil, invalidError("create volume transfer", "MaxManifestBytes is too large")
	}
	maxBytesInFlight := options.MaxBytesInFlight
	if maxBytesInFlight == 0 {
		maxBytesInFlight = max(defaultMaxBytesInFlight, minimumBytesInFlight)
	} else if maxBytesInFlight < minimumBytesInFlight {
		return nil, invalidError(
			"create volume transfer",
			fmt.Sprintf(
				"MaxBytesInFlight must be at least %d bytes for one bounded object decode",
				minimumBytesInFlight,
			),
		)
	}
	maxFiles := options.MaxFiles
	if maxFiles == 0 {
		maxFiles = defaultMaxFiles
	}
	if maxFiles < 1 {
		return nil, invalidError("create volume transfer", "MaxFiles must be positive")
	}
	if uint64(maxFiles) > uint64(^uint32(0)) {
		return nil, invalidError("create volume transfer", "MaxFiles exceeds the manifest format")
	}
	maxPortablePathBytes := options.MaxPortablePathBytes
	if maxPortablePathBytes == 0 {
		maxPortablePathBytes = defaultMaxPortablePathBytes
	}
	if maxPortablePathBytes < 1 {
		return nil, invalidError(
			"create volume transfer",
			"MaxPortablePathBytes must be positive",
		)
	}
	maxPortablePathComponents := options.MaxPortablePathComponents
	if maxPortablePathComponents == 0 {
		maxPortablePathComponents = defaultMaxPortablePathComponents
	}
	if maxPortablePathComponents < 1 {
		return nil, invalidError(
			"create volume transfer",
			"MaxPortablePathComponents must be positive",
		)
	}
	if maxPortablePathComponents > maximumPortablePathComponents {
		return nil, invalidError(
			"create volume transfer",
			fmt.Sprintf(
				"MaxPortablePathComponents must not exceed %d",
				maximumPortablePathComponents,
			),
		)
	}
	pathLimits := portablePathLimits{
		maxPathBytes:      maxPortablePathBytes,
		maxPathComponents: maxPortablePathComponents,
	}

	return &Client{
		newHasher:               options.NewHasher,
		decoder:                 options.Decoder,
		progress:                options.Progress,
		maxConcurrency:          maxConcurrency,
		maxBytesInFlight:        maxBytesInFlight,
		destinationReserveBytes: destinationReserveBytes,
		maxManifestBytes:        maxManifestBytes,
		maxFiles:                maxFiles,
		portablePathLimits:      pathLimits,
		availableSpace:          availableDestinationSpace,
	}, nil
}

func validateHashFactory(factory HashFactory) error {
	if factory == nil {
		return invalidError("create volume transfer", "NewHasher is required")
	}
	first := factory()
	second := factory()
	if !validHashShape(first) || !validHashShape(second) {
		return invalidError(
			"create volume transfer",
			"NewHasher must construct a fresh unkeyed BLAKE3-256 hash",
		)
	}
	if !hashMatches(first, blake3EmptyDigestHex) ||
		!hashMatches(second, blake3EmptyDigestHex) {
		return invalidError(
			"create volume transfer",
			"NewHasher must construct an unkeyed BLAKE3-256 hash",
		)
	}
	if err := writeFullHash(first, []byte("abc")); err != nil {
		return invalidError(
			"create volume transfer",
			"NewHasher returned a hash that cannot consume complete input",
		)
	}
	if !hashMatches(first, blake3ABCDigestHex) {
		return invalidError(
			"create volume transfer",
			"NewHasher must construct an unkeyed BLAKE3-256 hash",
		)
	}
	if !hashMatches(second, blake3EmptyDigestHex) {
		return invalidError(
			"create volume transfer",
			"NewHasher must return a fresh independent hash for each call",
		)
	}
	if err := writeFullHash(second, []byte("abc")); err != nil ||
		!hashMatches(second, blake3ABCDigestHex) {
		return invalidError(
			"create volume transfer",
			"NewHasher returned an invalid hash implementation",
		)
	}
	for _, instance := range []hash.Hash{first, second} {
		instance.Reset()
		if !hashMatches(instance, blake3EmptyDigestHex) {
			return invalidError(
				"create volume transfer",
				"NewHasher returned a hash with invalid Reset behavior",
			)
		}
		if err := writeFullHash(instance, []byte("abc")); err != nil ||
			!hashMatches(instance, blake3ABCDigestHex) {
			return invalidError(
				"create volume transfer",
				"NewHasher returned a hash with invalid Reset behavior",
			)
		}
	}
	return nil
}

func validHashShape(hasher hash.Hash) bool {
	return hasher != nil && hasher.Size() == len(Digest{})
}

func hashMatches(hasher hash.Hash, expectedHex string) bool {
	sum := hasher.Sum(nil)
	return len(sum) == len(Digest{}) && hex.EncodeToString(sum) == expectedHex
}

func writeFullHash(hasher hash.Hash, value []byte) error {
	written, err := hasher.Write(value)
	if written != len(value) {
		return io.ErrShortWrite
	}
	return err
}

func (c *Client) digest(value []byte) (Digest, error) {
	var digest Digest
	hasher := c.newHasher()
	if hasher == nil || hasher.Size() != len(digest) {
		return digest, preconditionError(
			"hash content",
			"hash constructor no longer returns a 32-byte hash",
		)
	}
	if err := writeFullHash(hasher, value); err != nil {
		if errors.Is(err, io.ErrShortWrite) {
			return digest, preconditionError("hash content", "hash implementation reported a short write")
		}
		return digest, preconditionError("hash content", "hash implementation rejected content")
	}
	sum := hasher.Sum(nil)
	if len(sum) != len(digest) {
		return digest, preconditionError("hash content", "hash implementation returned the wrong digest size")
	}
	copy(digest[:], sum)
	return digest, nil
}

func newCorrelationID() (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

type progressReporter struct {
	mu        sync.Mutex
	callback  ProgressFunc
	operation Operation
	lastPhase ProgressPhase
	lastItems uint64
	lastBytes uint64
}

func newProgressReporter(operation Operation, preferred, fallback ProgressFunc) *progressReporter {
	callback := preferred
	if callback == nil {
		callback = fallback
	}
	return &progressReporter{callback: callback, operation: operation}
}

func (r *progressReporter) emit(event ProgressEvent) {
	if r.callback == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	event.Operation = r.operation
	if event.Phase == r.lastPhase {
		event.CompletedItems = max(event.CompletedItems, r.lastItems)
		event.CompletedBytes = max(event.CompletedBytes, r.lastBytes)
	} else {
		r.lastPhase = event.Phase
		r.lastItems = 0
		r.lastBytes = 0
	}
	r.lastItems = event.CompletedItems
	r.lastBytes = event.CompletedBytes
	r.callback(event)
}

func totalPointer(value uint64) *uint64 {
	return &value
}
