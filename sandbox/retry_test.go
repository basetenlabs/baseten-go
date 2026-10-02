package sandbox

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
)

// shortBackoff shortens the retry backoff for one test, so retries take
// milliseconds instead of seconds. It changes package state, so a test using
// it must not run in parallel. The e2e tests get the real backoff back.
func shortBackoff(t *testing.T) {
	base, maxDelay := backoffBase, backoffMax
	backoffBase, backoffMax = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { backoffBase, backoffMax = base, maxDelay })
}

func TestRetryOptionsResolved(t *testing.T) {
	resolved := RetryOptions{ReadMaxRetries: 0, GatewayMaxRetries: -1, UploadMaxRetries: 7}.resolved()
	require.Equal(t, defaultReadMaxRetries, resolved.ReadMaxRetries)
	require.Equal(t, 0, resolved.GatewayMaxRetries)
	require.Equal(t, 7, resolved.UploadMaxRetries)
}

// Each case produces its failure on a real connection, since the errors that
// mark a dropped connection differ by platform and protocol.
func TestIsTransientResetError(t *testing.T) {
	t.Run("HTTP2StreamReset", func(t *testing.T) {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			// Aborting the handler resets its stream.
			panic(http.ErrAbortHandler)
		}))
		srv.EnableHTTP2 = true
		srv.StartTLS()
		t.Cleanup(srv.Close)
		err := requestError(t, srv.Client(), srv.URL)
		require.Contains(t, err.Error(), "stream error:")
		require.True(t, isTransientResetError(err), "expected transient: %v", err)
	})

	t.Run("HTTP2ConnectionDropped", func(t *testing.T) {
		conns := make(chan net.Conn, 1)
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			(<-conns).Close()
		}))
		srv.Config.ConnState = func(conn net.Conn, state http.ConnState) {
			if state == http.StateNew {
				conns <- conn
			}
		}
		srv.EnableHTTP2 = true
		srv.StartTLS()
		t.Cleanup(srv.Close)
		err := requestError(t, srv.Client(), srv.URL)
		require.True(t, isTransientResetError(err), "expected transient: %v", err)
	})

	t.Run("ClosedBeforeResponse", func(t *testing.T) {
		srv := hijackServer(t, func(conn net.Conn) {
			conn.Close()
		})
		err := requestError(t, srv.Client(), srv.URL)
		require.True(t, errors.Is(err, io.EOF), "expected io.EOF: %v", err)
		require.True(t, isTransientResetError(err), "expected transient: %v", err)
	})

	t.Run("ClosedMidBody", func(t *testing.T) {
		srv := hijackServer(t, func(conn net.Conn) {
			conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial"))
			conn.Close()
		})
		resp, err := srv.Client().Get(srv.URL)
		require.NoError(t, err)
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		require.True(t, errors.Is(err, io.ErrUnexpectedEOF), "expected io.ErrUnexpectedEOF: %v", err)
		require.True(t, isTransientResetError(err), "expected transient: %v", err)
	})

	t.Run("ConnectionReset", func(t *testing.T) {
		srv := hijackServer(t, func(conn net.Conn) {
			// Closing with no linger sends a reset instead of a clean close.
			conn.(*net.TCPConn).SetLinger(0)
			conn.Close()
		})
		err := requestError(t, srv.Client(), srv.URL)
		require.True(t, isTransientResetError(err), "expected transient: %v", err)
	})

	t.Run("ConnectionRefused", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := listener.Addr().String()
		listener.Close()
		err = requestError(t, http.DefaultClient, "http://"+addr)
		require.True(t, isTransientResetError(err), "expected transient: %v", err)
	})

	// The standard library cannot be made to send a GOAWAY on demand without
	// a raw HTTP/2 framer, so this is its text as the transport words it.
	t.Run("HTTP2GoAwayText", func(t *testing.T) {
		err := errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=1, ErrCode=NO_ERROR, debug=""`)
		require.True(t, isTransientResetError(err), "expected transient: %v", err)
	})

	t.Run("APIErrorNeverTransient", func(t *testing.T) {
		err := &APIError{Status: 500, Body: "GOAWAY", description: "stream error: GOAWAY"}
		require.False(t, isTransientResetError(err), "API errors are responses, not dropped connections")
	})

	t.Run("CanceledNeverTransient", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		t.Cleanup(srv.Close)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		require.NoError(t, err)
		_, err = srv.Client().Do(req)
		require.Error(t, err)
		require.False(t, isTransientResetError(err), "cancellation is not transient: %v", err)
	})
}

func TestRetryIdempotent(t *testing.T) {
	shortBackoff(t)
	reset := &net.OpError{Op: "read", Err: errors.New("stream error: stream ID 1; INTERNAL_ERROR")}

	t.Run("RetriesTransientUpToLimit", func(t *testing.T) {
		calls := 0
		_, err := retryIdempotent(t.Context(), 2, 0, func() (int, error) {
			calls++
			return 0, reset
		})
		require.Error(t, err)
		require.Equal(t, 3, calls)
	})

	t.Run("SucceedsAfterTransient", func(t *testing.T) {
		calls := 0
		result, err := retryIdempotent(t.Context(), 2, 0, func() (int, error) {
			calls++
			if calls == 1 {
				return 0, reset
			}
			return 42, nil
		})
		require.NoError(t, err)
		require.Equal(t, 42, result)
	})

	t.Run("GatewayHasOwnBudget", func(t *testing.T) {
		calls := 0
		_, err := retryIdempotent(t.Context(), 0, 1, func() (int, error) {
			calls++
			return 0, &GatewayError{APIError: APIError{Status: 503}}
		})
		require.ErrorAs[*GatewayError](t, err)
		require.Equal(t, 2, calls)
	})

	t.Run("NonTransientNotRetried", func(t *testing.T) {
		calls := 0
		_, err := retryIdempotent(t.Context(), 5, 5, func() (int, error) {
			calls++
			return 0, &APIError{Status: 404}
		})
		require.ErrorAs[*APIError](t, err)
		require.Equal(t, 1, calls)
	})

	t.Run("StopsWhenContextDone", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		_, err := retryIdempotent(ctx, 5, 0, func() (int, error) {
			calls++
			cancel()
			return 0, reset
		})
		require.Error(t, err)
		require.Equal(t, 1, calls)
	})

	t.Run("DeadlineDuringBackoffReturnsContextError", func(t *testing.T) {
		// A backoff far longer than the deadline, so the deadline passes
		// while waiting to retry.
		base, maxDelay := backoffBase, backoffMax
		backoffBase, backoffMax = time.Hour, time.Hour
		t.Cleanup(func() { backoffBase, backoffMax = base, maxDelay })
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		_, err := retryIdempotent(ctx, 5, 0, func() (int, error) {
			return 0, reset
		})
		require.True(t, errors.Is(err, context.DeadlineExceeded), "expected context.DeadlineExceeded, got %v", err)
	})
}

// hijackServer starts an HTTP/1.1 server whose handler takes over each
// connection and gives it to onConn.
func hijackServer(t *testing.T, onConn func(net.Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			panic(err)
		}
		onConn(conn)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// requestError sends a GET that must fail and returns its error.
func requestError(t *testing.T, client *http.Client, url string) error {
	t.Helper()
	resp, err := client.Get(url)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected request to %s to fail", url)
	}
	return err
}
