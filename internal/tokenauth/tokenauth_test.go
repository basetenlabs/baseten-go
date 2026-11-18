package tokenauth

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
)

// newTestCache returns a cache whose mints issue "token-<n>" valid for
// lifetime, and a clock the test moves by hand.
func newTestCache(lifetime, refreshLeeway time.Duration) (*Cache, *time.Time, *atomic.Int32) {
	now := time.Now()
	var mints atomic.Int32
	cache := NewCache(CacheOptions{
		Mint: func(context.Context) (Token, error) {
			n := mints.Add(1)
			return Token{Value: fmt.Sprintf("token-%d", n), ExpiresAt: now.Add(lifetime)}, nil
		},
		RefreshLeeway: refreshLeeway,
		MintTimeout:   time.Second,
		Now:           func() time.Time { return now },
	})
	return cache, &now, &mints
}

func TestCacheRefresh(t *testing.T) {
	t.Run("RefreshLeewayExpiry", func(t *testing.T) {
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

func TestCacheMintFailure(t *testing.T) {
	t.Run("FallsBackToTheStillValidToken", func(t *testing.T) {
		now := time.Now()
		fail := false
		cache := NewCache(CacheOptions{
			Mint: func(context.Context) (Token, error) {
				if fail {
					return Token{}, errors.New("mint failed")
				}
				return Token{Value: "token-1", ExpiresAt: now.Add(time.Hour)}, nil
			},
			RefreshLeeway: 5 * time.Minute,
			MintTimeout:   time.Second,
			Now:           func() time.Time { return now },
		})
		_, err := cache.Token(t.Context())
		require.NoError(t, err)
		fail = true
		now = now.Add(56 * time.Minute)
		token, err := cache.Token(t.Context())
		require.NoError(t, err)
		require.Equal(t, "token-1", token.Value)

		// Once it expires, or once the server refuses it, it is not used.
		cache.Invalidate("token-1")
		_, err = cache.Token(t.Context())
		require.Error(t, err)
	})

	t.Run("NoFallbackOnceExpired", func(t *testing.T) {
		now := time.Now()
		fail := false
		cache := NewCache(CacheOptions{
			Mint: func(context.Context) (Token, error) {
				if fail {
					return Token{}, errors.New("mint failed")
				}
				return Token{Value: "token-1", ExpiresAt: now.Add(time.Hour)}, nil
			},
			RefreshLeeway: 5 * time.Minute,
			MintTimeout:   time.Second,
			Now:           func() time.Time { return now },
		})
		_, err := cache.Token(t.Context())
		require.NoError(t, err)
		fail = true
		now = now.Add(time.Hour)
		_, err = cache.Token(t.Context())
		require.Error(t, err)
	})

	t.Run("WaitersGiveUpAfterOneMoreTry", func(t *testing.T) {
		// Every mint fails. Each caller waits on at most two, so none of them
		// loops on the others' failures.
		var mints atomic.Int32
		release := make(chan struct{})
		cache := NewCache(CacheOptions{
			Mint: func(context.Context) (Token, error) {
				mints.Add(1)
				<-release
				return Token{}, errors.New("mint failed")
			},
			MintTimeout: 10 * time.Second,
		})
		const callers = 20
		errs := make(chan error, callers)
		for range callers {
			go func() {
				_, err := cache.Token(t.Context())
				errs <- err
			}()
		}
		close(release)
		for range callers {
			require.Error(t, <-errs)
		}
		require.True(t, mints.Load() <= callers, "%d mints for %d callers", mints.Load(), callers)
	})
}
