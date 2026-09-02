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
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/internal/volume"
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
		Hasher:    newBlake3,
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
	// Every remaining result field, so a copy dropped from the result literal
	// in PushVolume goes red here rather than shipping a permanent zero. The
	// fixture makes the counters discriminating: dup.txt repeats small.txt's
	// bytes and this push runs without a reuse store, so the duplicate is
	// sent and the service reports it already stored — Existing is exactly
	// one, measured stable across repeated runs. Reused stays zero on this
	// path, so its copy — like HeadMoveDenied's false — is one this test
	// cannot discriminate.
	if !strings.HasPrefix(pushed.ManifestDigest, "b3:") {
		t.Errorf("manifest digest %q does not name a version", pushed.ManifestDigest)
	}
	if pushed.Sequence != 1 {
		t.Errorf("sequence %d, want 1 for the first commit", pushed.Sequence)
	}
	if pushed.HeadMoveDenied {
		t.Error("head move reported denied with a credential that can move it")
	}
	if pushed.Bytes <= 0 {
		t.Errorf("pushed %d bytes, want the tree's bytes", pushed.Bytes)
	}
	if pushed.Chunks <= 0 || pushed.Unique <= 0 {
		t.Errorf("chunk accounting empty: %d chunks, %d unique", pushed.Chunks, pushed.Unique)
	}
	if pushed.Existing != 1 {
		t.Errorf("existing %d, want the one duplicated chunk", pushed.Existing)
	}
	if pushed.Unique+pushed.Reused+pushed.Existing != pushed.Chunks {
		t.Errorf("chunk partition %d+%d+%d does not cover %d",
			pushed.Unique, pushed.Reused, pushed.Existing, pushed.Chunks)
	}
	if len(phases) < 3 {
		t.Errorf("progress reported phases %v, expected a scan, an upload, and a commit", phases)
	}

	dest := filepath.Join(t.TempDir(), "downloaded")
	downloaded, err := api.DownloadVolume(ctx, client.DownloadVolumeOptions{
		Ref:     fakeNamespace + "/" + fakeVolume + ":prod",
		DestDir: dest,
		Hasher:  newBlake3,
		Store:   fakePublicStore{download: fake.downloader()},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "assets"), 0o755) })

	if downloaded.ManifestDigest != pushed.ManifestDigest {
		t.Errorf("downloaded %s, pushed %s", downloaded.ManifestDigest, pushed.ManifestDigest)
	}
	// Every remaining download-result field, for the same reason as the push
	// asserts above. ChunksReused is genuinely zero on a fresh download, so
	// its copy is the one field this test cannot discriminate.
	if !strings.Contains(downloaded.VersionRef, "@b3:") {
		t.Errorf("version ref %q is not pinned to a version", downloaded.VersionRef)
	}
	if downloaded.Files != 5 || downloaded.Bytes != pushed.Bytes {
		t.Errorf("downloaded %d files and %d bytes, pushed 5 and %d", downloaded.Files, downloaded.Bytes, pushed.Bytes)
	}
	if downloaded.TotalFiles != 5 || downloaded.SelectedFiles != downloaded.TotalFiles {
		t.Errorf("selection %d of %d, want the whole volume's 5", downloaded.SelectedFiles, downloaded.TotalFiles)
	}
	if downloaded.ChunksFetched <= 0 {
		t.Errorf("fetched %d chunks, want at least one", downloaded.ChunksFetched)
	}
	if downloaded.ChunksReused != 0 {
		t.Errorf("reused %d chunks on a fresh download", downloaded.ChunksReused)
	}
	if len(downloaded.Warnings) != 0 {
		t.Errorf("unexpected warnings %v", downloaded.Warnings)
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
		SourceURI: "file:///fixture", Hasher: newBlake3,
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

// fakePublicStore adapts an internal downloader to the public seam,
// translating each request and result field by field the same way the client
// boundary does in the other direction. The round-trip tests build it over
// the fake service's downloader; the completeness test builds it over a
// recorder.
type fakePublicStore struct {
	download volume.ObjectDownloader
}

func (s fakePublicStore) DownloadObject(ctx context.Context, req client.VolumeObjectDownload) (*client.VolumeObjectResult, error) {
	res, err := s.download(ctx, volume.ObjectDownload{
		Endpoint: req.Endpoint,
		Region:   req.Region,
		Bucket:   req.Bucket,
		Key:      req.Key,
		Credentials: volume.Credentials{
			AccessKeyID:     req.Credentials.AccessKeyID,
			SecretAccessKey: req.Credentials.SecretAccessKey,
			SessionToken:    req.Credentials.SessionToken,
		},
		ExpectedSize: req.ExpectedSize,
	})
	if err != nil || res == nil {
		return nil, err
	}
	return &client.VolumeObjectResult{Body: res.Body, ContentType: res.ContentType, Size: res.Size}, nil
}

func (s fakePublicStore) Decompressor(r io.Reader) (io.ReadCloser, error) {
	return newZstdReader(r)
}

// TestFakePublicStoreCopiesEveryField pins the fake adapter's hand-copy in
// both directions: every field of the public request must arrive on the
// internal side, and every field of the internal result must arrive on the
// public side. Each struct is first checked to hold no zero field, so a
// field added to either side fails here until it is populated and copied —
// without that check a new field would ride through as an unnoticed zero
// while real transfers carry data in it.
func TestFakePublicStoreCopiesEveryField(t *testing.T) {
	var got volume.ObjectDownload
	store := fakePublicStore{download: func(_ context.Context, req volume.ObjectDownload) (*volume.ObjectResult, error) {
		got = req
		return &volume.ObjectResult{
			Body:        io.NopCloser(strings.NewReader("body")),
			ContentType: "application/zstd",
			Size:        7,
		}, nil
	}}

	sent := client.VolumeObjectDownload{
		Endpoint: "https://storage.example",
		Region:   "us-east-1",
		Bucket:   "volumes",
		Key:      "objects/b3/abc",
		Credentials: client.VolumeObjectCredentials{
			AccessKeyID:     "access-key",
			SecretAccessKey: "secret-key",
			SessionToken:    "session-token",
		},
		ExpectedSize: 42,
	}
	requireNoZeroField(t, reflect.ValueOf(sent), "public request")

	res, err := store.DownloadObject(context.Background(), sent)
	if err != nil {
		t.Fatal(err)
	}

	requireNoZeroField(t, reflect.ValueOf(got), "internal request")
	want := volume.ObjectDownload{
		Endpoint: sent.Endpoint, Region: sent.Region, Bucket: sent.Bucket, Key: sent.Key,
		Credentials: volume.Credentials{
			AccessKeyID:     sent.Credentials.AccessKeyID,
			SecretAccessKey: sent.Credentials.SecretAccessKey,
			SessionToken:    sent.Credentials.SessionToken,
		},
		ExpectedSize: sent.ExpectedSize,
	}
	if got != want {
		t.Errorf("the fake translated the request to %+v, want %+v", got, want)
	}

	requireNoZeroField(t, reflect.ValueOf(*res), "public result")
	if res.ContentType != "application/zstd" || res.Size != 7 {
		t.Errorf("result fields did not survive the copy: %+v", res)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil || string(body) != "body" {
		t.Errorf("the body did not survive the copy: %q, %v", body, err)
	}
}

// requireNoZeroField fails for any field of a struct, recursing into struct
// fields, that is left at its zero value.
func requireNoZeroField(t *testing.T, v reflect.Value, what string) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		name := v.Type().Field(i).Name
		if field.Kind() == reflect.Struct {
			requireNoZeroField(t, field, what+"."+name)
			continue
		}
		if field.IsZero() {
			t.Errorf("%s.%s is zero — populate it in this test and make sure the fake copies it", what, name)
		}
	}
}

// TestMixedCaseNamesAreLoweredForBothConsumers pins the single fold: the
// options translation lowercases Namespace and Volume once, and both
// consumers of the name — the token exchange that scopes the capability and
// the wire requests the transfer makes — read that translated value. A token
// scoped to "MoDeLs" would not authorize a transfer addressing "models", so
// the two seeing different foldings is not cosmetic.
func TestMixedCaseNamesAreLoweredForBothConsumers(t *testing.T) {
	root := buildTree(t)
	fake := newFakeService(t)

	var mu sync.Mutex
	var wirePaths []string
	recorded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		wirePaths = append(wirePaths, r.URL.Path)
		mu.Unlock()
		proxyTo(w, r, fake.server.URL)
	}))
	t.Cleanup(recorded.Close)

	var scoped [][]string
	managementAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Scopes     []string `json:"scopes"`
			Namespaces []string `json:"namespaces"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		mu.Lock()
		scoped = append(scoped, request.Namespaces)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": fake.token(), "bdn_endpoint": recorded.URL,
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"scopes":     request.Scopes, "namespaces": request.Namespaces,
		})
	}))
	t.Cleanup(managementAPI.Close)

	api, err := client.NewManagementClient(client.ManagementClientOptions{
		APIKey: "api-key", BaseURL: managementAPI.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := api.PushVolume(context.Background(), client.PushVolumeOptions{
		Namespace: "MoDeLs", Volume: "GPT2", SourceDir: root,
		SourceURI: "file:///fixture", Hasher: newBlake3,
	}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Consumer one: the capability token was scoped to the lowered namespace.
	if len(scoped) == 0 {
		t.Fatal("no token exchange happened — the assertion never ran")
	}
	for _, namespaces := range scoped {
		for _, ns := range namespaces {
			if ns != fakeNamespace {
				t.Errorf("the exchange was scoped to namespace %q, want %q", ns, fakeNamespace)
			}
		}
	}

	// Consumer two: every wire request addressed the lowered names, and at
	// least one carried them, so the assertion is not vacuously green.
	if len(wirePaths) == 0 {
		t.Fatal("no wire requests recorded — the assertion never ran")
	}
	sawNames := false
	for _, path := range wirePaths {
		if strings.Contains(path, "MoDeLs") || strings.Contains(path, "GPT2") {
			t.Errorf("a wire request addressed the mixed-case name: %s", path)
		}
		if strings.Contains(path, "/"+fakeNamespace+"/"+fakeVolume+"/") {
			sawNames = true
		}
	}
	if !sawNames {
		t.Errorf("no wire request addressed %s/%s: %v", fakeNamespace, fakeVolume, wirePaths)
	}
}

// recordingPublicStore wraps a public store and records every request handed
// to it, so a test can assert on what the production translation delivered.
type recordingPublicStore struct {
	inner client.VolumeObjectStore
	mu    *sync.Mutex
	got   *[]client.VolumeObjectDownload
}

func (s recordingPublicStore) DownloadObject(ctx context.Context, req client.VolumeObjectDownload) (*client.VolumeObjectResult, error) {
	s.mu.Lock()
	*s.got = append(*s.got, req)
	s.mu.Unlock()
	return s.inner.DownloadObject(ctx, req)
}

func (s recordingPublicStore) Decompressor(r io.Reader) (io.ReadCloser, error) {
	return s.inner.Decompressor(r)
}

// TestDownloadDeliversResolvedOriginToTheStore pins the PRODUCTION
// engine-to-public translation — the store adapter inside DownloadVolume —
// which TestFakePublicStoreCopiesEveryField cannot: that test pins the test
// adapter's own copy, in the opposite direction. The fake service leases
// known credential values at resolve, and every request the caller's store
// receives must carry all of them, along with the origin's endpoint, region,
// and bucket, a key, and — for chunk reads — the size the manifest records.
// A credential field dropped by the production copy arrives here as an empty
// string against a known value, not as an unnoticed zero.
func TestDownloadDeliversResolvedOriginToTheStore(t *testing.T) {
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

	if _, err := api.PushVolume(ctx, client.PushVolumeOptions{
		Namespace: fakeNamespace, Volume: fakeVolume, SourceDir: root,
		SourceURI: "file:///fixture", Hasher: newBlake3,
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []client.VolumeObjectDownload
	dest := filepath.Join(t.TempDir(), "out")
	if _, err := api.DownloadVolume(ctx, client.DownloadVolumeOptions{
		Ref:     fakeNamespace + "/" + fakeVolume,
		DestDir: dest,
		Hasher:  newBlake3,
		Store: recordingPublicStore{
			inner: fakePublicStore{download: fake.downloader()},
			mu:    &mu,
			got:   &got,
		},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "assets"), 0o755) })

	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("the store saw no requests — nothing was asserted")
	}
	wantCredentials := client.VolumeObjectCredentials{
		AccessKeyID:     "key",
		SecretAccessKey: "secret",
		SessionToken:    "session",
	}
	sized := 0
	for _, req := range got {
		if req.Credentials != wantCredentials {
			t.Errorf("request for %q carried credentials %+v, want %+v", req.Key, req.Credentials, wantCredentials)
		}
		if req.Endpoint != fake.server.URL {
			t.Errorf("request for %q addressed endpoint %q, want %q", req.Key, req.Endpoint, fake.server.URL)
		}
		if req.Region != "us-east-1" {
			t.Errorf("request for %q carried region %q, want us-east-1", req.Key, req.Region)
		}
		if req.Bucket != fakeBucket {
			t.Errorf("request for %q carried bucket %q, want %q", req.Key, req.Bucket, fakeBucket)
		}
		if req.Key == "" {
			t.Error("a request carried no key")
		}
		if req.ExpectedSize > 0 {
			sized++
		}
	}
	if sized == 0 {
		t.Error("no request carried an expected size — the manifest-recorded chunk lengths were dropped")
	}
}
