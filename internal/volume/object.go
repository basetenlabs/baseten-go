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
	return FetchObjectInto(ctx, download, decompress, req, maxSize, nil)
}

// FetchObjectInto is FetchObject reading into the caller's buffer: buf must
// have length zero, and its capacity is what the read fills before falling
// back to growth. The buffer is CALLER-OWNED throughout, and that ownership
// is structural rather than polite: in the overrun fallback, the append
// reallocates (length has met capacity), so the returned slice is NOT the
// caller's array — a caller that released "what it got back" would pool a
// foreign array and lose its own. The caller therefore releases exactly the
// buffer it supplied, never the returned slice, and only after it has
// finished with the bytes. A nil buf allocates per call, sized to
// ExpectedSize, which is the non-pooled path every short or metadata read
// takes.
func FetchObjectInto(
	ctx context.Context,
	download ObjectDownloader,
	decompress Decompressor,
	req ObjectDownload,
	maxSize int64,
	buf []byte,
) ([]byte, error) {
	opened, err := OpenObject(ctx, download, decompress, req)
	if err != nil {
		return nil, err
	}
	defer opened.Close()

	var body io.Reader = opened
	if maxSize > 0 {
		// One byte past the bound distinguishes an object that just fits from
		// one that does not.
		body = io.LimitReader(body, maxSize+1)
	}

	var data []byte
	var err2 error
	if buf != nil {
		data, err2 = readAllSizedInto(body, buf)
	} else {
		data, err2 = readAllSized(body, req.ExpectedSize)
	}
	if err := err2; err != nil {
		return nil, fmt.Errorf("read %s: %w", req.Key, err)
	}
	if maxSize > 0 && int64(len(data)) > maxSize {
		return nil, fmt.Errorf("object %s is larger than the %d byte limit", req.Key, maxSize)
	}
	return data, nil
}

// OpenObject starts a read of one object, yielding the object's own bytes:
// decompressed when the store held it compressed, and as they came otherwise.
// The returned reader must be closed, which closes the decompressor and the
// body beneath it together.
//
// This is what a caller reading an object too large to hold uses; every
// caller that wants the whole thing in memory goes through FetchObject, which
// is this plus a bounded read. Nothing here checks a digest, so a caller that
// trusts what it reads has to hash the stream itself.
func OpenObject(
	ctx context.Context,
	download ObjectDownloader,
	decompress Decompressor,
	req ObjectDownload,
) (io.ReadCloser, error) {
	result, err := download(ctx, req)
	if err != nil {
		return nil, err
	}

	compressed := strings.HasSuffix(result.ContentType, zstdSuffix)

	// A stored object whose length already disagrees with what the manifest
	// says is wrong before any of it is read. The digest check would catch it
	// too, but only after pulling the whole body over the network and hashing
	// it. The comparison is only meaningful uncompressed: a compressed object's
	// stored length is a property of the compressor, not of the content.
	if !compressed && req.ExpectedSize > 0 && result.Size > 0 && result.Size != req.ExpectedSize {
		result.Body.Close()
		return nil, fmt.Errorf("object %s is %d bytes, the manifest says %d",
			req.Key, result.Size, req.ExpectedSize)
	}
	if !compressed {
		return result.Body, nil
	}

	if decompress == nil {
		result.Body.Close()
		return nil, fmt.Errorf("object %s is stored as %s and no decompressor was supplied", req.Key, result.ContentType)
	}
	reader, err := decompress(result.Body)
	if err != nil {
		result.Body.Close()
		return nil, fmt.Errorf("decompress %s: %w", req.Key, err)
	}
	return &decompressedObject{reader: reader, body: result.Body}, nil
}

// decompressedObject is a decompressor over a store body, closing both. The
// decompressor closes first: it may hold buffered reads of the body, and a
// closed body under a live decompressor is what makes those fail.
type decompressedObject struct {
	reader io.ReadCloser
	body   io.ReadCloser
}

func (o *decompressedObject) Read(p []byte) (int, error) { return o.reader.Read(p) }

func (o *decompressedObject) Close() error {
	err := o.reader.Close()
	if bodyErr := o.body.Close(); err == nil {
		err = bodyErr
	}
	return err
}

// readAllSized reads r to EOF. A caller that knows the content's length
// passes it so the buffer is allocated once at that size — for a chunk,
// io.ReadAll's doubling would reallocate around fourteen times and copy the
// content about twice over, all of it garbage but the last. The expected size
// is the content's own length, so it holds for compressed objects too, where
// the stored size says nothing useful.
//
// One byte of spare capacity lets a body running past the expected length
// keep reading rather than fail here: the caller's bound check is what
// decides what an overrun means, and it needs to see the extra byte to fire.
func readAllSized(r io.Reader, size int64) ([]byte, error) {
	if size <= 0 {
		return io.ReadAll(r)
	}
	return readAllSizedInto(r, make([]byte, 0, size+1))
}

// readAllSizedInto is readAllSized filling the caller's storage: buf must
// have length zero and the intended capacity. On the fallback branch the
// append reallocates — length has met capacity — so the returned slice stops
// aliasing buf there, which is why buffer ownership stays with the caller
// (see FetchObjectInto).
func readAllSizedInto(r io.Reader, buf []byte) ([]byte, error) {
	data := buf
	for {
		n, err := r.Read(data[len(data):cap(data)])
		data = data[:len(data)+n]
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return data, err
		}
		if len(data) == cap(data) {
			// Longer than the caller expected. Read the rest the growing
			// way; the caller's size check names what that means.
			rest, err := io.ReadAll(r)
			return append(data, rest...), err
		}
	}
}

// MaxManifestBytes bounds a manifest read into memory. A manifest is one line
// per entry, so this is a very large tree rather than a plausible one; the
// bound exists to fail loudly instead of exhausting memory.
const MaxManifestBytes = 512 << 20

// MaxChunkmapBytes bounds a chunkmap read into memory. A chunkmap describes at
// most one file, at roughly 160 bytes per 8 MiB chunk.
const MaxChunkmapBytes = 64 << 20
