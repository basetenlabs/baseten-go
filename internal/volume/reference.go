package volume

import (
	"encoding/hex"
	"encoding/json"
	"strings"
)

const (
	blake3EmptyDigestHex = "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262"
	blake3ABCDigestHex   = "6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85"
)

var blake3EmptyDigest = Digest{
	0xaf, 0x13, 0x49, 0xb9, 0xf5, 0xf9, 0xa1, 0xa6,
	0xa0, 0x40, 0x4d, 0xea, 0x36, 0xdc, 0xc9, 0x49,
	0x9b, 0xcb, 0x25, 0xc9, 0xad, 0xc1, 0x12, 0xb7,
	0xcc, 0x9a, 0x93, 0xca, 0xe4, 0x1f, 0x32, 0x62,
}

// Digest is a canonical BLAKE3 content digest.
type Digest [32]byte

// ParseDigest parses b3 followed by exactly 64 hexadecimal characters.
func ParseDigest(value string) (Digest, error) {
	var digest Digest
	if !strings.HasPrefix(value, "b3:") {
		return digest, invalidError("parse digest", "digest must use the b3 prefix")
	}
	encoded := value[len("b3:"):]
	if len(encoded) != hex.EncodedLen(len(digest)) {
		return digest, invalidError("parse digest", "digest must contain 64 hexadecimal characters")
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return digest, invalidError("parse digest", "digest contains non-hexadecimal characters")
	}
	copy(digest[:], decoded)
	return digest, nil
}

// Hex returns the lowercase digest without its b3 prefix.
func (d Digest) Hex() string {
	return hex.EncodeToString(d[:])
}

func (d Digest) String() string {
	return "b3:" + d.Hex()
}

// MarshalText implements encoding.TextMarshaler.
func (d Digest) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Digest) UnmarshalText(text []byte) error {
	parsed, err := ParseDigest(string(text))
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// MarshalJSON encodes the digest as its canonical string.
func (d Digest) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON decodes a canonical digest string.
func (d *Digest) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return invalidError("decode digest", "digest must be a JSON string")
	}
	return d.UnmarshalText([]byte(value))
}

// ObjectKind is the semantic kind of a content-addressed object.
type ObjectKind string

const (
	ObjectKindChunk    ObjectKind = "chunk"
	ObjectKindChunkmap ObjectKind = "chunkmap"
	ObjectKindBlob     ObjectKind = "blob"
	ObjectKindManifest ObjectKind = "manifest"
)

type objectTarget struct {
	RelativeKey string `json:"relative_key"`
}

func targetForDigest(digest Digest) objectTarget {
	encoded := digest.Hex()
	return objectTarget{
		RelativeKey: "objects/b3/" + encoded[:2] + "/" + encoded[2:4] + "/" + encoded,
	}
}

func validateTarget(target objectTarget, digest Digest) error {
	expected := targetForDigest(digest).RelativeKey
	if target.RelativeKey != expected {
		return protocolError("validate object target", "object target is not canonical")
	}
	return nil
}
