package sandbox

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"strings"
	"syscall"
	"time"
)

// RetryOptions sets how many times calls to a sandbox are retried on dropped
// connections and gateway errors. Only calls that are safe to repeat are ever
// retried. For each field, zero uses the default and a negative value turns
// retries off.
type RetryOptions struct {
	// ReadMaxRetries is the retry limit for reads on a dropped or reset
	// connection. Defaults to 5.
	ReadMaxRetries int

	// GatewayMaxRetries is the retry limit for 502, 503, and 504 responses
	// from the edge in front of a sandbox. Defaults to 2.
	GatewayMaxRetries int

	// UploadMaxRetries is the retry limit for uploads on a dropped or reset
	// connection. Defaults to 3.
	UploadMaxRetries int
}

const (
	defaultReadMaxRetries    = 5
	defaultGatewayMaxRetries = 2
	defaultUploadMaxRetries  = 3
)

// Retry backoff bounds. Variables so tests can shorten them.
var (
	backoffBase = 200 * time.Millisecond
	backoffMax  = 2 * time.Second
)

// resolved fills in the defaults and turns negative limits into zero.
func (o RetryOptions) resolved() RetryOptions {
	return RetryOptions{
		ReadMaxRetries:    resolveRetryLimit(o.ReadMaxRetries, defaultReadMaxRetries),
		GatewayMaxRetries: resolveRetryLimit(o.GatewayMaxRetries, defaultGatewayMaxRetries),
		UploadMaxRetries:  resolveRetryLimit(o.UploadMaxRetries, defaultUploadMaxRetries),
	}
}

func resolveRetryLimit(limit, defaultLimit int) int {
	if limit == 0 {
		return defaultLimit
	}
	return max(limit, 0)
}

// Errors, found anywhere in an error's chain, that mean the connection dropped
// before a response arrived. io.EOF is what a connection closed before any
// response looks like.
var transientErrors = append([]error{
	syscall.ECONNRESET,
	syscall.ECONNREFUSED,
	syscall.ETIMEDOUT,
	syscall.EPIPE,
	io.ErrUnexpectedEOF,
	io.EOF,
}, platformTransientErrors...)

// HTTP/2 reset markers in error text. The standard library does not export
// its HTTP/2 error types, so text is all there is to match. Bare
// "INTERNAL_ERROR" is deliberately absent, since it also appears on failures
// that are not transient.
var transientResetMarkers = []string{
	// The server closed the connection: "http2: server sent GOAWAY and
	// closed the connection; ..."
	"GOAWAY",
	// The server is rate limiting stream resets, an error code name.
	"ENHANCE_YOUR_CALM",
	// The server reset one stream: "stream error: stream ID 3; ...".
	"stream error:",
	// The connection dropped mid-request: "http2: client connection lost".
	"client connection lost",
}

// isTransientResetError reports whether an error is a dropped or reset
// connection, as opposed to a response from the server. Only then is
// repeating an idempotent call safe.
func isTransientResetError(err error) bool {
	// A response came back, so the server handled the request, even when its
	// body happens to contain one of the markers.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	for _, transient := range transientErrors {
		if errors.Is(err, transient) {
			return true
		}
	}
	text := err.Error()
	for _, marker := range transientResetMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// backoffDelay is exponential backoff capped at a maximum, plus up to one base
// delay of jitter so that many clients failing together do not retry
// together.
func backoffDelay(attempt int) time.Duration {
	capped := min(backoffBase<<(attempt-1), backoffMax)
	return capped + rand.N(backoffBase)
}

// retryIdempotent runs an idempotent call, retrying dropped connections and
// edge gateway errors on separate budgets. It must never wrap a call with side
// effects, since a dropped connection does not tell whether the server acted
// on it.
func retryIdempotent[T any](ctx context.Context, maxRetries, gatewayMaxRetries int, call func() (T, error)) (T, error) {
	attempt := 0
	gatewayAttempt := 0
	for {
		result, err := call()
		if err == nil || ctx.Err() != nil {
			return result, err
		}
		var delay time.Duration
		var gatewayErr *GatewayError
		if errors.As(err, &gatewayErr) {
			gatewayAttempt++
			if gatewayAttempt > gatewayMaxRetries {
				return result, err
			}
			delay = backoffDelay(gatewayAttempt)
		} else {
			attempt++
			if attempt > maxRetries || !isTransientResetError(err) {
				return result, err
			}
			delay = backoffDelay(attempt)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			// The caller's cancellation or deadline, not the error being
			// retried, is why the call ends.
			timer.Stop()
			var zero T
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
}
