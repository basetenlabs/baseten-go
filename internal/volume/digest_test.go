package volume

import (
	"crypto/sha256"
	"errors"
	"hash"
	"testing"

	"github.com/basetenlabs/baseten-go/internal/require"
)

// truncatedHash is a hash whose output is the wrong length, standing in for
// the mistake CheckHasher exists to catch: a BLAKE3 constructor asked for an
// extended or shortened digest.
type truncatedHash struct {
	hash.Hash
	size int
}

func (t truncatedHash) Sum(b []byte) []byte { return t.Hash.Sum(b)[:t.size] }

func TestCheckHasherRejectsWrongHashes(t *testing.T) {
	tests := map[string]func() hash.Hash{
		"nil factory":     nil,
		"nil hasher":      func() hash.Hash { return nil },
		"wrong algorithm": sha256.New,
		"short digest": func() hash.Hash {
			return truncatedHash{Hash: sha256.New(), size: 16}
		},
	}
	for name, newHasher := range tests {
		t.Run(name, func(t *testing.T) {
			err := CheckHasher(newHasher)
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrHasher), "error %v should wrap ErrHasher", err)
		})
	}
}
