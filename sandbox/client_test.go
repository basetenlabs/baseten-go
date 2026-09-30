package sandbox_test

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
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/sandbox"
)

// controlPlaneForTest serves the token exchange and the sandbox control plane
// on one httptest server, recording every request's method, path,
// Authorization, and body.
type controlPlaneRecorder struct {
	mu       sync.Mutex
	requests []recordedRequest
	mints    []string
	revoked  map[string]bool
	handler  http.HandlerFunc
}

type recordedRequest struct {
	method        string
	path          string
	authorization string
	body          string
	teamIDQuery   string
}

func (r *controlPlaneRecorder) record(req *http.Request, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, recordedRequest{
		method:        req.Method,
		path:          req.URL.Path,
		authorization: req.Header.Get("Authorization"),
		body:          body,
		teamIDQuery:   req.URL.Query().Get("team_id"),
	})
}

func (r *controlPlaneRecorder) countByPath(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, request := range r.requests {
		if request.path == path {
			count++
		}
	}
	return count
}

// serve wires the default routing: token exchange plus a JSON sandbox record
// for every control-plane path.
func (r *controlPlaneRecorder) serve(t *testing.T, record string) *httptest.Server {
	t.Helper()
	if r.revoked == nil {
		r.revoked = map[string]bool{}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.record(req, string(body))
		switch req.URL.Path {
		case "/v1/token":
			r.mu.Lock()
			token := fmt.Sprintf("tok-%d", len(r.mints)+1)
			r.mints = append(r.mints, token)
			r.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"token": %q, "expires_at": %q}`, token, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		default:
			authorization := req.Header.Get("Authorization")
			r.mu.Lock()
			revoked := r.revoked[authorization]
			r.mu.Unlock()
			if revoked {
				w.Header().Set("x-blaxel-error-code", "TOKEN_REVOKED")
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"error": "TOKEN_REVOKED"}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			// Creation answers 201, deletion 202, the rest 200.
			switch req.Method {
			case "DELETE":
				w.WriteHeader(202)
			case "POST":
				w.WriteHeader(201)
			}
			_, _ = io.WriteString(w, record)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (r *controlPlaneRecorder) lastMint() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.mints) == 0 {
		return ""
	}
	return r.mints[len(r.mints)-1]
}

func (r *controlPlaneRecorder) revoke(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revoked[token] = true
}

const testSandboxRecord = `{
	"name": "sbx-1", "url": "https://sbx-1.invalid", "status": "DEPLOYED",
	"state": "RUNNING", "image": "baseten/base-image:latest", "memory": 4096,
	"region": "us-was-1", "enabled": true,
	"envs": [{"name": "A", "value": "1"}, {"name": "B", "value": "2", "secret": true}],
	"labels": {"k": "v"}, "display_name": "Disp", "external_id": "ext-1",
	"created_at": "2026-09-30T10:00:00Z", "updated_at": "2026-09-30T11:00:00Z",
	"created_by": "u1", "updated_by": "u2", "last_used_at": "2026-09-30T12:00:00Z",
	"expires_in": 3600
}`

func clientForTest(t *testing.T, serverURL string, mutate func(*sandbox.SandboxesClientOptions)) *sandbox.SandboxesClient {
	t.Helper()
	opts := sandbox.SandboxesClientOptions{
		APIKey:            "test-key",
		ManagementBaseURL: serverURL,
		TeamID:            "team-1",
	}
	if mutate != nil {
		mutate(&opts)
	}
	client, err := sandbox.NewSandboxesClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestTokenMintedOnceAndSent(t *testing.T) {
	recorder := &controlPlaneRecorder{}
	server := recorder.serve(t, testSandboxRecord)
	client := clientForTest(t, server.URL, nil)

	info, err := client.GetInfo(context.Background(), "sbx-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "sbx-1" || info.Status != "DEPLOYED" || info.Memory != 4096 {
		t.Errorf("record not converted: %+v", info)
	}
	if len(info.Envs) != 2 || info.Envs["B"] != (sandbox.SandboxEnvValue{Value: "2", Secret: true}) {
		t.Errorf("envs not converted: %+v", info.Envs)
	}
	if info.ExpiresInSeconds != 3600 {
		t.Errorf("expires_in not converted: %d", info.ExpiresInSeconds)
	}
	if recorder.countByPath("/v1/token") != 1 {
		t.Errorf("expected one mint, got %d", recorder.countByPath("/v1/token"))
	}
	// A second call reuses the cached token.
	if _, err := client.GetInfo(context.Background(), "sbx-1"); err != nil {
		t.Fatal(err)
	}
	if recorder.countByPath("/v1/token") != 1 {
		t.Errorf("cached token not reused: %d mints", recorder.countByPath("/v1/token"))
	}
	last := recorder.requests[len(recorder.requests)-1]
	if last.authorization != "Bearer tok-1" || last.teamIDQuery != "team-1" {
		t.Errorf("authorization %q, team_id %q", last.authorization, last.teamIDQuery)
	}
}

func TestTokenRevocationRemintsAndResends(t *testing.T) {
	recorder := &controlPlaneRecorder{}
	server := recorder.serve(t, testSandboxRecord)
	recorder.revoke("Bearer tok-1")
	client := clientForTest(t, server.URL, nil)

	info, err := client.GetInfo(context.Background(), "sbx-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "sbx-1" {
		t.Errorf("record not returned: %+v", info)
	}
	if len(recorder.mints) != 2 {
		t.Errorf("expected a re-mint, got %d mints", len(recorder.mints))
	}
	last := recorder.requests[len(recorder.requests)-1]
	if last.authorization != "Bearer "+recorder.lastMint() {
		t.Errorf("re-send not authed with the new token: %q", last.authorization)
	}
}

func TestTokenRevocationExhaustionReturnsLastError(t *testing.T) {
	recorder := &controlPlaneRecorder{}
	server := recorder.serve(t, testSandboxRecord)
	client := clientForTest(t, server.URL, nil)
	// Every token the server will mint is revoked.
	recorder.revoke("Bearer tok-1")
	recorder.revoke("Bearer tok-2")
	recorder.revoke("Bearer tok-3")

	_, err := client.GetInfo(context.Background(), "sbx-1")
	var apiError *sandbox.SandboxAPIError
	if !errors.As(err, &apiError) {
		t.Fatalf("want SandboxAPIError, got %v", err)
	}
	// The revocation body has no "message", so per the error contract the
	// code stays empty and the description carries TOKEN_REVOKED.
	if apiError.Status != 401 || apiError.Code != "" {
		t.Errorf("status %d code %q", apiError.Status, apiError.Code)
	}
	if !strings.Contains(apiError.Error(), "TOKEN_REVOKED") {
		t.Errorf("message %q", apiError.Error())
	}
	if len(recorder.mints) != 3 {
		t.Errorf("expected 3 mints (initial + 2 retries), got %d", len(recorder.mints))
	}
}

func TestAPIKeyWithTokenProviderRejected(t *testing.T) {
	_, err := sandbox.NewSandboxesClient(sandbox.SandboxesClientOptions{
		APIKey:        "key",
		TokenProvider: func(context.Context) (string, error) { return "token", nil },
	})
	if err == nil {
		t.Fatal("expected an error for APIKey with TokenProvider")
	}
}

func TestTokenProviderSentWithoutMint(t *testing.T) {
	recorder := &controlPlaneRecorder{}
	server := recorder.serve(t, testSandboxRecord)
	client := clientForTest(t, server.URL, func(opts *sandbox.SandboxesClientOptions) {
		opts.APIKey = ""
		opts.TokenProvider = func(context.Context) (string, error) { return "provided-token", nil }
	})

	if _, err := client.GetInfo(context.Background(), "sbx-1"); err != nil {
		t.Fatal(err)
	}
	if recorder.countByPath("/v1/token") != 0 {
		t.Errorf("provider path must not mint, got %d mints", recorder.countByPath("/v1/token"))
	}
	last := recorder.requests[len(recorder.requests)-1]
	if last.authorization != "Bearer provided-token" {
		t.Errorf("authorization %q", last.authorization)
	}
}

func TestCreateSendsOnlySetFields(t *testing.T) {
	recorder := &controlPlaneRecorder{}
	server := recorder.serve(t, testSandboxRecord)
	client := clientForTest(t, server.URL, nil)

	created, err := client.Create(context.Background(), &sandbox.CreateSandboxRequest{
		Name:   "sbx-1",
		Region: "us-was-1",
		Envs:   map[string]sandbox.SandboxEnvValue{"A": {Value: "1"}},
		Labels: map[string]string{"k": "v"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Info().Name != "sbx-1" || created.URL() != "https://sbx-1.invalid" {
		t.Errorf("sandbox not built from the record: %+v", created.Info())
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(recorder.requests[len(recorder.requests)-1].body), &body); err != nil {
		t.Fatal(err)
	}
	for _, unset := range []string{"image", "memory", "display_name", "external_id"} {
		if _, present := body[unset]; present {
			t.Errorf("unset field %q sent: %v", unset, body)
		}
	}
	if body["name"] != "sbx-1" || body["region"] != "us-was-1" {
		t.Errorf("set fields missing: %v", body)
	}
	envs, _ := body["envs"].([]any)
	if len(envs) != 1 || envs[0].(map[string]any)["value"] != "1" {
		t.Errorf("envs not converted: %v", body["envs"])
	}
	if labels, _ := body["labels"].(map[string]any); labels["k"] != "v" {
		t.Errorf("labels not converted: %v", body["labels"])
	}
}

func TestCreateEmptyRequestSendsEmptyBody(t *testing.T) {
	recorder := &controlPlaneRecorder{}
	server := recorder.serve(t, testSandboxRecord)
	client := clientForTest(t, server.URL, nil)

	if _, err := client.Create(context.Background(), &sandbox.CreateSandboxRequest{}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(recorder.requests[len(recorder.requests)-1].body) != "{}" {
		t.Errorf("empty create must send an empty object, got %q",
			recorder.requests[len(recorder.requests)-1].body)
	}
}

func TestUpdateSendsOnlySetFields(t *testing.T) {
	recorder := &controlPlaneRecorder{}
	server := recorder.serve(t, testSandboxRecord)
	client := clientForTest(t, server.URL, nil)

	updated, err := client.Update(context.Background(), "sbx-1", &sandbox.UpdateSandboxRequest{
		DisplayName: "New Name",
		Labels:      map[string]string{"only": "one"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.DisplayName != "Disp" {
		// The server replies with the fixture, which still says Disp.
		t.Errorf("update result not converted: %+v", updated)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(recorder.requests[len(recorder.requests)-1].body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 {
		t.Errorf("partial update sent more than its two fields: %v", body)
	}
	if body["display_name"] != "New Name" {
		t.Errorf("display_name %v", body["display_name"])
	}
	if labels, _ := body["labels"].(map[string]any); labels["only"] != "one" {
		t.Errorf("labels replace semantics: %v", body["labels"])
	}
	if recorder.requests[len(recorder.requests)-1].method != "PATCH" {
		t.Errorf("update must PATCH, got %s", recorder.requests[len(recorder.requests)-1].method)
	}
}

func TestUpdateEnabledFalseDisables(t *testing.T) {
	recorder := &controlPlaneRecorder{}
	server := recorder.serve(t, testSandboxRecord)
	client := clientForTest(t, server.URL, nil)

	disabled := false
	if _, err := client.Update(context.Background(), "sbx-1", &sandbox.UpdateSandboxRequest{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(recorder.requests[len(recorder.requests)-1].body), &body); err != nil {
		t.Fatal(err)
	}
	if body["enabled"] != false {
		t.Errorf("enabled must serialize as false, got %v", body["enabled"])
	}
}

func TestDeleteReturnsRecord(t *testing.T) {
	recorder := &controlPlaneRecorder{}
	server := recorder.serve(t, testSandboxRecord)
	client := clientForTest(t, server.URL, nil)

	info, err := client.Delete(context.Background(), "sbx-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "sbx-1" {
		t.Errorf("delete result not converted: %+v", info)
	}
	last := recorder.requests[len(recorder.requests)-1]
	if last.method != "DELETE" || last.path != "/v1/sandboxes/instances/sbx-1" {
		t.Errorf("delete request %s %s", last.method, last.path)
	}
}

func TestListPaginatesUntilExhausted(t *testing.T) {
	page := func(items string, hasMore bool, cursor string) string {
		cursorField := ""
		if cursor != "" {
			cursorField = fmt.Sprintf(`, "cursor": %q`, cursor)
		}
		return fmt.Sprintf(`{"items": [%s], "pagination": {"has_more": %t%s}}`, items, hasMore, cursorField)
	}
	pages := map[string]string{
		"":   page(`{"name": "sbx-1"}`, true, "c2"),
		"c2": page(`{"name": "sbx-2"}`, true, "c3"),
		"c3": page(`{"name": "sbx-3"}`, false, ""),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		page, present := pages[r.URL.Query().Get("cursor")]
		if !present {
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
			return
		}
		_, _ = io.WriteString(w, page)
	}))
	t.Cleanup(server.Close)
	client := clientForTest(t, server.URL, nil)

	var names []string
	for info, err := range client.List(context.Background(), &sandbox.ListSandboxesRequest{
		Query:    "sbx",
		Statuses: []sandbox.SandboxStatus{"DEPLOYED"},
		PageSize: 1,
	}) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, info.Name)
	}
	if len(names) != 3 || names[0] != "sbx-1" || names[2] != "sbx-3" {
		t.Errorf("pagination incomplete: %v", names)
	}
}

func TestListRepeatedCursorStops(t *testing.T) {
	body := `{"items": [{"name": "sbx-1"}], "pagination": {"has_more": true, "cursor": "same"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	client := clientForTest(t, server.URL, nil)

	var count int
	var listErr error
	for info, err := range client.List(context.Background(), &sandbox.ListSandboxesRequest{}) {
		if info != nil {
			count++
		}
		if err != nil {
			listErr = err
		}
	}
	if count != 2 {
		t.Errorf("the repeated cursor must surface after the second page, got %d infos", count)
	}
	if listErr == nil || !strings.Contains(listErr.Error(), "repeated cursor") {
		t.Errorf("want a repeated-cursor error, got %v", listErr)
	}
}

func TestSandboxFromInfoWithoutURLErrors(t *testing.T) {
	recorder := &controlPlaneRecorder{}
	server := recorder.serve(t, testSandboxRecord)
	client := clientForTest(t, server.URL, nil)

	_, err := client.SandboxFromInfo(sandbox.SandboxInfo{Name: "sbx-1"})
	if err == nil || !strings.Contains(err.Error(), "no URL") {
		t.Fatalf("want a no-URL error, got %v", err)
	}
}

func TestControlPlaneErrorCarriesCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"message": "sandbox not found", "error": "NOT_FOUND", "details": {"name": "sbx-1"}}`)
	}))
	t.Cleanup(server.Close)
	client := clientForTest(t, server.URL, nil)

	_, err := client.GetInfo(context.Background(), "sbx-1")
	var apiError *sandbox.SandboxAPIError
	if !errors.As(err, &apiError) {
		t.Fatalf("want SandboxAPIError, got %v", err)
	}
	if apiError.Status != 404 || apiError.Code != "NOT_FOUND" {
		t.Errorf("status %d code %q", apiError.Status, apiError.Code)
	}
	if !strings.Contains(apiError.Error(), "sandbox not found") {
		t.Errorf("message %q", apiError.Error())
	}
}

func TestControlPlaneGatewayStatusStaysPlainError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	t.Cleanup(server.Close)
	client := clientForTest(t, server.URL, nil)

	_, err := client.GetInfo(context.Background(), "sbx-1")
	var gatewayError *sandbox.SandboxGatewayError
	if errors.As(err, &gatewayError) {
		t.Error("the control plane does not sit behind the edge, so 502 must stay a plain error")
	}
	var apiError *sandbox.SandboxAPIError
	if !errors.As(err, &apiError) || apiError.Status != 502 {
		t.Fatalf("want a plain SandboxAPIError, got %v", err)
	}
}

func TestCreateSucceedsWithoutURL(t *testing.T) {
	// The record as of creation is usually DEPLOYING and has no execution
	// URL yet, so Create must not require one; exec on the result fails
	// loudly instead.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/token":
			fmt.Fprintf(w, `{"token": "tok-1", "expires_at": %q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.Method == "POST":
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"name": "sbx-1", "status": "DEPLOYING"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := clientForTest(t, server.URL, nil)

	created, err := client.Create(context.Background(), &sandbox.CreateSandboxRequest{Name: "sbx-1"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Info().Status != "DEPLOYING" || created.URL() != "" {
		t.Errorf("record not carried: %+v", created.Info())
	}
	_, err = created.Process().Exec(context.Background(), &sandbox.ExecOptions{Command: "true"})
	if err == nil || !strings.Contains(err.Error(), "no URL yet") {
		t.Fatalf("want a no-URL error from exec, got %v", err)
	}
}
