package separatemoduletests_test

// The public surface, end to end: an API key exchanged for a capability token,
// a tree pushed through ManagementClient, and the same tree downloaded back.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/client"
)

// newManagementAPI stands in for the Baseten API: it answers the token
// exchange with a capability token and the host of the volume service.
func newManagementAPI(t *testing.T, fake *fakeService, exchanges *atomic.Int64) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/volumes/token" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer api-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// The real endpoint is namespace-scoped with an explicit scope list
		// and forbids unknown fields, so the fake reads the request rather
		// than ignoring it: a client sending the old volume-scoped shape
		// should fail here rather than pass and fail in a real deployment.
		var request struct {
			Scopes        []string `json:"scopes"`
			Namespaces    []string `json:"namespaces"`
			CorrelationID string   `json:"correlation_id"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil ||
			len(request.Scopes) == 0 || len(request.Namespaces) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Counted only once the request is accepted: a refused request is not
		// an exchange, and tests that count exchanges mean the ones that
		// minted a token.
		exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": fake.token(), "bdn_endpoint": fake.server.URL,
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			// Echoed as granted, which is what a caller reads to learn what it
			// actually got.
			"scopes": request.Scopes, "namespaces": request.Namespaces,
		})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestManagementClientRoundTrip(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)
	var exchanges atomic.Int64

	api, err := client.NewManagementClient(client.ManagementClientOptions{
		APIKey:  "api-key",
		BaseURL: newManagementAPI(t, fake, &exchanges),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var phases []client.VolumePhase
	pushed, err := api.PushVolume(ctx, client.PushVolumeOptions{
		Namespace: fakeNamespace,
		Volume:    fakeVolume,
		SourceDir: root,
		SourceURI: "file:///fixture",
		Tags:      []string{"prod"},
		NewHasher: newBlake3,
		Progress: func(p client.VolumeProgress) {
			if len(phases) == 0 || phases[len(phases)-1] != p.Phase {
				phases = append(phases, p.Phase)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pushed.Files != 5 || !pushed.HeadUpdated {
		t.Errorf("pushed %d files, head updated %v", pushed.Files, pushed.HeadUpdated)
	}
	if len(pushed.TagsApplied) != 1 || pushed.TagsApplied[0] != "prod" {
		t.Errorf("applied tags %v", pushed.TagsApplied)
	}
	if len(phases) < 3 {
		t.Errorf("progress reported phases %v, expected a scan, an upload, and a commit", phases)
	}

	dest := filepath.Join(t.TempDir(), "downloaded")
	downloaded, err := api.DownloadVolume(ctx, client.DownloadVolumeOptions{
		Ref:             fakeNamespace + "/" + fakeVolume + ":prod",
		DestDir:         dest,
		NewHasher:       newBlake3,
		NewDecompressor: newZstdReader,
		DownloadObject:  fake.downloader(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "assets"), 0o755) })

	if downloaded.ManifestDigest != pushed.ManifestDigest {
		t.Errorf("downloaded %s, pushed %s", downloaded.ManifestDigest, pushed.ManifestDigest)
	}
	if got, want := treeDescription(t, dest), treeDescription(t, root); got != want {
		t.Errorf("the round trip changed the tree\n got:\n%s\nwant:\n%s", got, want)
	}

	// The API key is exchanged once per transfer and the resulting token is
	// reused for every request that transfer makes.
	if exchanges.Load() != 2 {
		t.Errorf("exchanged %d tokens, want one per transfer", exchanges.Load())
	}
}

// TestManagementClientReExchangesARejectedToken covers a transfer outliving
// its capability token, which is the failure the per-request token source
// exists to survive.
func TestManagementClientReExchangesARejectedToken(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)
	var exchanges atomic.Int64

	// The volume service rejects the first token it is shown, as an expired
	// one would be.
	var rejected atomic.Bool
	guarded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rejected.CompareAndSwap(false, true) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"UNAUTHENTICATED","reason":"UNAUTHENTICATED",`+
				`"domain":"bdn.baseten.co","message":"token expired"}}`)
			return
		}
		proxyTo(w, r, fake.server.URL)
	}))
	t.Cleanup(guarded.Close)

	managementAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": fake.token(), "bdn_endpoint": guarded.URL,
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"scopes":     []string{"PUSH", "TAG"}, "namespaces": []string{fakeNamespace},
		})
	}))
	t.Cleanup(managementAPI.Close)

	api, err := client.NewManagementClient(client.ManagementClientOptions{
		APIKey: "api-key", BaseURL: managementAPI.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	pushed, err := api.PushVolume(context.Background(), client.PushVolumeOptions{
		Namespace: fakeNamespace, Volume: fakeVolume, SourceDir: root,
		SourceURI: "file:///fixture", NewHasher: newBlake3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pushed.Files != 5 {
		t.Errorf("pushed %d files, want 5", pushed.Files)
	}
	if exchanges.Load() != 2 {
		t.Errorf("exchanged %d tokens, want a second one after the rejection", exchanges.Load())
	}
}

// proxyTo forwards a request to another server, so a test can wrap the fake
// service without reimplementing it.
func proxyTo(w http.ResponseWriter, r *http.Request, target string) {
	forwarded, err := http.NewRequestWithContext(r.Context(), r.Method, target+r.URL.RequestURI(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	forwarded.Header = r.Header.Clone()
	forwarded.ContentLength = r.ContentLength

	resp, err := http.DefaultClient.Do(forwarded)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for name, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// TestTokenFakeRefusesUnknownFields is the test the strict decode cannot
// exist without: the fake's comment has claimed unknown-field refusal since
// it was written, and for a while the code did not do it — a claim nothing
// asserted. Post a request with a field the real endpoint would refuse and
// watch the fake refuse it too; with a plain decode this test goes red.
func TestTokenFakeRefusesUnknownFields(t *testing.T) {
	fake := newFakeService(t)
	var exchanges atomic.Int64
	api := newManagementAPI(t, fake, &exchanges)

	body := `{"scopes":["PUSH"],"namespaces":["ns"],"volume":"the-old-volume-scoped-shape"}`
	req, err := http.NewRequest(http.MethodPost, api+"/v1/volumes/token", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer api-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a request with an unknown field got %d, want %d — the fake is looser than the endpoint it stands in for",
			resp.StatusCode, http.StatusBadRequest)
	}
	if exchanges.Load() != 0 {
		t.Errorf("the refused request was counted as an exchange")
	}
}
