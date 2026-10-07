package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/internal/require"
)

func TestNewClient(t *testing.T) {
	provider := func(context.Context, TokenProviderOptions) (string, error) { return "token", nil }
	management, err := client.NewManagementClient(client.ManagementClientOptions{APIKey: "key"})
	require.NoError(t, err)

	t.Run("RequiresCredentials", func(t *testing.T) {
		_, err := NewClient(ClientOptions{})
		require.Error(t, err)
	})

	t.Run("CredentialsMutuallyExclusive", func(t *testing.T) {
		for _, opts := range []ClientOptions{
			{APIKey: "key", TokenProvider: provider},
			{APIKey: "key", ManagementClient: management},
			{ManagementClient: management, TokenProvider: provider},
		} {
			_, err := NewClient(opts)
			require.Error(t, err)
		}
	})

	t.Run("AcceptsEachCredential", func(t *testing.T) {
		for _, opts := range []ClientOptions{
			{APIKey: "key"},
			{ManagementClient: management},
			{TokenProvider: provider},
		} {
			_, err := NewClient(opts)
			require.NoError(t, err)
		}
	})

	t.Run("DefaultsAndTrimsBaseURL", func(t *testing.T) {
		c, err := NewClient(ClientOptions{APIKey: "key"})
		require.NoError(t, err)
		require.Equal(t, "https://api.baseten.co", c.API().BaseURL)
		c, err = NewClient(ClientOptions{APIKey: "key", BaseURL: "https://api.example.com/"})
		require.NoError(t, err)
		require.Equal(t, "https://api.example.com", c.API().BaseURL)
	})

	t.Run("APIKeyMintsThroughBaseURL", func(t *testing.T) {
		srv := newTokenServer(t)
		c, err := NewClient(ClientOptions{APIKey: "key", BaseURL: srv.URL})
		require.NoError(t, err)
		status, seen := echo(t, c.api.HTTPClient.(*authenticatedClient), srv.URL, nil)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "token-1", seen)
	})

	t.Run("ManagementClientMintsWithItsOwnAuth", func(t *testing.T) {
		srv := newTokenServer(t)
		var mintAuth string
		mintClient, err := client.NewManagementClient(client.ManagementClientOptions{
			BaseURL:   srv.URL,
			DeferAuth: true,
			HTTPClient: doerFunc(func(req *http.Request) (*http.Response, error) {
				req.Header.Set("Authorization", "Bearer oauth")
				mintAuth = req.Header.Get("Authorization")
				return http.DefaultClient.Do(req)
			}),
		})
		require.NoError(t, err)
		c, err := NewClient(ClientOptions{ManagementClient: mintClient, BaseURL: srv.URL})
		require.NoError(t, err)
		status, seen := echo(t, c.api.HTTPClient.(*authenticatedClient), srv.URL, nil)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "token-1", seen)
		require.Equal(t, "Bearer oauth", mintAuth)
	})
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

// recordedRequest is one request the fake control plane got.
type recordedRequest struct {
	method string
	path   string
	query  string
	body   string
}

// controlPlane is a fake control plane: it mints tokens, records every other
// request, and answers each with the next queued response for its method and
// path, or a 404.
type controlPlane struct {
	*httptest.Server
	mu        sync.Mutex
	requests  []recordedRequest
	responses map[string][]fakeResponse
}

type fakeResponse struct {
	status int
	body   string
}

func newControlPlane(t *testing.T) *controlPlane {
	t.Helper()
	cp := &controlPlane{responses: map[string][]fakeResponse{}}
	cp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/token" {
			json.NewEncoder(w).Encode(map[string]any{"token": "token", "expires_at": time.Now().Add(time.Hour)})
			return
		}
		body, _ := io.ReadAll(r.Body)
		cp.mu.Lock()
		defer cp.mu.Unlock()
		cp.requests = append(cp.requests, recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery, string(body)})
		key := r.Method + " " + r.URL.Path
		queued := cp.responses[key]
		if len(queued) == 0 {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"code":"NOT_FOUND","message":"not found"}`))
			return
		}
		cp.responses[key] = queued[1:]
		w.WriteHeader(queued[0].status)
		w.Write([]byte(queued[0].body))
	}))
	t.Cleanup(cp.Close)
	return cp
}

func (cp *controlPlane) respond(method, path string, status int, body string) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	key := method + " " + path
	cp.responses[key] = append(cp.responses[key], fakeResponse{status, body})
}

func (cp *controlPlane) client(t *testing.T, opts ClientOptions) *Client {
	t.Helper()
	opts.APIKey = "key"
	opts.BaseURL = cp.URL
	c, err := NewClient(opts)
	require.NoError(t, err)
	return c
}

func (cp *controlPlane) lastRequest(t *testing.T) recordedRequest {
	t.Helper()
	cp.mu.Lock()
	defer cp.mu.Unlock()
	require.True(t, len(cp.requests) > 0, "no request recorded")
	return cp.requests[len(cp.requests)-1]
}

const sandboxRecord = `{"name":"sb","url":"https://sb.example/","status":"DEPLOYING","created_at":"2026-10-01T00:00:00Z"}`

func TestCreate(t *testing.T) {
	t.Run("SendsOnlySetFields", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("POST", "/v1/sandboxes/instances", 201, sandboxRecord)
		result, err := cp.client(t, ClientOptions{TeamID: "team-1"}).Create(t.Context(), CreateOptions{
			Name:              "sb",
			CreateIfNotExists: true,
			Labels:            map[string]string{},
		})
		require.NoError(t, err)
		request := cp.lastRequest(t)
		require.Equal(t, "team_id=team-1", request.query)
		require.Equal(t, `{"create_if_not_exists":true,"labels":{},"name":"sb"}`, request.body)
		// The result is a usable Sandbox, with the URL's trailing slash
		// trimmed, and carries the record.
		require.Equal(t, "sb", result.Name())
		require.Equal(t, "https://sb.example", result.URL())
		require.Equal(t, StatusDeploying, result.Info.Status)
	})

	t.Run("EmptyOptionsSendEmptyBody", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("POST", "/v1/sandboxes/instances", 201, sandboxRecord)
		_, err := cp.client(t, ClientOptions{}).Create(t.Context(), CreateOptions{})
		require.NoError(t, err)
		request := cp.lastRequest(t)
		require.Equal(t, "", request.query)
		require.Equal(t, `{}`, request.body)
	})

	t.Run("NoURLNamesCreatedSandbox", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("POST", "/v1/sandboxes/instances", 201, `{"name":"sb","status":"DEPLOYING"}`)
		_, err := cp.client(t, ClientOptions{}).Create(t.Context(), CreateOptions{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "sandbox sb was created")
	})

	t.Run("APIError", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("POST", "/v1/sandboxes/instances", 409, `{"code":"CONFLICT","message":"exists"}`)
		_, err := cp.client(t, ClientOptions{}).Create(t.Context(), CreateOptions{Name: "sb"})
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Equal(t, "CONFLICT", apiErr.Code)
	})
}

func TestGetInfo(t *testing.T) {
	cp := newControlPlane(t)
	cp.respond("GET", "/v1/sandboxes/instances/sb", 200, sandboxRecord)
	info, err := cp.client(t, ClientOptions{}).GetInfo(t.Context(), GetInfoOptions{Name: "sb", ShowSecrets: true})
	require.NoError(t, err)
	require.Equal(t, "sb", info.Name)
	require.Equal(t, "show_secrets=true", cp.lastRequest(t).query)
}

func TestGet(t *testing.T) {
	t.Run("BuildsSandbox", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("GET", "/v1/sandboxes/instances/sb", 200, sandboxRecord)
		sb, err := cp.client(t, ClientOptions{}).Get(t.Context(), GetOptions{Name: "sb"})
		require.NoError(t, err)
		require.Equal(t, "https://sb.example", sb.URL())
		require.Equal(t, "", cp.lastRequest(t).query)
	})

	t.Run("NotFound", func(t *testing.T) {
		cp := newControlPlane(t)
		_, err := cp.client(t, ClientOptions{}).Get(t.Context(), GetOptions{Name: "missing"})
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Equal(t, 404, apiErr.Status)
	})
}

func TestList(t *testing.T) {
	t.Run("PagesLazily", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("GET", "/v1/sandboxes/instances", 200,
			`{"items":[{"name":"a","status":"DEPLOYED"},{"name":"b","status":"DEPLOYED"}],"pagination":{"has_more":true,"cursor":"c1"}}`)
		cp.respond("GET", "/v1/sandboxes/instances", 200,
			`{"items":[{"name":"c","status":"FAILED"}],"pagination":{"has_more":false}}`)
		var names []string
		for info, err := range cp.client(t, ClientOptions{}).List(t.Context(), ListOptions{
			Statuses: []Status{StatusDeployed, StatusFailed},
			PageSize: 2,
		}) {
			require.NoError(t, err)
			names = append(names, info.Name)
		}
		require.Len(t, names, 3)
		require.Equal(t, "c", names[2])
		require.Len(t, cp.requests, 2)
		require.Equal(t, "limit=2&status=DEPLOYED&status=FAILED", cp.requests[0].query)
		require.Equal(t, "cursor=c1&limit=2&status=DEPLOYED&status=FAILED", cp.requests[1].query)
	})

	t.Run("StopsFetchingOnBreak", func(t *testing.T) {
		cp := newControlPlane(t)
		cp.respond("GET", "/v1/sandboxes/instances", 200,
			`{"items":[{"name":"a","status":"DEPLOYED"}],"pagination":{"has_more":true,"cursor":"c1"}}`)
		for range cp.client(t, ClientOptions{}).List(t.Context(), ListOptions{}) {
			break
		}
		require.Len(t, cp.requests, 1)
	})

	t.Run("RepeatedCursorErrors", func(t *testing.T) {
		cp := newControlPlane(t)
		for range 2 {
			cp.respond("GET", "/v1/sandboxes/instances", 200, `{"items":[],"pagination":{"has_more":true,"cursor":"same"}}`)
		}
		var lastErr error
		for _, err := range cp.client(t, ClientOptions{}).List(t.Context(), ListOptions{}) {
			lastErr = err
		}
		require.Error(t, lastErr)
		require.Contains(t, lastErr.Error(), "cursor")
	})

	t.Run("ErrorEndsIteration", func(t *testing.T) {
		cp := newControlPlane(t)
		count := 0
		for info, err := range cp.client(t, ClientOptions{}).List(t.Context(), ListOptions{}) {
			count++
			require.True(t, info == nil, "no record with an error")
			require.ErrorAs[*APIError](t, err)
		}
		require.Equal(t, 1, count)
	})
}

func TestUpdate(t *testing.T) {
	cp := newControlPlane(t)
	cp.respond("PATCH", "/v1/sandboxes/instances/sb", 200, sandboxRecord)
	_, err := cp.client(t, ClientOptions{}).Update(t.Context(), UpdateOptions{
		Name:   "sb",
		Labels: map[string]string{},
		Envs:   map[string]EnvValue{"A": {Value: "1", NonSecret: true}},
	})
	require.NoError(t, err)
	require.Equal(t, `{"envs":[{"name":"A","secret":false,"value":"1"}],"labels":{}}`, cp.lastRequest(t).body)
}

func TestDelete(t *testing.T) {
	cp := newControlPlane(t)
	cp.respond("DELETE", "/v1/sandboxes/instances/sb", 202, `{"name":"sb","status":"DELETING"}`)
	info, err := cp.client(t, ClientOptions{}).Delete(t.Context(), DeleteOptions{Name: "sb"})
	require.NoError(t, err)
	require.Equal(t, StatusDeleting, info.Status)
}

func TestSandboxFromURL(t *testing.T) {
	cp := newControlPlane(t)
	c := cp.client(t, ClientOptions{})
	_, err := c.SandboxFromURL(SandboxFromURLOptions{})
	require.Error(t, err)
	sb, err := c.SandboxFromURL(SandboxFromURLOptions{URL: "https://sb.example/"})
	require.NoError(t, err)
	require.Equal(t, "", sb.Name())
	require.Equal(t, "https://sb.example", sb.URL())
	// No call was made.
	require.Len(t, cp.requests, 0)
}
