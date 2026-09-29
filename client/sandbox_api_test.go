package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/client/sandboxapi"
)

func sandboxAPIForTest(t *testing.T, handler http.HandlerFunc) *sandboxapi.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cl, err := client.NewSandboxClient(client.SandboxClientOptions{Token: "test-token", BaseURL: srv.URL, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return cl.API()
}

func TestSandboxMultipartWire(t *testing.T) {
	payload := []byte{0, 1, 255, '\n'}
	api := sandboxAPIForTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/filesystem-multipart/upload/part" || r.URL.Query().Get("partNumber") != "2" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		part, err := mr.NextPart()
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		got, err := io.ReadAll(part)
		if err != nil || !bytes.Equal(got, payload) || part.FileName() != "part.bin" {
			t.Errorf("multipart payload mismatch: %q, %v", got, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	// The operation encoding overrides an inherited JSON content type.
	api.Headers.Set("Content-Type", "application/json")
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "part.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(payload)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = api.PutFilesystemMultipartPart(t.Context(), "upload", sandboxapi.PutFilesystemMultipartUploadIdPartParams{PartNumber: 2}, &body, writer.FormDataContentType())
	if err != nil {
		t.Fatal(err)
	}
}

func TestSandboxBinaryStdinWire(t *testing.T) {
	payload := []byte{0, 255, 128, 10}
	api := sandboxAPIForTest(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(payload, got) || r.Header.Get("Content-Type") != "application/octet-stream" {
			t.Error("binary body was transformed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	api.Headers.Set("Content-Type", "application/json")
	if _, err := api.PostProcessStdin(t.Context(), "process", bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxArchiveResponseStatuses(t *testing.T) {
	for _, code := range []int{200, 202} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			api := sandboxAPIForTest(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{}`)
			})
			got, err := api.PostArchiveExport(t.Context(), sandboxapi.ExportOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got.StatusCode != code || (got.JSON200 != nil) != (code == 200) || (got.JSON202 != nil) != (code == 202) {
				t.Fatalf("wrong response variant: %+v", got)
			}
		})
	}
}

func TestSandboxStreamArrivesBeforeServerCompletesAndCancels(t *testing.T) {
	cancelled := make(chan struct{})
	api := sandboxAPIForTest(t, func(w http.ResponseWriter, r *http.Request) {
		var body sandboxapi.ProcessRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Command != "echo hello" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected typed streaming request: body=%+v err=%v headers=%v", body, err, r.Header)
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Error("missing stream Accept")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, "{\"type\":\"stdout\",\"data\":\"first\"}\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resp, err := api.PostProcessRaw(ctx, sandboxapi.ProcessRequest{Command: "echo hello"}, sandboxapi.RawRequestOptions{Accept: "text/event-stream"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, len("{\"type\":\"stdout\",\"data\":\"first\"}\n"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "{\"type\":\"stdout\",\"data\":\"first\"}\n" {
		t.Fatalf("unexpected event: %q", buf)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not reach server")
	}
}

func TestSandboxErrorsKeepStatusHeadersAndFallbackBody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		code  int
		body  string
		typed bool
	}{
		{"json", 404, `{"error":"missing"}`, true},
		{"html", 500, "<html>upstream failed</html>", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := sandboxAPIForTest(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := api.GetFilesystem(t.Context(), "/missing", sandboxapi.GetFilesystemPathParams{})
			if tc.typed {
				var typed *sandboxapi.ResponseErrorResponse
				if !errors.As(err, &typed) || typed.StatusCode != tc.code || typed.Header.Get("Retry-After") != "3" {
					t.Fatalf("lost typed error: %v", err)
				}
			} else {
				var raw *sandboxapi.ResponseError
				if !errors.As(err, &raw) || raw.StatusCode != tc.code || raw.Body != tc.body || raw.Header.Get("Retry-After") != "3" {
					t.Fatalf("lost fallback error: %v", err)
				}
			}
		})
	}
}

func TestSandboxManagementTeamHeader(t *testing.T) {
	team := "team-header-only"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Team-Id") != team {
			t.Error("missing team header")
		}
		if r.URL.RawQuery != "" {
			t.Errorf("header parameter leaked into query: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"name":"created","status":"DEPLOYING"}`)
	}))
	defer server.Close()
	c, err := client.NewManagementClient(client.ManagementClientOptions{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.API().CreateSandbox(t.Context(), managementapi.CreateSandboxParams{XTeamId: &team}, managementapi.CreateSandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
}

// Match the Accept negotiation and query forwarding of the JavaScript client.
func TestSandboxTextStreams(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprint(watch), func(t *testing.T) {
			api := sandboxAPIForTest(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept") != "text/plain" {
					t.Errorf("Accept = %q", r.Header.Get("Accept"))
				}
				if watch && (r.URL.EscapedPath() != "/watch/filesystem/%2Fapp" || r.URL.Query().Get("ignore") != "node_modules,dist") {
					t.Errorf("unexpected watch URL: %s", r.URL)
				}
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, "event\n")
			})
			var resp *http.Response
			var err error
			if watch {
				ignore := "node_modules,dist"
				resp, err = api.GetWatchFilesystem(t.Context(), "/app", sandboxapi.GetWatchFilesystemPathParams{Ignore: &ignore})
			} else {
				resp, err = api.GetProcessLogsStream(t.Context(), "p-1")
			}
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil || string(got) != "event\n" {
				t.Fatalf("raw response = %q, %v", got, err)
			}
		})
	}
}
