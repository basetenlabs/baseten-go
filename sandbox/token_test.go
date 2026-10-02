package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/internal/require"
)

// tokenServer serves /v1/token, minting "token-1", "token-2", and so on, and
// /echo, which reports the Authorization it got, or rejects the tokens in
// revoked as revoked.
type tokenServer struct {
	*httptest.Server
	mints     atomic.Int32
	expiresIn time.Duration
	// mintGate, when set, holds each mint until it receives.
	mintGate chan struct{}
	// mintStarted, when set, is sent to as each mint starts.
	mintStarted chan struct{}
	// failMints fails this many mints before succeeding.
	failMints atomic.Int32

	mu         sync.Mutex
	revoked    map[string]bool
	echoBodies []string
}

func newTokenServer(t *testing.T) *tokenServer {
	t.Helper()
	s := &tokenServer{expiresIn: time.Hour, revoked: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/token", func(w http.ResponseWriter, r *http.Request) {
		if s.mintStarted != nil {
			// Never blocks, so an unexpected extra mint fails the test
			// instead of hanging it.
			select {
			case s.mintStarted <- struct{}{}:
			default:
			}
		}
		if s.mintGate != nil {
			<-s.mintGate
		}
		w.Header().Set("Content-Type", "application/json")
		if s.failMints.Load() > 0 {
			s.failMints.Add(-1)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"code":"INTERNAL_ERROR","message":"mint failed"}`))
			return
		}
		n := s.mints.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("token-%d", n),
			"expires_at": time.Now().Add(s.expiresIn).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		s.echoBodies = append(s.echoBodies, string(body))
		revoked := s.revoked[token]
		s.mu.Unlock()
		if revoked {
			w.Header().Set("x-blaxel-error-code", "TOKEN_REVOKED")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(token))
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *tokenServer) revoke(tokens ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, token := range tokens {
		s.revoked[token] = true
	}
}

func (s *tokenServer) tokens(t *testing.T) *tokenSource {
	t.Helper()
	management, err := client.NewManagementClient(client.ManagementClientOptions{APIKey: "key", BaseURL: s.URL})
	require.NoError(t, err)
	return &tokenSource{management: management}
}

// echo sends a request through the authenticated client and returns its
// status and the token the server saw.
func echo(t *testing.T, c *authenticatedClient, url string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/echo", body)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	seen, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(seen)
}

func TestTokenSource(t *testing.T) {
	t.Run("MintsOnceAndCaches", func(t *testing.T) {
		srv := newTokenServer(t)
		tokens := srv.tokens(t)
		for range 3 {
			token, err := tokens.token(t.Context(), "")
			require.NoError(t, err)
			require.Equal(t, "token-1", token)
		}
		require.Equal(t, int32(1), srv.mints.Load())
	})

	t.Run("RefreshesNearExpiry", func(t *testing.T) {
		srv := newTokenServer(t)
		srv.expiresIn = tokenExpiryLeeway / 2
		tokens := srv.tokens(t)
		first, err := tokens.token(t.Context(), "")
		require.NoError(t, err)
		require.Equal(t, "token-1", first)
		second, err := tokens.token(t.Context(), "")
		require.NoError(t, err)
		require.Equal(t, "token-2", second)
	})

	t.Run("ConcurrentCallersShareOneMint", func(t *testing.T) {
		srv := newTokenServer(t)
		srv.mintStarted = make(chan struct{}, 1)
		srv.mintGate = make(chan struct{})
		tokens := srv.tokens(t)

		results := make(chan string, 5)
		var wg sync.WaitGroup
		for range 5 {
			wg.Go(func() {
				token, err := tokens.token(t.Context(), "")
				if err != nil {
					results <- err.Error()
					return
				}
				results <- token
			})
		}
		<-srv.mintStarted

		// A caller giving up stops waiting without failing the mint.
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := tokens.token(canceled, "")
		require.Equal(t, context.Canceled, err)

		close(srv.mintGate)
		wg.Wait()
		close(results)
		for token := range results {
			require.Equal(t, "token-1", token)
		}
		require.Equal(t, int32(1), srv.mints.Load())
	})

	t.Run("FailedMintNotCached", func(t *testing.T) {
		srv := newTokenServer(t)
		srv.failMints.Store(1)
		tokens := srv.tokens(t)
		_, err := tokens.token(t.Context(), "")
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Equal(t, "INTERNAL_ERROR", apiErr.Code)
		token, err := tokens.token(t.Context(), "")
		require.NoError(t, err)
		require.Equal(t, "token-1", token)
	})

	t.Run("StalledMintTimesOutAndIsNotCached", func(t *testing.T) {
		timeout := tokenMintTimeout
		tokenMintTimeout = 50 * time.Millisecond
		t.Cleanup(func() { tokenMintTimeout = timeout })
		srv := newTokenServer(t)
		srv.mintGate = make(chan struct{})
		// Released before the server closes, even if the test fails first.
		release := sync.OnceFunc(func() { close(srv.mintGate) })
		t.Cleanup(release)
		tokens := srv.tokens(t)
		// No deadline of its own, so only the mint's timeout ends the wait.
		_, err := tokens.token(context.Background(), "")
		require.True(t, errors.Is(err, context.DeadlineExceeded), "expected context.DeadlineExceeded, got %v", err)
		release()
		token, err := tokens.token(t.Context(), "")
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(token, "token-"), "token %q", token)
	})

	t.Run("InvalidateKeepsNewerToken", func(t *testing.T) {
		srv := newTokenServer(t)
		tokens := srv.tokens(t)
		_, err := tokens.token(t.Context(), "")
		require.NoError(t, err)
		tokens.invalidate("token-0")
		token, err := tokens.token(t.Context(), "")
		require.NoError(t, err)
		require.Equal(t, "token-1", token)
		tokens.invalidate("token-1")
		token, err = tokens.token(t.Context(), "")
		require.NoError(t, err)
		require.Equal(t, "token-2", token)
	})
}

func TestAuthenticatedClient(t *testing.T) {
	// Shortened, so the tests that wait take milliseconds. Package state, so
	// these tests must not run in parallel.
	delay := tokenRevokedRetryDelay
	tokenRevokedRetryDelay = 50 * time.Millisecond
	t.Cleanup(func() { tokenRevokedRetryDelay = delay })

	t.Run("ResendsRevokedWithNewToken", func(t *testing.T) {
		srv := newTokenServer(t)
		srv.revoke("token-1")
		c := &authenticatedClient{inner: http.DefaultClient, tokens: srv.tokens(t)}
		started := time.Now()
		status, seen := echo(t, c, srv.URL, strings.NewReader("payload"))
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "token-2", seen)
		// The first resend does not wait.
		require.True(t, time.Since(started) < tokenRevokedRetryDelay, "first resend waited")
		// The body went out whole both times.
		require.Equal(t, "payload", srv.echoBodies[0])
		require.Equal(t, "payload", srv.echoBodies[1])
	})

	t.Run("SecondResendWaitsPastCutoff", func(t *testing.T) {
		srv := newTokenServer(t)
		srv.revoke("token-1", "token-2")
		c := &authenticatedClient{inner: http.DefaultClient, tokens: srv.tokens(t)}
		started := time.Now()
		status, seen := echo(t, c, srv.URL, nil)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "token-3", seen)
		require.True(t, time.Since(started) >= tokenRevokedRetryDelay, "second resend did not wait")
	})

	t.Run("BackoffStopsWhenContextDone", func(t *testing.T) {
		srv := newTokenServer(t)
		srv.revoke("token-1", "token-2")
		c := &authenticatedClient{inner: http.DefaultClient, tokens: srv.tokens(t)}
		ctx, cancel := context.WithTimeout(t.Context(), tokenRevokedRetryDelay/2)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/echo", nil)
		require.NoError(t, err)
		_, err = c.Do(req)
		require.True(t, errors.Is(err, context.DeadlineExceeded), "expected deadline exceeded, got %v", err)
	})

	t.Run("GivesUpAfterMaxRetries", func(t *testing.T) {
		srv := newTokenServer(t)
		srv.revoke("token-1", "token-2", "token-3", "token-4")
		c := &authenticatedClient{inner: http.DefaultClient, tokens: srv.tokens(t)}
		status, _ := echo(t, c, srv.URL, nil)
		require.Equal(t, http.StatusUnauthorized, status)
		require.Equal(t, int32(tokenInvalidationMaxRetries+1), srv.mints.Load())
	})

	t.Run("NonReplayableBodyNotResent", func(t *testing.T) {
		srv := newTokenServer(t)
		srv.revoke("token-1")
		c := &authenticatedClient{inner: http.DefaultClient, tokens: srv.tokens(t)}
		// Wrapped so http.NewRequest cannot set GetBody.
		status, _ := echo(t, c, srv.URL, io.MultiReader(strings.NewReader("stream")))
		require.Equal(t, http.StatusUnauthorized, status)
		require.Len(t, srv.echoBodies, 1)
	})

	t.Run("ProviderToldRevokedToken", func(t *testing.T) {
		srv := newTokenServer(t)
		srv.revoke("provided-1")
		var told []string
		provider := func(_ context.Context, opts TokenProviderOptions) (string, error) {
			told = append(told, opts.RevokedToken)
			if opts.RevokedToken == "provided-1" {
				return "provided-2", nil
			}
			return "provided-1", nil
		}
		c := &authenticatedClient{inner: http.DefaultClient, tokens: &tokenSource{provider: provider}}
		status, seen := echo(t, c, srv.URL, nil)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "provided-2", seen)
		require.Len(t, told, 2)
		require.Equal(t, "", told[0])
		require.Equal(t, "provided-1", told[1])
	})

	t.Run("PlainUnauthorizedNotResent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(srv.Close)
		calls := 0
		provider := func(context.Context, TokenProviderOptions) (string, error) {
			calls++
			return "token", nil
		}
		c := &authenticatedClient{inner: http.DefaultClient, tokens: &tokenSource{provider: provider}}
		status, _ := echo(t, c, srv.URL, nil)
		require.Equal(t, http.StatusUnauthorized, status)
		require.Equal(t, 1, calls)
	})
}
