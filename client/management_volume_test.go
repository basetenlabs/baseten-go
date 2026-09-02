package client

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
	"github.com/basetenlabs/baseten-go/internal/volume/bdn"
)

func stubHasher() hash.Hash { return sha256.New() }

// stubStore satisfies the public seam without doing anything, for option
// translation and validation tests.
type stubStore struct{}

func (stubStore) DownloadObject(context.Context, VolumeObjectDownload) (*VolumeObjectResult, error) {
	return nil, nil
}

func (stubStore) Decompressor(io.Reader) (io.ReadCloser, error) { return nil, nil }

func TestPushOptionsValidate(t *testing.T) {
	valid := PushVolumeOptions{
		Namespace: "models", Volume: "gpt2", SourceDir: "/tmp/tree", Hasher: stubHasher,
	}
	require.NoError(t, pushOptions(valid).Validate())

	// The old shape allowed a downloader without a decompressor and had to
	// refuse it in validation; the Store interface makes that state
	// unrepresentable, so the "one seam alone" rows are gone with the bug
	// they guarded.
	withStore := valid
	withStore.Store = stubStore{}
	require.NoError(t, pushOptions(withStore).Validate())

	tests := map[string]func(*PushVolumeOptions){
		"no namespace": func(o *PushVolumeOptions) { o.Namespace = "" },
		"no volume":    func(o *PushVolumeOptions) { o.Volume = "" },
		"no source":    func(o *PushVolumeOptions) { o.SourceDir = "" },
		"no hasher":    func(o *PushVolumeOptions) { o.Hasher = nil },
		"reserved tag": func(o *PushVolumeOptions) { o.Tags = []string{"head"} },
	}
	for name, break_ := range tests {
		t.Run(name, func(t *testing.T) {
			opts := valid
			break_(&opts)
			require.Error(t, pushOptions(opts).Validate())
		})
	}
}

func TestDownloadOptionsValidate(t *testing.T) {
	valid := DownloadVolumeOptions{
		Ref: "models/gpt2", DestDir: "/tmp/out",
		Hasher: stubHasher, Store: stubStore{},
	}
	require.NoError(t, pullOptions(valid).Validate())

	for _, ref := range []string{"models/gpt2:prod", "models/gpt2@b3:abc123abc123", "bdn://models/gpt2"} {
		t.Run(ref, func(t *testing.T) {
			opts := valid
			opts.Ref = ref
			require.NoError(t, pullOptions(opts).Validate())
		})
	}

	tests := map[string]func(*DownloadVolumeOptions){
		"no ref":         func(o *DownloadVolumeOptions) { o.Ref = "" },
		"malformed ref":  func(o *DownloadVolumeOptions) { o.Ref = "gpt2" },
		"no destination": func(o *DownloadVolumeOptions) { o.DestDir = "" },
		"no hasher":      func(o *DownloadVolumeOptions) { o.Hasher = nil },
		"no store":       func(o *DownloadVolumeOptions) { o.Store = nil },
		// Restart discards a partly downloaded tree; in Overwrite mode that
		// tree is the caller's own directory, including the files Overwrite
		// promises to leave alone.
		"restart while overwriting": func(o *DownloadVolumeOptions) { o.Overwrite, o.Restart = true, true },
	}
	for name, break_ := range tests {
		t.Run(name, func(t *testing.T) {
			opts := valid
			break_(&opts)
			require.Error(t, pullOptions(opts).Validate())
		})
	}
}

// TestVolumeTokenExchange covers the hand-off from the API key to a capability
// token: the key authenticates one request, and everything after it goes to a
// different host with a token scoped to a namespace and a set of scopes.
func TestVolumeTokenExchange(t *testing.T) {
	var exchanges atomic.Int64
	var gotAuth, gotBody, gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = strings.TrimSpace(string(body))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "capability-token", "bdn_endpoint": "https://volumes.example",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"scopes":     []string{"PUSH", "TAG"}, "namespaces": []string{"models"},
		})
	}))
	defer server.Close()

	client, err := NewManagementClient(ManagementClientOptions{APIKey: "api-key", BaseURL: server.URL})
	require.NoError(t, err)

	tokens := client.volumeTokenSource("models", []string{"PUSH", "TAG"}, "corr-1").tokenSource()
	token, host, err := tokens(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, "capability-token", token)
	require.Equal(t, "https://volumes.example", host)
	require.Equal(t, "/v1/volumes/token", gotPath)
	require.Equal(t, "Bearer api-key", gotAuth)
	// Field-by-field rather than byte-for-byte: the request goes through the
	// generated client, whose struct declares the same three fields in a
	// different order. Key order carries no meaning to the service; the pin
	// is that every field arrives with its value.
	var sent struct {
		Scopes        []string `json:"scopes"`
		Namespaces    []string `json:"namespaces"`
		CorrelationID string   `json:"correlation_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(gotBody), &sent))
	require.Equal(t, "PUSH,TAG", strings.Join(sent.Scopes, ","))
	require.Equal(t, "models", strings.Join(sent.Namespaces, ","))
	require.Equal(t, "corr-1", sent.CorrelationID)

	// The token is held on to rather than exchanged per request.
	if _, _, err := tokens(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	require.Equal(t, int64(1), exchanges.Load())

	// A token the service rejected is replaced, which is how a transfer
	// outlives a credential it cannot renew.
	if _, _, err := tokens(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	require.Equal(t, int64(2), exchanges.Load())
}

func TestVolumeTokenExchangeFailures(t *testing.T) {
	tests := map[string]http.HandlerFunc{
		"rejected": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"detail":"no volume access"}`)
		},
		"not json": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "<html>hello</html>")
		},
		// The fakes below declare the JSON content type the real service always
		// sends; without it the generated client refuses the response before
		// the condition under test is ever reached.
		"no token": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"bdn_endpoint":"https://volumes.example"}`)
		},
		// A null endpoint is the service saying this deployment does not serve
		// volumes yet, which is a different thing from a malformed response
		// and must not be mistaken for one later.
		"null endpoint": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"capability-token","bdn_endpoint":null}`)
		},
		"unparseable expiry": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w,
				`{"token":"t","bdn_endpoint":"https://x","expires_at":"whenever"}`)
		},
		// An empty-string endpoint is not null: null is the deployment saying
		// it has no volume API, an empty string is a response this client
		// cannot use. The two shapes get different errors, tested here and in
		// the sentinel test below.
		"empty endpoint": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"capability-token","bdn_endpoint":""}`)
		},
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()

			client, err := NewManagementClient(ManagementClientOptions{APIKey: "api-key", BaseURL: server.URL})
			require.NoError(t, err)

			_, _, err = client.volumeTokenSource("models", []string{"PULL"}, "").tokenSource()(context.Background(), "")
			require.Error(t, err)
			require.Contains(t, err.Error(), "exchange volume token")
		})
	}
}

// TestVolumeOptionsReachTheEngine checks the translation into the internal
// options, which is where a dropped field would silently change behavior.
func TestVolumeOptionsReachTheEngine(t *testing.T) {
	concurrency := VolumeConcurrencyOptions{FileJobs: 3, ChunkOperations: 4, MaxBytesInFlight: 5}

	push := pushOptions(PushVolumeOptions{
		Namespace: "models", Volume: "gpt2", SourceDir: "/tmp/tree", SourceURI: "file:///fixed",
		Tags: []string{"prod"}, RequireHeadMove: true, Hasher: stubHasher,
		Store:       stubStore{},
		Concurrency: concurrency,
	})
	require.Equal(t, "models", push.Namespace)
	require.Equal(t, "gpt2", push.Volume)
	require.Equal(t, "/tmp/tree", push.SourceDir)
	require.Equal(t, "file:///fixed", push.SourceURI)
	require.Len(t, push.Tags, 1)
	require.True(t, push.RequireHeadMove, "RequireHeadMove was dropped")
	require.NotNil(t, push.Decompress)
	require.NotNil(t, push.DownloadObject)
	require.Equal(t, 3, push.Concurrency.FileJobs)

	pull := pullOptions(DownloadVolumeOptions{
		Ref: "models/gpt2:prod", DestDir: "/tmp/out", Overwrite: true, Restart: true,
		Include: []string{"weights"}, Hasher: stubHasher,
		Store:       stubStore{},
		Concurrency: concurrency,
	})
	require.Equal(t, "models/gpt2:prod", pull.Ref)
	require.Equal(t, "/tmp/out", pull.DestDir)
	require.True(t, pull.Overwrite, "Overwrite was dropped")
	require.True(t, pull.Restart, "Restart was dropped")
	require.Len(t, pull.Include, 1)
	require.Equal(t, int64(5), pull.Concurrency.MaxBytesInFlight)

	// Leaving concurrency unset must not zero the engine's defaults: the
	// zero value translates to the engine's zero value, whose meaning is
	// already "defaults".
	require.Equal(t, 0, pushOptions(PushVolumeOptions{}).Concurrency.FileJobs)
}

// TestVolumeTransfersValidateBeforeExchangingAToken checks that bad options
// fail without spending a request, so a mistake costs nothing server-side.
func TestVolumeTransfersValidateBeforeExchangingAToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid options should not have reached the API")
	}))
	defer server.Close()

	client, err := NewManagementClient(ManagementClientOptions{APIKey: "api-key", BaseURL: server.URL})
	require.NoError(t, err)

	_, err = client.PushVolume(context.Background(), PushVolumeOptions{Namespace: "models"})
	require.Error(t, err)

	_, err = client.DownloadVolume(context.Background(), DownloadVolumeOptions{Ref: "models/gpt2"})
	require.Error(t, err)
}

// TestTokenExchangeCollapsesASimultaneousExpiry covers what a token expiring
// mid-transfer actually looks like: every request in flight is rejected at
// once. Each rejection naming the token it was refused lets the source answer
// all but the first from what it already replaced, so an expiry costs one
// exchange rather than one per in-flight request.
func TestTokenExchangeCollapsesASimultaneousExpiry(t *testing.T) {
	var exchanges atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": fmt.Sprintf("token-%d", n), "bdn_endpoint": "https://volumes.example",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"scopes":     []string{"PULL"}, "namespaces": []string{"models"},
		})
	}))
	defer server.Close()

	client, err := NewManagementClient(ManagementClientOptions{APIKey: "api-key", BaseURL: server.URL})
	require.NoError(t, err)
	tokens := client.volumeTokenSource("models", []string{"PULL"}, "").tokenSource()

	first, _, err := tokens(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, int64(1), exchanges.Load())

	// Sixty-four requests holding the same now-expired token, all rejected
	// together, exactly as a saturated transfer would be.
	const inFlight = 64
	var wg sync.WaitGroup
	replaced := make([]string, inFlight)
	for i := range inFlight {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, _, err := tokens(context.Background(), first)
			require.NoError(t, err)
			replaced[i] = token
		}()
	}
	wg.Wait()

	require.Equal(t, int64(2), exchanges.Load())
	for i, token := range replaced {
		require.Equal(t, "token-2", token)
		require.NotEqual(t, first, replaced[i])
	}
}

// TestTokenRefreshesAheadOfExpiry covers the quiet path. Tokens cannot be
// renewed, so a transfer longer than one has to exchange another; doing it on
// the expiry the service reported means nothing is ever rejected, where
// waiting for a 401 means every request in flight fails at once first.
func TestTokenRefreshesAheadOfExpiry(t *testing.T) {
	var exchanges atomic.Int64
	var ttl time.Duration = time.Hour
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": fmt.Sprintf("token-%d", n), "bdn_endpoint": "https://volumes.example",
			"expires_at": time.Now().Add(ttl).UTC().Format(time.RFC3339),
			"scopes":     []string{"PULL"}, "namespaces": []string{"models"},
		})
	}))
	defer server.Close()

	client, err := NewManagementClient(ManagementClientOptions{APIKey: "api-key", BaseURL: server.URL})
	require.NoError(t, err)
	tokens := client.volumeTokenSource("models", []string{"PULL"}, "").tokenSource()

	// A token good for an hour is reused.
	first, _, err := tokens(context.Background(), "")
	require.NoError(t, err)
	_, _, err = tokens(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, int64(1), exchanges.Load())

	// One inside the refresh margin is replaced before it is ever refused, so
	// no request has to fail to discover it.
	ttl = 30 * time.Second
	second, _, err := tokens(context.Background(), first)
	require.NoError(t, err)
	require.Equal(t, int64(2), exchanges.Load())
	require.NotEqual(t, first, second)

	third, _, err := tokens(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, int64(3), exchanges.Load())
	require.NotEqual(t, second, third)
}

// TestPushScopeSelection pins what a push asks for. The exchange refuses a
// scope the caller cannot have rather than granting a smaller set, so every
// unnecessary scope is a way for the whole transfer to fail.
func TestPushScopeSelection(t *testing.T) {
	bare := PushVolumeOptions{Namespace: "models", Volume: "gpt2", SourceDir: "/tmp/t", Hasher: stubHasher}

	tests := []struct {
		name string
		with func(*PushVolumeOptions)
		want string
	}{
		// Moving head needs no scope beyond push.
		{"plain push", func(*PushVolumeOptions) {}, "PUSH"},
		// Applying a tag at commit is gated like setting one directly.
		{"with tags", func(o *PushVolumeOptions) { o.Tags = []string{"prod"} }, "PUSH,TAG"},
		// Reading the previous version needs pull authority — and this is the
		// condition that must not drift: without PULL the lookup is refused,
		// the push reads that as "no previous version", and delta reuse stops
		// with nothing to notice it by. The Store interface made the old
		// one-seam-alone rows unrepresentable.
		{"with the reuse store", func(o *PushVolumeOptions) {
			o.Store = stubStore{}
		}, "PUSH,PULL"},
		{"tags and reuse", func(o *PushVolumeOptions) {
			o.Tags = []string{"prod"}
			o.Store = stubStore{}
		}, "PUSH,TAG,PULL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := bare
			tc.with(&opts)
			require.Equal(t, tc.want, strings.Join(pushScopes(opts), ","))
		})
	}
}

// TestCorrelationIDIsAcceptable pins the identifier against what the field
// accepts: non-empty, printable ASCII, and no spaces. A value outside that is
// rejected, and rejection there fails the exchange rather than dropping the
// field.
func TestCorrelationIDIsAcceptable(t *testing.T) {
	for range 50 {
		id := newCorrelationID()
		require.True(t, id != "", "correlation id must not be empty")
		require.True(t, len(id) <= 128, "correlation id %q is longer than 128", id)
		for _, r := range id {
			require.True(t, r >= 0x21 && r <= 0x7e,
				"correlation id %q holds %q, outside printable ASCII without spaces", id, r)
		}
	}
}

// The two collapse tests below cover different paths to the same guarantee,
// and both are needed. When a replacement is healthy, everything after the
// first caller is served from the held token — ordinary caching. When it is
// not, the latch is what stops the rest re-exchanging. A single test with a
// healthy replacement passes even with the latch deleted, so it cannot stand
// for both.

// TestRefreshCollapsesWithAHealthyReplacement covers the caching path: one
// caller notices the expiry, exchanges, and every other in-flight caller is
// served the replacement rather than exchanging its own.
func TestRefreshCollapsesWithAHealthyReplacement(t *testing.T) {
	var exchanges atomic.Int64
	var ttl atomic.Int64
	ttl.Store(int64(time.Hour))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": fmt.Sprintf("token-%d", n), "bdn_endpoint": "https://volumes.example",
			"expires_at": time.Now().Add(time.Duration(ttl.Load())).UTC().Format(time.RFC3339),
			"scopes":     []string{"PULL"}, "namespaces": []string{"models"},
		})
	}))
	defer server.Close()

	client, err := NewManagementClient(ManagementClientOptions{APIKey: "api-key", BaseURL: server.URL})
	require.NoError(t, err)
	tokens := client.volumeTokenSource("models", []string{"PULL"}, "corr").tokenSource()

	// A token already inside the refresh margin, held by everything in flight.
	ttl.Store(int64(10 * time.Second))
	if _, _, err := tokens(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	require.Equal(t, int64(1), exchanges.Load())

	// The replacement is healthy, so it serves everyone.
	ttl.Store(int64(time.Hour))
	got := burst(t, tokens, 64)
	require.Equal(t, int64(2), exchanges.Load())
	requireOneToken(t, got)
}

// TestRefreshCollapsesWhenReplacementsStayShort covers the path the latch
// guards: every replacement is born inside the margin, so without the latch
// each in-flight caller would exchange its own. Against the exchange
// endpoint's per-key rate limit that is a failed transfer, not a slow one.
func TestRefreshCollapsesWhenReplacementsStayShort(t *testing.T) {
	var exchanges atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": fmt.Sprintf("token-%d", n), "bdn_endpoint": "https://volumes.example",
			// Held short throughout: no replacement is ever healthy.
			"expires_at": time.Now().Add(10 * time.Second).UTC().Format(time.RFC3339),
			"scopes":     []string{"PULL"}, "namespaces": []string{"models"},
		})
	}))
	defer server.Close()

	client, err := NewManagementClient(ManagementClientOptions{APIKey: "api-key", BaseURL: server.URL})
	require.NoError(t, err)
	tokens := client.volumeTokenSource("models", []string{"PULL"}, "corr").tokenSource()

	if _, _, err := tokens(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	require.Equal(t, int64(1), exchanges.Load())

	// The initial acquire, then one replacement that proves refreshing is not
	// helping. Everything else is served from what is held.
	got := burst(t, tokens, 64)
	require.Equal(t, int64(2), exchanges.Load())
	requireOneToken(t, got)
}

// burst runs n concurrent fetches and returns the tokens they received. The
// caller checks the exchange count first, because a count is what says how bad
// a regression is; the tokens then say the callers really were served the same
// credential rather than coincidentally counted right.
func burst(t *testing.T, tokens bdn.TokenSource, n int) []string {
	t.Helper()
	var wg sync.WaitGroup
	got := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, _, err := tokens(context.Background(), "")
			require.NoError(t, err)
			got[i] = token
		}()
	}
	wg.Wait()
	return got
}

// requireOneToken checks that every caller in a burst was served the same
// credential.
func requireOneToken(t *testing.T, got []string) {
	t.Helper()
	for i, token := range got {
		if token == "" {
			t.Fatalf("caller %d received no token", i)
		}
		require.Equal(t, got[0], token)
	}
}

// TestRefreshStopsWhenItBuysNothing covers a deployment whose tokens are
// shorter-lived than the refresh margin, or a clock far enough ahead to look
// that way. Every replacement is then instantly stale again, so refreshing on
// each attempt would exchange once per attempt — serialized, and against a
// per-key rate limit that would end the transfer and starve anything else
// using the same key.
//
// The right degraded mode is to stop refreshing quietly and let rejections
// drive it: one exchange per refusal rather than one per attempt.
func TestRefreshStopsWhenItBuysNothing(t *testing.T) {
	var exchanges atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": fmt.Sprintf("token-%d", n), "bdn_endpoint": "https://volumes.example",
			// Well inside the refresh margin, so every token looks due for
			// replacement the moment it arrives.
			"expires_at": time.Now().Add(10 * time.Second).UTC().Format(time.RFC3339),
			"scopes":     []string{"PULL"}, "namespaces": []string{"models"},
		})
	}))
	defer server.Close()

	client, err := NewManagementClient(ManagementClientOptions{APIKey: "api-key", BaseURL: server.URL})
	require.NoError(t, err)
	tokens := client.volumeTokenSource("models", []string{"PULL"}, "corr").tokenSource()

	// The first call exchanges; the second finds the replacement no better and
	// gives up on refreshing ahead of time. Everything after reuses it.
	for range 50 {
		if _, _, err := tokens(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
	}
	require.Equal(t, int64(2), exchanges.Load())

	// Rejections still work, which is the whole point of the degraded mode:
	// one exchange per refusal, not one per attempt.
	token, _, err := tokens(context.Background(), "")
	require.NoError(t, err)
	if _, _, err := tokens(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	require.Equal(t, int64(3), exchanges.Load())
}

// TestNoVolumeAPIIsDistinguishable covers an environment with no volume
// service. A caller wants to say that plainly rather than report what looks
// like an outage, so it is a sentinel rather than an opaque message.
func TestNoVolumeAPIIsDistinguishable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"t","bdn_endpoint":null,"scopes":["PULL"],"namespaces":["models"]}`)
	}))
	defer server.Close()

	client, err := NewManagementClient(ManagementClientOptions{APIKey: "api-key", BaseURL: server.URL})
	require.NoError(t, err)

	_, _, err = client.volumeTokenSource("models", []string{"PULL"}, "").tokenSource()(context.Background(), "")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoVolumeAPI), "expected ErrNoVolumeAPI, got %v", err)
}

// TestEmptyEndpointIsNotMissingAPI pins the other half of that distinction:
// an empty-string endpoint is a malformed response, not the deployment
// saying it has no volume API. Reporting the sentinel for it would send an
// operator hunting deployment configuration for a service bug.
func TestEmptyEndpointIsNotMissingAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"t","bdn_endpoint":"","scopes":["PULL"],"namespaces":["models"]}`)
	}))
	defer server.Close()

	client, err := NewManagementClient(ManagementClientOptions{APIKey: "api-key", BaseURL: server.URL})
	require.NoError(t, err)

	_, _, err = client.volumeTokenSource("models", []string{"PULL"}, "").tokenSource()(context.Background(), "")
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrNoVolumeAPI), "an empty endpoint must not read as a missing volume API: %v", err)
	require.True(t, errors.Is(err, ErrMalformedVolumeEndpoint), "the empty shape should carry its own sentinel: %v", err)
}
