package bdn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
	"github.com/basetenlabs/baseten-go/internal/volume"
)

func TestRetriesTransientStatuses(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		wantRetried bool
	}{
		{"service unavailable", http.StatusServiceUnavailable, true},
		{"internal", http.StatusInternalServerError, true},
		{"gateway timeout", http.StatusGatewayTimeout, true},
		{"rate limited", http.StatusTooManyRequests, true},
		{"bad request", http.StatusBadRequest, false},
		{"not found", http.StatusNotFound, false},
		{"conflict", http.StatusConflict, false},
		{"gone", http.StatusGone, false},
		{"too large", http.StatusRequestEntityTooLarge, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writeError(w, tc.status, "CODE", "SOME_REASON", "nope")
			})

			_, err := client.Resolve(context.Background(), "ns/vol")
			require.Error(t, err)
			if tc.wantRetried {
				require.Equal(t, int64(5), calls.Load())
			} else {
				require.Equal(t, int64(1), calls.Load())
			}
		})
	}
}

func TestSucceedsAfterRetry(t *testing.T) {
	digest := volume.Digest{0xaa}
	var calls atomic.Int64
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 4 {
			writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "UNAVAILABLE", "shedding")
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"digest": digest.String(), "target": volume.TargetForDigest(digest), "created": true,
		})
	})

	result, err := client.UploadObject(context.Background(),
		testSession("/objects/{digest}"), ContentTypeChunk, digest, []byte("x"))
	require.NoError(t, err)
	require.Equal(t, int64(4), calls.Load())
	// The upload succeeded, but the origin asked for less along the way. That
	// signal has to survive the retry, or a client that backs off successfully
	// looks exactly like one that never needed to.
	require.Equal(t, volume.Stall, result.Outcome)
}

// TestRetryAfterCapsTheWait pins the two properties of the server's hint: it
// replaces the local backoff, and it is a ceiling rather than the wait itself.
func TestRetryAfterCapsTheWait(t *testing.T) {
	var calls atomic.Int64
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			// Far longer than the client's own cap, which must bound it.
			w.Header().Set("Retry-After", "3600")
			writeError(w, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "RATE_LIMITED", "slow down")
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"resolved": map[string]any{
				"origin_digest": volume.Digest{}.String(),
				"target":        volume.Target{RelativeKey: "k"},
			},
			"origin": map[string]any{},
		})
	})

	start := time.Now()
	_, err := client.Resolve(context.Background(), "ns/vol")
	require.NoError(t, err)
	require.Equal(t, int64(2), calls.Load())
	require.True(t, time.Since(start) < time.Second, "the hour-long hint was not capped")
}

func TestRetryAfterParsing(t *testing.T) {
	tests := []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"5", 5 * time.Second, true},
		{"0", 0, true},
		{" 2 ", 2 * time.Second, true},
		{"", 0, false},
		{"-1", 0, false},
		// The HTTP-date form is ignored rather than guessed at: acting on it
		// would mean trusting this machine's clock to agree with the server's.
		{"Wed, 21 Oct 2026 07:28:00 GMT", 0, false},
		{"soon", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			header := http.Header{}
			if tc.value != "" {
				header.Set("Retry-After", tc.value)
			}
			got, ok := retryAfter(header)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestBackoffStaysWithinItsCeiling(t *testing.T) {
	cfg := RetryConfig{MaxAttempts: 5, Base: 50 * time.Millisecond, Cap: time.Second}
	for attempt := 1; attempt <= 6; attempt++ {
		// The first attempt to be waited on is attempt 1, and it waits within
		// one base interval rather than two: the shift counts the waits
		// already spent, not the attempt's own number.
		ceiling := min(cfg.Base<<(attempt-1), cfg.Cap)
		for range 50 {
			got := cfg.backoff(attempt)
			require.True(t, got >= 0 && got <= ceiling,
				"attempt %d backoff %v outside [0, %v]", attempt, got, ceiling)
		}
	}
}

// TestBackoffCeilingIsTheDoublingSequence pins the formula itself rather than
// only the bound, so a shift that is off by one cannot pass by staying under
// a ceiling that moved with it.
func TestBackoffCeilingIsTheDoublingSequence(t *testing.T) {
	cfg := RetryConfig{MaxAttempts: 99, Base: 10 * time.Millisecond, Cap: time.Hour}
	// The first wait is bounded by one base interval, and each wait after it
	// by twice the one before.
	for attempt, want := 1, cfg.Base; attempt <= 8; attempt, want = attempt+1, want*2 {
		var high time.Duration
		for range 500 {
			if got := cfg.backoff(attempt); got > high {
				high = got
			}
		}
		require.True(t, high <= want, "attempt %d exceeded %v with %v", attempt, want, high)
		require.True(t, high > want/2, "attempt %d never approached %v (max %v)", attempt, want, high)
	}

	// The clamp survives: past the shift limit the ceiling is the cap, not an
	// overflowed shift. attempt-1 is what gets clamped, so this covers the
	// boundary the subtraction moved.
	wide := RetryConfig{MaxAttempts: 99, Base: time.Millisecond, Cap: 2 * time.Second}
	for _, attempt := range []int{33, 34, 64, 200} {
		for range 50 {
			got := wide.backoff(attempt)
			require.True(t, got >= 0 && got <= wide.Cap,
				"attempt %d backoff %v outside [0, %v]", attempt, got, wide.Cap)
		}
	}
}

func TestClassifyTransport(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want volume.Outcome
	}{
		// A connection the peer had already closed says nothing about load:
		// it is a property of connection reuse, not of saturation.
		{"eof", errors.New(`Get "http://host": EOF`), volume.Neutral},
		{"wrapped eof", fmt.Errorf("read response: %w", io.EOF), volume.Neutral},
		{"unexpected eof", io.ErrUnexpectedEOF, volume.Neutral},
		{"idle close", errors.New("http: server closed idle connection"), volume.Neutral},
		// These say the origin, or the path to it, cannot keep up.
		{"connection refused", errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), volume.Stall},
		{"timeout", errors.New("context deadline exceeded (Client.Timeout exceeded)"), volume.Stall},
		{"no buffer space", errors.New("write tcp: no buffer space available"), volume.Stall},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, classifyTransport(tc.err))
		})
	}
}

// TestRetryPastAClosedConnectionIsNeutral pins the other half of the
// classification. A request that succeeded only after the peer dropped a
// connection carries no evidence about capacity — connection reuse churn is
// not the origin asking for less — so it must not report success either, which
// an adaptive limiter would read as room to grow.
func TestRetryPastAClosedConnectionIsNeutral(t *testing.T) {
	digest := volume.Digest{0xaa}
	var calls atomic.Int64
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			hijacked, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = hijacked.Close()
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"digest": digest.String(), "target": volume.TargetForDigest(digest), "created": true,
		})
	})

	result, err := client.UploadObject(context.Background(),
		testSession("/objects/{digest}"), ContentTypeChunk, digest, []byte("x"))
	require.NoError(t, err)
	require.Equal(t, int64(2), calls.Load())
	require.Equal(t, volume.Neutral, result.Outcome)
}

// TestAStallOutranksAClosedConnection covers a request that saw both: the
// pushback is the signal that matters.
func TestAStallOutranksAClosedConnection(t *testing.T) {
	digest := volume.Digest{0xaa}
	var calls atomic.Int64
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			hijacked, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = hijacked.Close()
		case 2:
			writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "UNAVAILABLE", "shedding")
		default:
			writeJSON(t, w, http.StatusOK, map[string]any{
				"digest": digest.String(), "target": volume.TargetForDigest(digest), "created": true,
			})
		}
	})

	result, err := client.UploadObject(context.Background(),
		testSession("/objects/{digest}"), ContentTypeChunk, digest, []byte("x"))
	require.NoError(t, err)
	require.Equal(t, volume.Stall, result.Outcome)
}

// TestRetriesTransportFailures covers a server that hangs up without
// answering, which the retry loop must treat as transient.
func TestRetriesTransportFailures(t *testing.T) {
	var calls atomic.Int64
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			// Closing the underlying connection mid-response surfaces as a
			// transport error rather than a status.
			hijacked, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = hijacked.Close()
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"resolved": map[string]any{
				"origin_digest": volume.Digest{}.String(),
				"target":        volume.Target{RelativeKey: "k"},
			},
			"origin": map[string]any{},
		})
	})

	_, err := client.Resolve(context.Background(), "ns/vol")
	require.NoError(t, err)
	require.Equal(t, int64(3), calls.Load())
}

func TestStopsOnCancelledContext(t *testing.T) {
	var calls atomic.Int64
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "UNAVAILABLE", "shedding")
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Resolve(ctx, "ns/vol")
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "expected a cancellation, got %v", err)
	require.Equal(t, int64(0), calls.Load())
}

func TestDecodesErrorEnvelope(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusConflict, "ABORTED", volume.ReasonCASConflict, "expected 7, current 9")
	})

	_, err := client.Commit(context.Background(), CommitRequest{
		Namespace: "ns", Volume: "vol", UploadID: "u", IdempotencyKey: "k",
	})
	require.Error(t, err)

	var serviceErr *volume.Error
	require.True(t, errors.As(err, &serviceErr), "expected a volume.Error, got %T", err)
	require.Equal(t, "ABORTED", serviceErr.Code)
	require.Equal(t, volume.ReasonCASConflict, serviceErr.Reason)
	require.Equal(t, volume.ErrorDomain, serviceErr.Domain)
	require.Equal(t, http.StatusConflict, serviceErr.HTTPStatus)
	require.Equal(t, "7", serviceErr.Metadata["expected_sequence"])
	// The typed retry hint is surfaced; the unrecognized detail beside it is
	// ignored rather than being an error of its own.
	require.Equal(t, 250*time.Millisecond, serviceErr.RetryDelay)
	require.True(t, volume.HasReason(err, volume.ReasonCASConflict), "HasReason should match")
}

// TestDecodesExpiredSession covers the status whose meaning the canonical code
// set cannot express, and which a push has to recognize to say anything useful.
func TestDecodesExpiredSession(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusGone, "FAILED_PRECONDITION", volume.ReasonUploadSessionExpired, "past expires_at")
	})

	_, err := client.UploadObject(context.Background(),
		testSession("/objects/{digest}"), ContentTypeChunk, volume.Digest{}, []byte("x"))
	require.Error(t, err)
	require.True(t, volume.HasReason(err, volume.ReasonUploadSessionExpired), "got %v", err)
}

// TestFallsBackWhenThereIsNoEnvelope covers a proxy answering with something
// that is not the service's error shape at all.
func TestFallsBackWhenThereIsNoEnvelope(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>502 Bad Gateway</html>")
	})

	_, err := client.Resolve(context.Background(), "ns/vol")
	require.Error(t, err)

	var serviceErr *volume.Error
	require.True(t, errors.As(err, &serviceErr), "expected a volume.Error, got %T", err)
	require.Equal(t, "INTERNAL", serviceErr.Code)
	require.Equal(t, http.StatusBadGateway, serviceErr.HTTPStatus)
	require.Equal(t, "", serviceErr.Domain)
}

// TestReExchangesRejectedToken covers a transfer outliving its credential.
// Tokens cannot be renewed, so the client asks for a fresh one and carries on
// rather than failing partway through with work already done.
func TestReExchangesRejectedToken(t *testing.T) {
	var seen []string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		seen = append(seen, token)
		if token != "refreshed" {
			writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", volume.ReasonUnauthenticated, "expired")
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"resolved": map[string]any{
				"origin_digest": volume.Digest{}.String(),
				"target":        volume.Target{RelativeKey: "k"},
			},
			"origin": map[string]any{},
		})
	})

	_, err := client.Resolve(context.Background(), "ns/vol")
	require.NoError(t, err)
	require.Len(t, seen, 2)
	require.Equal(t, "refreshed", seen[1])
}

// TestGivesUpOnASecondRejection covers a credential that is wrong rather than
// stale: asking for another would loop.
func TestGivesUpOnASecondRejection(t *testing.T) {
	var calls atomic.Int64
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", volume.ReasonUnauthenticated, "no")
	})

	_, err := client.Resolve(context.Background(), "ns/vol")
	require.Error(t, err)
	require.Equal(t, int64(2), calls.Load())
	require.True(t, volume.HasReason(err, volume.ReasonUnauthenticated), "got %v", err)
}

func TestNewIdempotencyKeyIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		key, err := NewIdempotencyKey()
		require.NoError(t, err)
		require.False(t, seen[key], "duplicate key %q", key)
		seen[key] = true
	}
}

// TestCredentialExchangeDoesNotSpendARetry pins that being handed a fresh
// credential is not one of the attempts the retry budget pays for.
//
// The property is only observable when the budget is tight enough to matter.
// The two tests above run with attempts to spare, so they pass whether or not
// the exchange spends one; here a single retry has to survive the 401 to reach
// the 200, and if the exchange consumed it the 503 would be terminal.
func TestCredentialExchangeDoesNotSpendARetry(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A fixed sequence: the status is what drives the branch under test,
		// not which token arrived — token rotation has its own test.
		switch calls.Add(1) {
		case 1:
			writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", volume.ReasonUnauthenticated, "expired")
		case 2:
			writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "UNAVAILABLE", "shedding")
		default:
			writeJSON(t, w, http.StatusOK, map[string]any{
				"resolved": map[string]any{
					"origin_digest": volume.Digest{}.String(),
					"target":        volume.Target{RelativeKey: "k"},
				},
				"origin": map[string]any{},
			})
		}
	}))
	defer server.Close()

	client, err := New(Options{
		HTTPClient: server.Client(),
		Tokens: func(_ context.Context, rejected string) (string, string, error) {
			if rejected != "" {
				return "refreshed", server.URL, nil
			}
			return "initial", server.URL, nil
		},
		// One retry, and a backoff short enough that the test does not sleep
		// for a property the backoff plays no part in.
		Retry: RetryConfig{MaxAttempts: 2, Base: time.Microsecond, Cap: time.Millisecond},
	})
	require.NoError(t, err)

	_, err = client.Resolve(context.Background(), "ns/vol")
	require.NoError(t, err)
	// Three requests: the rejected one, the single retry the budget allows,
	// and the one that succeeded. Two would mean the exchange took the retry.
	require.Equal(t, int64(3), calls.Load())
}
