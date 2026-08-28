package volume

import (
	"crypto/sha256"
	"hash"
	"strings"
	"testing"
)

type wrongOutputHash struct {
	hash.Hash
}

func (hasher *wrongOutputHash) Sum(prefix []byte) []byte {
	sum := hasher.Hash.Sum(nil)
	sum[0] ^= 0xff
	return append(prefix, sum...)
}

type shortWriteHash struct {
	hash.Hash
}

func (hasher *shortWriteHash) Write(value []byte) (int, error) {
	written, err := hasher.Hash.Write(value)
	if err != nil || written == 0 {
		return written, err
	}
	return written - 1, nil
}

type badResetHash struct {
	hash.Hash
}

func (*badResetHash) Reset() {}

func TestNewValidatesBLAKE3HashFactory(t *testing.T) {
	shared := newTestHasher()
	tests := []struct {
		name    string
		factory HashFactory
	}{
		{name: "nil factory"},
		{name: "nil result", factory: func() hash.Hash { return nil }},
		{name: "SHA-256", factory: sha256.New},
		{name: "shared instance", factory: func() hash.Hash { return shared }},
		{
			name: "wrong output",
			factory: func() hash.Hash {
				return &wrongOutputHash{Hash: newTestHasher()}
			},
		},
		{
			name: "short write",
			factory: func() hash.Hash {
				return &shortWriteHash{Hash: newTestHasher()}
			},
		},
		{
			name: "bad reset",
			factory: func() hash.Hash {
				return &badResetHash{Hash: newTestHasher()}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(Options{NewHasher: test.factory, Decoder: copyDecoder{}})
			if err == nil || !IsCode(err, ErrorInvalidArgument) {
				t.Fatalf("New error = %v, want %s", err, ErrorInvalidArgument)
			}
		})
	}
}

func TestDigestClassifiesShortHashWrites(t *testing.T) {
	shortWrites := false
	factory := func() hash.Hash {
		if shortWrites {
			return &shortWriteHash{Hash: newTestHasher()}
		}
		return newTestHasher()
	}
	client, err := New(Options{NewHasher: factory, Decoder: copyDecoder{}})
	if err != nil {
		t.Fatal(err)
	}
	shortWrites = true
	_, err = client.digest([]byte("content"))
	if err == nil ||
		!IsCode(err, ErrorPreconditionFailed) ||
		!strings.Contains(err.Error(), "short write") {
		t.Fatalf("short write error = %v, want %s", err, ErrorPreconditionFailed)
	}
}
