package tokenauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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

func TestRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		headers map[string]string
		want    bool
	}{
		{"SandboxRevoked", http.StatusUnauthorized, map[string]string{"x-blaxel-error-code": "TOKEN_REVOKED"}, true},
		{"InvalidTokenChallenge", http.StatusUnauthorized, map[string]string{"WWW-Authenticate": `Bearer error="invalid_token"`}, false},
		{"PlainUnauthorized", http.StatusUnauthorized, nil, false},
		{"OtherChallenge", http.StatusUnauthorized, map[string]string{"WWW-Authenticate": `Bearer realm="baseten"`}, false},
		{"Forbidden", http.StatusForbidden, map[string]string{"x-blaxel-error-code": "TOKEN_REVOKED"}, false},
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

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func refusal() *http.Response {
	resp := &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}
	resp.Header.Set("x-blaxel-error-code", "TOKEN_REVOKED")
	return resp
}

func TestDo(t *testing.T) {
	newGet := func() (func(context.Context, string) (string, error), *[]string) {
		var refusedSeen []string
		n := 0
		return func(_ context.Context, refused string) (string, error) {
			refusedSeen = append(refusedSeen, refused)
			n++
			return fmt.Sprintf("token-%d", n), nil
		}, &refusedSeen
	}

	t.Run("ResendsWithANewTokenAndTheSameBody", func(t *testing.T) {
		get, refused := newGet()
		var seen []string
		next := doerFunc(func(req *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(req.Body)
			seen = append(seen, req.Header.Get("Authorization")+" "+string(body))
			if len(seen) == 1 {
				return refusal(), nil
			}
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		})
		req, err := http.NewRequest(http.MethodPost, "https://example.com", strings.NewReader("payload"))
		require.NoError(t, err)
		resp, err := Do(next, req, get, nil, 0)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "Bearer token-1 payload,Bearer token-2 payload", strings.Join(seen, ","))
		require.Equal(t, ",token-1", strings.Join(*refused, ","))
		require.Equal(t, "", req.Header.Get("Authorization"))
	})

	t.Run("StopsAfterTwoResends", func(t *testing.T) {
		get, _ := newGet()
		sends := 0
		next := doerFunc(func(*http.Request) (*http.Response, error) {
			sends++
			return refusal(), nil
		})
		req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
		require.NoError(t, err)
		resp, err := Do(next, req, get, nil, 0)
		require.NoError(t, err)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		require.Equal(t, 3, sends)
	})

	t.Run("NeverResendsABodyItCannotRebuild", func(t *testing.T) {
		get, _ := newGet()
		sends := 0
		next := doerFunc(func(*http.Request) (*http.Response, error) {
			sends++
			return refusal(), nil
		})
		req, err := http.NewRequest(http.MethodPost, "https://example.com", io.NopCloser(strings.NewReader("stream")))
		require.NoError(t, err)
		require.True(t, req.GetBody == nil, "GetBody is set")
		var invalidated []string
		resp, err := Do(next, req, get, func(token string) { invalidated = append(invalidated, token) }, 0)
		require.NoError(t, err)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		require.Equal(t, 1, sends)
		// Not sent again, but the refused token is still dropped so later
		// requests do not reuse it.
		require.Equal(t, "token-1", strings.Join(invalidated, ","))
	})

	t.Run("OtherUnauthorizedIsNotResent", func(t *testing.T) {
		get, _ := newGet()
		sends := 0
		next := doerFunc(func(*http.Request) (*http.Response, error) {
			sends++
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: http.NoBody}, nil
		})
		req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
		require.NoError(t, err)
		_, err = Do(next, req, get, nil, 0)
		require.NoError(t, err)
		require.Equal(t, 1, sends)
	})
}
