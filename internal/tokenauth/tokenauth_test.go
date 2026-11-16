package tokenauth

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
)

// newTestCache returns a cache whose mints issue "token-<n>" valid for
// lifetime, and a clock the test moves by hand.
func newTestCache(lifetime, refreshBefore time.Duration) (*Cache, *time.Time, *atomic.Int32) {
	now := time.Now()
	var mints atomic.Int32
	cache := NewCache(CacheOptions{
		Mint: func(context.Context) (Token, error) {
			n := mints.Add(1)
			return Token{Value: fmt.Sprintf("token-%d", n), ExpiresAt: now.Add(lifetime)}, nil
		},
		RefreshBefore: refreshBefore,
		MintTimeout:   time.Second,
		Now:           func() time.Time { return now },
	})
	return cache, &now, &mints
}

func TestCacheRefresh(t *testing.T) {
	t.Run("RefreshBeforeExpiry", func(t *testing.T) {
		cache, now, mints := newTestCache(time.Hour, 5*time.Minute)
		_, err := cache.Token(t.Context())
		require.NoError(t, err)
		*now = now.Add(54*time.Minute + 59*time.Second)
		token, err := cache.Token(t.Context())
		require.NoError(t, err)
		require.Equal(t, "token-1", token.Value)
		*now = now.Add(time.Second)
		token, err = cache.Token(t.Context())
		require.NoError(t, err)
		require.Equal(t, "token-2", token.Value)
		require.Equal(t, int32(2), mints.Load())
	})

	t.Run("CappedAtHalfLifetime", func(t *testing.T) {
		// Without the cap, a five-minute margin on a one-minute token would
		// mint on every call.
		cache, now, mints := newTestCache(time.Minute, 5*time.Minute)
		_, err := cache.Token(t.Context())
		require.NoError(t, err)
		*now = now.Add(29 * time.Second)
		_, err = cache.Token(t.Context())
		require.NoError(t, err)
		require.Equal(t, int32(1), mints.Load())
		*now = now.Add(time.Second)
		_, err = cache.Token(t.Context())
		require.NoError(t, err)
		require.Equal(t, int32(2), mints.Load())
	})

	t.Run("InvalidateKeepsNewerToken", func(t *testing.T) {
		cache, _, _ := newTestCache(time.Hour, time.Minute)
		_, err := cache.Token(t.Context())
		require.NoError(t, err)
		cache.Invalidate("token-0")
		token, err := cache.Token(t.Context())
		require.NoError(t, err)
		require.Equal(t, "token-1", token.Value)
		cache.Invalidate("token-1")
		token, err = cache.Token(t.Context())
		require.NoError(t, err)
		require.Equal(t, "token-2", token.Value)
	})
}

func TestRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		headers map[string]string
		want    bool
	}{
		{"SandboxRevoked", http.StatusUnauthorized, map[string]string{"x-blaxel-error-code": "TOKEN_REVOKED"}, true},
		{"InvalidToken", http.StatusUnauthorized, map[string]string{"WWW-Authenticate": `Bearer realm="baseten", error="invalid_token"`}, true},
		{"PlainUnauthorized", http.StatusUnauthorized, nil, false},
		{"OtherChallenge", http.StatusUnauthorized, map[string]string{"WWW-Authenticate": `Bearer realm="baseten"`}, false},
		{"Forbidden", http.StatusForbidden, map[string]string{"WWW-Authenticate": `Bearer error="invalid_token"`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{}}
			for k, v := range tc.headers {
				resp.Header.Set(k, v)
			}
			require.Equal(t, tc.want, Refused(resp))
		})
	}
}
