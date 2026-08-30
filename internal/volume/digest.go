// Package volume implements the client side of the BDN volume transfer
// protocol: the canonical wire records, source scanning and chunking, and the
// push and pull engines.
//
// The package is network-free. The HTTP protocol client lives in
// [github.com/basetenlabs/baseten-go/internal/volume/bdn], and object storage
// reads are supplied by the caller as a function seam, so nothing here imports
// net/http or an AWS SDK.
package volume

import (
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
)

// DigestSize is the length in bytes of a content digest. Every digest in the
// protocol is an unkeyed BLAKE3 hash truncated to nothing: BLAKE3's natural
// 256-bit output.
const DigestSize = 32

// digestPrefix labels the one hash algorithm the protocol admits. Digests are
// rendered "b3:<64 lowercase hex>"; a "sha256:" digest is not a weaker digest
// but a different namespace, and is rejected rather than accepted.
const digestPrefix = "b3:"

// BLAKE3 known-answer vectors, used to check that an injected hasher really is
// unkeyed BLAKE3-256 before a transfer starts hashing real data. A hasher that
// is subtly wrong (a keyed instance, or a 64-byte extended output) would
// otherwise produce a whole tree of digests no other client can resolve.
const (
	blake3EmptyHex = "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262"
	blake3ABCHex   = "6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85"
)

// ErrHasher reports that the caller's NewHasher does not produce unkeyed
// BLAKE3-256 digests.
var ErrHasher = errors.New("NewHasher must return an unkeyed BLAKE3-256 hash")

// Digest is the BLAKE3-256 hash of an object's uncompressed bytes. Compression
// is a storage decision the server makes and unmakes; identity is always over
// the plain bytes.
type Digest [DigestSize]byte

// Hex returns the digest as 64 lowercase hex characters, without the algorithm
// prefix. Wire records carry [Digest.String]; keys carry Hex.
func (d Digest) Hex() string {
	return hex.EncodeToString(d[:])
}

// String returns the wire form of the digest, "b3:<64 hex>".
func (d Digest) String() string {
	return digestPrefix + d.Hex()
}

// ParseDigest parses the wire form of a digest. It requires the "b3:" prefix
// and exactly 64 lowercase hex characters.
func ParseDigest(s string) (Digest, error) {
	var d Digest
	rest, ok := strings.CutPrefix(s, digestPrefix)
	if !ok {
		return d, fmt.Errorf("digest %q: want %q prefix", s, digestPrefix)
	}
	if len(rest) != 2*DigestSize {
		return d, fmt.Errorf("digest %q: want %d hex characters, got %d", s, 2*DigestSize, len(rest))
	}
	if strings.ToLower(rest) != rest {
		return d, fmt.Errorf("digest %q: hex must be lowercase", s)
	}
	if _, err := hex.Decode(d[:], []byte(rest)); err != nil {
		return d, fmt.Errorf("digest %q: %w", s, err)
	}
	return d, nil
}

// Target names an object by its key relative to the namespace prefix. Readers
// never build a key from a digest: they follow the target a record carries and
// prepend "bdn/{org}/{namespace}/". Keeping the indirection means the server
// can move its key layout without invalidating manifests.
type Target struct {
	RelativeKey string `json:"relative_key"`
}

// TargetForDigest builds the canonical relative key for a content-addressed
// object: "objects/b3/{aa}/{bb}/{hex}", where aa and bb are the first two byte
// pairs of the hex digest. The two levels of fan-out cap any one prefix at
// 256x256 objects.
//
// The push path uses the target the server echoes from an upload rather than
// this function; it exists for tests and for reads that already hold a digest.
func TargetForDigest(d Digest) Target {
	h := d.Hex()
	return Target{RelativeKey: "objects/b3/" + h[0:2] + "/" + h[2:4] + "/" + h}
}

// CheckHasher verifies that newHasher produces unkeyed BLAKE3-256 digests, by
// hashing the empty string and "abc" and comparing against the published test
// vectors. Push and pull call it once per operation, before any real hashing.
func CheckHasher(newHasher func() hash.Hash) error {
	if newHasher == nil {
		return fmt.Errorf("%w: NewHasher is nil", ErrHasher)
	}
	for _, tc := range []struct{ in, want string }{
		{"", blake3EmptyHex},
		{"abc", blake3ABCHex},
	} {
		got, err := HashBytes(newHasher, []byte(tc.in))
		if err != nil {
			return err
		}
		if got.Hex() != tc.want {
			return fmt.Errorf("%w: hash of %q is %s, want b3:%s", ErrHasher, tc.in, got, tc.want)
		}
	}
	return nil
}

// HashBytes returns the digest of b using a fresh hasher from newHasher.
func HashBytes(newHasher func() hash.Hash, b []byte) (Digest, error) {
	h := newHasher()
	if h == nil {
		return Digest{}, fmt.Errorf("%w: NewHasher returned nil", ErrHasher)
	}
	h.Write(b)
	return sumDigest(h)
}

// sumDigest reads a completed hash into a Digest, rejecting any hasher whose
// output is not 32 bytes.
func sumDigest(h hash.Hash) (Digest, error) {
	var d Digest
	sum := h.Sum(nil)
	if len(sum) != DigestSize {
		return d, fmt.Errorf("%w: digest is %d bytes, want %d", ErrHasher, len(sum), DigestSize)
	}
	copy(d[:], sum)
	return d, nil
}
