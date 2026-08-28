package volume

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

type deterministicTestHash struct {
	bytes.Buffer
}

var (
	deterministicTestEmptyDigest = mustDecodeTestDigest(blake3EmptyDigestHex)
	deterministicTestABCDigest   = mustDecodeTestDigest(blake3ABCDigestHex)
)

func mustDecodeTestDigest(encoded string) [32]byte {
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		panic(err)
	}
	return [32]byte(decoded)
}

func newTestHasher() hash.Hash {
	return &deterministicTestHash{}
}

func (hasher *deterministicTestHash) Sum(prefix []byte) []byte {
	var sum [32]byte
	switch hasher.String() {
	case "":
		sum = deterministicTestEmptyDigest
	case "abc":
		sum = deterministicTestABCDigest
	default:
		sum = sha256.Sum256(append([]byte("volume-test-hash-v1\x00"), hasher.Bytes()...))
	}
	return append(prefix, sum[:]...)
}

func (*deterministicTestHash) Size() int {
	return len(Digest{})
}

func (*deterministicTestHash) BlockSize() int {
	return sha256.BlockSize
}

func testFixtureDigest(value []byte) Digest {
	hasher := newTestHasher()
	_, _ = hasher.Write(value)
	var digest Digest
	copy(digest[:], hasher.Sum(nil))
	return digest
}
