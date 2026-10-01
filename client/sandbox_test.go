package client_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/sandboxapi"
	"github.com/basetenlabs/baseten-go/internal/require"
)

func newSandboxClient(t *testing.T, srv *httptest.Server, opts client.SandboxClientOptions) *sandboxapi.Client {
	t.Helper()
	opts.BaseURL = srv.URL
	cl, err := client.NewSandboxClient(opts)
	require.NoError(t, err)
	return cl.API()
}

func TestSandboxClientAuth(t *testing.T) {
	t.Run("SendsBearerToken", func(t *testing.T) {
		var capture requestCapture
		srv := newTestServer(t, 200, map[string]any{"status": "ok"}, &capture)
		_, err := newSandboxClient(t, srv, client.SandboxClientOptions{Token: "test-token"}).GetHealth(t.Context())
		require.NoError(t, err)
		require.Equal(t, "/health", capture.Path)
		require.Equal(t, "Bearer test-token", capture.Header.Get("Authorization"))
		require.Regexp(t, `^baseten-go/\S+`, capture.Header.Get("User-Agent"))
	})

	t.Run("EmptyTokenOmitsAuthorization", func(t *testing.T) {
		var capture requestCapture
		srv := newTestServer(t, 200, map[string]any{"status": "ok"}, &capture)
		_, err := newSandboxClient(t, srv, client.SandboxClientOptions{}).GetHealth(t.Context())
		require.NoError(t, err)
		require.Equal(t, "", capture.Header.Get("Authorization"))
	})

	t.Run("ClonesHeaders", func(t *testing.T) {
		var capture requestCapture
		srv := newTestServer(t, 200, map[string]any{"status": "ok"}, &capture)
		headers := http.Header{"X-Test": {"original"}}
		api := newSandboxClient(t, srv, client.SandboxClientOptions{Token: "test-token", Headers: headers})
		headers.Set("X-Test", "changed")
		_, err := api.GetHealth(t.Context())
		require.NoError(t, err)
		require.Equal(t, "original", capture.Header.Get("X-Test"))
		require.Equal(t, "", headers.Get("Authorization"))
	})
}

func TestSandboxAdvancedRequest(t *testing.T) {
	t.Run("MultipartContentType", func(t *testing.T) {
		var capture requestCapture
		srv := newTestServer(t, 200, map[string]any{"partNumber": 2}, &capture)
		// A client-wide Content-Type must not override the caller's, which
		// carries the boundary.
		api := newSandboxClient(t, srv, client.SandboxClientOptions{
			Headers: http.Header{"Content-Type": {"application/json"}},
		})
		_, err := api.PutFilesystemMultipartPart(t.Context(), "up-1",
			sandboxapi.PutFilesystemMultipartUploadIdPartParams{PartNumber: 2},
			sandboxapi.AdvancedRequest{
				Body:        strings.NewReader("--b\r\n\r\ndata\r\n--b--\r\n"),
				ContentType: "multipart/form-data; boundary=b",
			})
		require.NoError(t, err)
		require.Equal(t, "/filesystem-multipart/up-1/part", capture.Path)
		require.Equal(t, "2", capture.Query.Get("partNumber"))
		require.Equal(t, "multipart/form-data; boundary=b", capture.Header.Get("Content-Type"))
		require.Len(t, capture.Header.Values("Content-Type"), 1)
		require.Equal(t, "--b\r\n\r\ndata\r\n--b--\r\n", capture.Body)
	})

	t.Run("DefaultsToDeclaredContentType", func(t *testing.T) {
		var capture requestCapture
		srv := newTestServer(t, 200, map[string]any{"message": "ok"}, &capture)
		_, err := newSandboxClient(t, srv, client.SandboxClientOptions{}).PostProcessStdin(t.Context(), "p-1",
			sandboxapi.AdvancedRequest{Body: strings.NewReader("\x01\x02\x03")})
		require.NoError(t, err)
		require.Equal(t, "application/octet-stream", capture.Header.Get("Content-Type"))
		require.Equal(t, "\x01\x02\x03", capture.Body)
	})
}

func TestSandboxRaw(t *testing.T) {
	t.Run("RequestsNonJSONTypeUnread", func(t *testing.T) {
		var capture requestCapture
		srv := newTestServer(t, 200, map[string]any{"pid": "1"}, &capture)
		resp, err := newSandboxClient(t, srv, client.SandboxClientOptions{}).PostProcessRaw(t.Context(),
			sandboxapi.ProcessRequest{Command: "echo hi"})
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, "text/event-stream", capture.Header.Get("Accept"))
		require.Contains(t, capture.Body, `"command":"echo hi"`)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(body), `"pid":"1"`)
	})

	t.Run("RawOnlyQueryAndAccept", func(t *testing.T) {
		var capture requestCapture
		srv := newTestServer(t, 200, nil, &capture)
		ignore := "node_modules,dist"
		resp, err := newSandboxClient(t, srv, client.SandboxClientOptions{}).GetWatchFilesystemRaw(t.Context(), "/app",
			sandboxapi.GetWatchFilesystemPathParams{Ignore: &ignore})
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, "/watch/filesystem/%2Fapp", capture.RawPath)
		require.Equal(t, "node_modules,dist", capture.Query.Get("ignore"))
		require.Equal(t, "text/plain", capture.Header.Get("Accept"))
	})
}

func TestSandboxMultiStatus(t *testing.T) {
	t.Run("200", func(t *testing.T) {
		srv := newTestServer(t, 200, map[string]any{"manifest": map[string]any{}, "size": 7}, nil)
		result, err := newSandboxClient(t, srv, client.SandboxClientOptions{}).PostArchiveExport(t.Context(),
			sandboxapi.ExportOptions{})
		require.NoError(t, err)
		require.Equal(t, 200, result.StatusCode)
		require.Nil(t, result.JSON202)
		require.Equal(t, 7, *result.JSON200.Size)
	})

	t.Run("202", func(t *testing.T) {
		srv := newTestServer(t, 202, map[string]any{"state": "running"}, nil)
		async := true
		result, err := newSandboxClient(t, srv, client.SandboxClientOptions{}).PostArchiveExport(t.Context(),
			sandboxapi.ExportOptions{Async: &async})
		require.NoError(t, err)
		require.Equal(t, 202, result.StatusCode)
		require.Nil(t, result.JSON200)
		require.Equal(t, "running", string(*result.JSON202.State))
	})
}

func TestSandboxErrors(t *testing.T) {
	t.Run("TypedKeepsStatus", func(t *testing.T) {
		srv := newTestServer(t, 404, map[string]any{"error": "no such file"}, nil)
		_, err := newSandboxClient(t, srv, client.SandboxClientOptions{}).GetFilesystem(t.Context(), "/missing",
			sandboxapi.GetFilesystemPathParams{})
		typed := require.ErrorAs[*sandboxapi.ResponseErrorResponse](t, err)
		require.Equal(t, 404, typed.StatusCode)
		require.Equal(t, "no such file", typed.ErrorResponse.Error)
	})

	t.Run("NonJSONFallsBackToUntyped", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(500)
			io.WriteString(w, "<html>Bad Gateway</html>")
		}))
		t.Cleanup(srv.Close)
		_, err := newSandboxClient(t, srv, client.SandboxClientOptions{}).GetFilesystem(t.Context(), "/x",
			sandboxapi.GetFilesystemPathParams{})
		untyped := require.ErrorAs[*sandboxapi.ResponseError](t, err)
		require.Equal(t, 500, untyped.StatusCode)
		require.Equal(t, "<html>Bad Gateway</html>", untyped.Body)
	})
}
