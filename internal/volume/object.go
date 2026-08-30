package volume

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Credentials are the short-lived read-only credentials the volume service
// leases for reading a namespace's objects.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// ObjectDownload names one object to read from the store.
type ObjectDownload struct {
	// Endpoint is empty for AWS itself and a base URL otherwise. The two
	// address buckets differently, which is why the distinction is passed
	// through rather than resolved here.
	Endpoint string
	Region   string
	Bucket   string
	Key      string

	Credentials Credentials

	// ExpectedSize is the object's size when it is known, and zero when it is
	// not. A chunk's length comes from the manifest, so it is known; a
	// manifest's own size is not.
	ExpectedSize int64
}

// ObjectResult is an open object.
type ObjectResult struct {
	Body io.ReadCloser

	// ContentType is the stored media type, and is the only thing that says
	// how the bytes are encoded. The service decides at write time whether to
	// compress a chunk, and the key does not record what it chose, so a reader
	// that guessed from the key would eventually guess wrong.
	ContentType string

	// Size is the stored length, which for a compressed object is the
	// compressed length rather than the object's own.
	Size int64
}

// ObjectDownloader reads one object from the store.
//
// The caller supplies it so this module needs no cloud SDK of its own. It also
// owns retrying at the storage layer: an error returned here has already
// exhausted whatever budget the implementation has, and ends the operation
// that asked for the object.
type ObjectDownloader func(ctx context.Context, req ObjectDownload) (*ObjectResult, error)

// Decompressor wraps a reader of zstd-compressed bytes in one that yields the
// original bytes. The caller supplies it for the same reason as
// ObjectDownloader: it keeps a compression library out of this module.
type Decompressor func(r io.Reader) (io.ReadCloser, error)

// ObjectKey is the full store key of a target within a namespace. Callers
// follow the target a record carries rather than building a key from a digest,
// which is what lets the service change its layout without rewriting history.
func ObjectKey(org, namespace string, t Target) string {
	return fmt.Sprintf("bdn/%s/%s/%s", org, namespace, t.RelativeKey)
}

// zstdSuffix marks a media type whose bytes are compressed.
const zstdSuffix = "+zstd"

// FetchObject reads one object whole and decodes it according to the media
// type the store returned.
//
// maxSize bounds what will be read, so a wrong or hostile length cannot be
// turned into unbounded memory. Zero means no bound, which is only appropriate
// where the size is already known.
func FetchObject(
	ctx context.Context,
	download ObjectDownloader,
	decompress Decompressor,
	req ObjectDownload,
	maxSize int64,
) ([]byte, error) {
	result, err := download(ctx, req)
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()

	compressed := strings.HasSuffix(result.ContentType, zstdSuffix)

	// A stored object whose length already disagrees with what the manifest
	// says is wrong before any of it is read. The digest check would catch it
	// too, but only after pulling the whole body over the network and hashing
	// it. The comparison is only meaningful uncompressed: a compressed object's
	// stored length is a property of the compressor, not of the content.
	if !compressed && req.ExpectedSize > 0 && result.Size > 0 && result.Size != req.ExpectedSize {
		return nil, fmt.Errorf("object %s is %d bytes, the manifest says %d",
			req.Key, result.Size, req.ExpectedSize)
	}

	var body io.Reader = result.Body
	if compressed {
		if decompress == nil {
			return nil, fmt.Errorf("object %s is stored as %s and no decompressor was supplied", req.Key, result.ContentType)
		}
		reader, err := decompress(result.Body)
		if err != nil {
			return nil, fmt.Errorf("decompress %s: %w", req.Key, err)
		}
		defer reader.Close()
		body = reader
	}
	if maxSize > 0 {
		// One byte past the bound distinguishes an object that just fits from
		// one that does not.
		body = io.LimitReader(body, maxSize+1)
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", req.Key, err)
	}
	if maxSize > 0 && int64(len(data)) > maxSize {
		return nil, fmt.Errorf("object %s is larger than the %d byte limit", req.Key, maxSize)
	}
	return data, nil
}

// MaxManifestBytes bounds a manifest read into memory. A manifest is one line
// per entry, so this is a very large tree rather than a plausible one; the
// bound exists to fail loudly instead of exhausting memory.
const MaxManifestBytes = 512 << 20

// MaxChunkmapBytes bounds a chunkmap read into memory. A chunkmap describes at
// most one file, at roughly 160 bytes per 8 MiB chunk.
const MaxChunkmapBytes = 64 << 20
