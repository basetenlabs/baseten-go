package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
)

// fakeTokenServer is a stand-in for POST /v1/auth/token that issues
// "access-<n>" for the nth exchange and records each request's assertion.
type fakeTokenServer struct {
	t          *testing.T
	exchanges  atomic.Int64
	expiresIn  int64
	mu         sync.Mutex
	assertions []string
	// respond, when set, overrides the success response.
	respond func(w http.ResponseWriter)
}

func (s *fakeTokenServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/auth/token" {
		s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		s.t.Errorf("Content-Type = %q", got)
	}
	if got := r.Header.Get("Authorization"); got != "" {
		s.t.Errorf("exchange carried Authorization %q", got)
	}
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.t.Errorf("decoding body: %v", err)
	}
	if len(body) != 3 || body["grant_type"] != jwtBearerGrantType || body["trust_rule_id"] != "rule-1" {
		s.t.Errorf("unexpected body %v", body)
	}
	s.mu.Lock()
	s.assertions = append(s.assertions, body["assertion"])
	s.mu.Unlock()
	n := s.exchanges.Add(1)
	if s.respond != nil {
		s.respond(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"access_token":"access-%d","token_type":"Bearer","expires_in":%d}`, n, s.expiresIn)
}

func newTestCredentials(t *testing.T, srv *httptest.Server, opts WorkloadIdentityCredentialsOptions) *WorkloadIdentityCredentials {
	t.Helper()
	opts.BaseURL = srv.URL
	opts.HTTPClient = srv.Client()
	if opts.TrustRuleID == "" {
		opts.TrustRuleID = "rule-1"
	}
	if opts.IdentityToken == nil && opts.IdentityTokenFile == "" {
		opts.IdentityToken = func(context.Context) (string, error) { return "idp-jwt", nil }
	}
	creds, err := NewWorkloadIdentityCredentials(opts)
	require.NoError(t, err)
	return creds
}

func TestWorkloadIdentityCredentials(t *testing.T) {
	ctx := context.Background()

	t.Run("ExchangesAndCaches", func(t *testing.T) {
		fake := &fakeTokenServer{t: t, expiresIn: 3600}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		creds := newTestCredentials(t, srv, WorkloadIdentityCredentialsOptions{})

		before := time.Now()
		tok, err := creds.AccessToken(ctx)
		require.NoError(t, err)
		require.Equal(t, "access-1", tok.Token)
		require.True(t, !tok.ExpiresAt.Before(before.Add(time.Hour)), "ExpiresAt %v too early", tok.ExpiresAt)

		tok, err = creds.AccessToken(ctx)
		require.NoError(t, err)
		require.Equal(t, "access-1", tok.Token)
		require.Equal(t, int64(1), fake.exchanges.Load())
		require.Equal(t, "idp-jwt", fake.assertions[0])
	})

	t.Run("RefreshesWithinLeeway", func(t *testing.T) {
		fake := &fakeTokenServer{t: t, expiresIn: 600}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		creds := newTestCredentials(t, srv, WorkloadIdentityCredentialsOptions{RefreshLeeway: time.Minute})
		now := time.Now()
		creds.now = func() time.Time { return now }

		_, err := creds.AccessToken(ctx)
		require.NoError(t, err)

		now = now.Add(8*time.Minute + 59*time.Second)
		tok, err := creds.AccessToken(ctx)
		require.NoError(t, err)
		require.Equal(t, "access-1", tok.Token)

		now = now.Add(time.Second)
		tok, err = creds.AccessToken(ctx)
		require.NoError(t, err)
		require.Equal(t, "access-2", tok.Token)
	})

	t.Run("LeewayCappedAtHalfLifetime", func(t *testing.T) {
		fake := &fakeTokenServer{t: t, expiresIn: 60}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		// The default two-minute leeway exceeds the lifetime; without the cap
		// every call would exchange.
		creds := newTestCredentials(t, srv, WorkloadIdentityCredentialsOptions{})
		now := time.Now()
		creds.now = func() time.Time { return now }

		_, err := creds.AccessToken(ctx)
		require.NoError(t, err)
		now = now.Add(29 * time.Second)
		_, err = creds.AccessToken(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(1), fake.exchanges.Load())
		now = now.Add(time.Second)
		_, err = creds.AccessToken(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(2), fake.exchanges.Load())
	})

	t.Run("RereadsTokenFileOnEveryExchange", func(t *testing.T) {
		fake := &fakeTokenServer{t: t, expiresIn: 600}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		path := filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(path, []byte("jwt-one\n"), 0o600))
		creds := newTestCredentials(t, srv, WorkloadIdentityCredentialsOptions{IdentityTokenFile: path})
		now := time.Now()
		creds.now = func() time.Time { return now }

		_, err := creds.AccessToken(ctx)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, []byte("jwt-two"), 0o600))
		now = now.Add(10 * time.Minute)
		_, err = creds.AccessToken(ctx)
		require.NoError(t, err)
		require.Len(t, fake.assertions, 2)
		require.Equal(t, "jwt-one", fake.assertions[0])
		require.Equal(t, "jwt-two", fake.assertions[1])
	})

	t.Run("UnreadableTokenFile", func(t *testing.T) {
		fake := &fakeTokenServer{t: t, expiresIn: 600}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		path := filepath.Join(t.TempDir(), "missing")
		creds := newTestCredentials(t, srv, WorkloadIdentityCredentialsOptions{IdentityTokenFile: path})

		_, err := creds.AccessToken(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "reading identity token file")
		require.True(t, errors.Is(err, os.ErrNotExist), "want ErrNotExist, got %v", err)
		require.Equal(t, int64(0), fake.exchanges.Load())
	})

	t.Run("EmptyTokenFile", func(t *testing.T) {
		srv := httptest.NewServer(&fakeTokenServer{t: t, expiresIn: 600})
		defer srv.Close()
		path := filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(path, []byte("\n"), 0o600))
		creds := newTestCredentials(t, srv, WorkloadIdentityCredentialsOptions{IdentityTokenFile: path})

		_, err := creds.AccessToken(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "is empty")
	})

	t.Run("InvalidGrant", func(t *testing.T) {
		fake := &fakeTokenServer{t: t, respond: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
		}}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		creds := newTestCredentials(t, srv, WorkloadIdentityCredentialsOptions{})

		_, err := creds.AccessToken(ctx)
		require.True(t, errors.Is(err, ErrInvalidGrant), "want ErrInvalidGrant, got %v", err)
		require.Contains(t, err.Error(), "rule-1")

		// A refusal is not cached: the next call exchanges again.
		_, err = creds.AccessToken(ctx)
		require.True(t, errors.Is(err, ErrInvalidGrant), "want ErrInvalidGrant, got %v", err)
		require.Equal(t, int64(2), fake.exchanges.Load())
	})

	t.Run("NonJSONResponse", func(t *testing.T) {
		srv := httptest.NewServer(&fakeTokenServer{t: t, respond: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "<html>bad gateway</html>")
		}})
		defer srv.Close()
		creds := newTestCredentials(t, srv, WorkloadIdentityCredentialsOptions{})

		_, err := creds.AccessToken(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "HTTP 502 with a non-JSON response")
		require.Contains(t, err.Error(), "bad gateway")
	})

	t.Run("OtherErrorStatus", func(t *testing.T) {
		srv := httptest.NewServer(&fakeTokenServer{t: t, respond: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"rate_limited"}`)
		}})
		defer srv.Close()
		creds := newTestCredentials(t, srv, WorkloadIdentityCredentialsOptions{})

		_, err := creds.AccessToken(ctx)
		require.Error(t, err)
		require.False(t, errors.Is(err, ErrInvalidGrant), "429 is not invalid_grant")
		require.Contains(t, err.Error(), "HTTP 429")
	})

	t.Run("MalformedSuccess", func(t *testing.T) {
		for name, body := range map[string]string{
			"NoAccessToken": `{"token_type":"Bearer","expires_in":60}`,
			"WrongType":     `{"access_token":"a","token_type":"MAC","expires_in":60}`,
			"NoExpiry":      `{"access_token":"a","token_type":"Bearer"}`,
		} {
			t.Run(name, func(t *testing.T) {
				srv := httptest.NewServer(&fakeTokenServer{t: t, respond: func(w http.ResponseWriter) {
					fmt.Fprint(w, body)
				}})
				defer srv.Close()
				_, err := newTestCredentials(t, srv, WorkloadIdentityCredentialsOptions{}).AccessToken(ctx)
				require.Error(t, err)
			})
		}
	})

	t.Run("ConcurrentCallersShareOneExchange", func(t *testing.T) {
		release := make(chan struct{})
		fake := &fakeTokenServer{t: t, expiresIn: 3600}
		fake.respond = func(w http.ResponseWriter) {
			<-release
			fmt.Fprint(w, `{"access_token":"shared","token_type":"Bearer","expires_in":3600}`)
		}
		srv := httptest.NewServer(fake)
		defer srv.Close()
		creds := newTestCredentials(t, srv, WorkloadIdentityCredentialsOptions{})

		const callers = 32
		var wg sync.WaitGroup
		errs := make(chan error, callers)
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tok, err := creds.AccessToken(ctx)
				if err == nil && tok.Token != "shared" {
					err = fmt.Errorf("got token %q", tok.Token)
				}
				errs <- err
			}()
		}
		require.Eventually(t, 5*time.Second, 5*time.Millisecond, func(t require.T) {
			if fake.exchanges.Load() != 1 {
				t.Fatalf("exchange not started yet")
			}
		})
		close(release)
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		require.Equal(t, int64(1), fake.exchanges.Load())
	})

	t.Run("WaiterHonorsContext", func(t *testing.T) {
		release := make(chan struct{})
		fake := &fakeTokenServer{t: t}
		fake.respond = func(w http.ResponseWriter) { <-release }
		srv := httptest.NewServer(fake)
		defer srv.Close()
		// Runs before srv.Close, which waits for the blocked handler.
		defer close(release)
		creds := newTestCredentials(t, srv, WorkloadIdentityCredentialsOptions{})

		go creds.AccessToken(ctx) //nolint:errcheck // holds the lock until release
		require.Eventually(t, 5*time.Second, 5*time.Millisecond, func(t require.T) {
			if fake.exchanges.Load() != 1 {
				t.Fatalf("exchange not started yet")
			}
		})
		waitCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		_, err := creds.AccessToken(waitCtx)
		require.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
	})
}

func TestNewWorkloadIdentityCredentialsValidation(t *testing.T) {
	provider := func(context.Context) (string, error) { return "jwt", nil }
	for name, opts := range map[string]WorkloadIdentityCredentialsOptions{
		"NoIdentityToken":   {TrustRuleID: "r"},
		"BothIdentityToken": {TrustRuleID: "r", IdentityTokenFile: "/f", IdentityToken: provider},
		"NoTrustRule":       {IdentityToken: provider},
		"NegativeLeeway":    {TrustRuleID: "r", IdentityToken: provider, RefreshLeeway: -time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewWorkloadIdentityCredentials(opts)
			require.Error(t, err)
		})
	}
}

// TestClientsWithWorkloadIdentityCredentials checks that the management and
// inference clients fetch the bearer token per request, so a refreshed token
// is used without rebuilding the client.
func TestClientsWithWorkloadIdentityCredentials(t *testing.T) {
	ctx := context.Background()
	fake := &fakeTokenServer{t: t, expiresIn: 600}
	var mu sync.Mutex
	var seenAuth []string
	mux := http.NewServeMux()
	mux.Handle("/v1/auth/token", fake)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"org_id":"o","created_at":"2026-01-01T00:00:00Z","allow_personal_api_keys":true,"allow_service_account_api_keys":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	creds := newTestCredentials(t, srv, WorkloadIdentityCredentialsOptions{})
	now := time.Now()
	var nowMu sync.Mutex
	creds.now = func() time.Time { nowMu.Lock(); defer nowMu.Unlock(); return now }

	mgmt, err := NewManagementClient(ManagementClientOptions{
		Credentials: creds,
		BaseURL:     srv.URL,
		HTTPClient:  srv.Client(),
	})
	require.NoError(t, err)
	_, err = mgmt.API().GetOrganizationsMe(ctx)
	require.NoError(t, err)

	nowMu.Lock()
	now = now.Add(time.Hour)
	nowMu.Unlock()
	_, err = mgmt.API().GetOrganizationsMe(ctx)
	require.NoError(t, err)

	inf, err := NewInferenceClient(InferenceClientOptions{
		Credentials: creds,
		BaseURL:     srv.URL,
		HTTPClient:  srv.Client(),
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/anything", nil)
	require.NoError(t, err)
	resp, err := inf.API().HTTPClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seenAuth, 3)
	require.Equal(t, "Bearer access-1", seenAuth[0])
	require.Equal(t, "Bearer access-2", seenAuth[1])
	require.Equal(t, "Bearer access-2", seenAuth[2])
}

func TestBearerAuthDoerLeavesOtherHostsAlone(t *testing.T) {
	var gotAuth atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
	}))
	defer other.Close()
	source := accessTokenSourceFunc(func(context.Context) (AccessToken, error) {
		t.Error("token fetched for a foreign host")
		return AccessToken{}, errors.New("unexpected")
	})
	doer, err := newBearerAuthDoer(other.Client(), source, "https://api.baseten.co")
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPut, other.URL+"/presigned", nil)
	require.NoError(t, err)
	resp, err := doer.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, "", gotAuth.Load().(string))
}

func TestClientAuthOptionsValidation(t *testing.T) {
	creds, err := NewWorkloadIdentityCredentials(WorkloadIdentityCredentialsOptions{
		TrustRuleID:   "r",
		IdentityToken: func(context.Context) (string, error) { return "jwt", nil },
	})
	require.NoError(t, err)

	_, err = NewManagementClient(ManagementClientOptions{Credentials: creds, APIKey: "k"})
	require.Error(t, err)
	_, err = NewManagementClient(ManagementClientOptions{Credentials: creds, DeferAuth: true})
	require.Error(t, err)
	_, err = NewManagementClient(ManagementClientOptions{
		Credentials: creds,
		Headers:     http.Header{"Authorization": {"Bearer x"}},
	})
	require.Error(t, err)
	_, err = NewInferenceClient(InferenceClientOptions{Credentials: creds, APIKey: "k", ModelID: "m"})
	require.Error(t, err)
	_, err = NewInferenceClient(InferenceClientOptions{Credentials: creds, ModelID: "m"})
	require.NoError(t, err)
}

type accessTokenSourceFunc func(context.Context) (AccessToken, error)

func (f accessTokenSourceFunc) AccessToken(ctx context.Context) (AccessToken, error) { return f(ctx) }
