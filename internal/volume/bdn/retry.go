package bdn

import (
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-go/internal/volume"
)

// RetryConfig is the retry policy for every request to the volume service.
type RetryConfig struct {
	// MaxAttempts counts the first try, so five means four retries.
	MaxAttempts int

	// Base is how long the first retry may wait.
	Base time.Duration

	// Cap bounds any single wait, including one the server asked for.
	Cap time.Duration
}

// DefaultRetryConfig is the established retry policy for the volume service.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{MaxAttempts: 5, Base: 50 * time.Millisecond, Cap: time.Second}
}

func (c RetryConfig) withDefaults() RetryConfig {
	def := DefaultRetryConfig()
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = def.MaxAttempts
	}
	if c.Base <= 0 {
		c.Base = def.Base
	}
	if c.Cap <= 0 {
		c.Cap = def.Cap
	}
	return c
}

// backoff returns how long to wait before the given retry, counting from one.
//
// The wait is uniform over the whole interval up to an exponentially growing
// ceiling, not the ceiling itself. Sleeping the exact value would keep a fleet
// of clients that failed together retrying together, rebuilding the pileup
// that caused the failure.
func (c RetryConfig) backoff(attempt int) time.Duration {
	ceiling := c.Base << min(attempt-1, 32)
	if ceiling > c.Cap || ceiling <= 0 {
		ceiling = c.Cap
	}
	return jitter(ceiling)
}

// jitter picks uniformly from [0, ceiling].
func jitter(ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(ceiling) + 1))
}

// retryableStatus reports whether a status is worth trying again. Everything
// else, a 4xx that is not a rate limit included, means the request itself was
// wrong and will be wrong again.
func retryableStatus(status int) bool {
	return status >= 500 || status == http.StatusTooManyRequests
}

// retryAfter reads the Retry-After header in its delta-seconds form. The HTTP
// date form is ignored: the server sends seconds, and a date would need a
// clock this client cannot trust to agree with the server's.
func retryAfter(header http.Header) (time.Duration, bool) {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0, false
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// waitFor returns how long to wait after a retryable response. A server hint
// replaces the local backoff rather than stacking with it, and is still
// jittered: told to wait a second, a fleet must not wake as one.
func (c RetryConfig) waitFor(attempt int, header http.Header) time.Duration {
	if hint, ok := retryAfter(header); ok {
		return jitter(min(hint, c.Cap))
	}
	return c.backoff(attempt)
}

// classifyTransport decides what a transport failure says about the origin's
// capacity. A connection the peer closed before answering says nothing: it is
// a property of connection reuse, and treating it as backpressure is a
// mistake that strangles throughput. Everything else — a refused
// connect, a timeout, a local buffer exhaustion — is the origin or the path to
// it being unable to keep up.
func classifyTransport(err error) volume.Outcome {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return volume.Neutral
	}
	// net/http reports a closed idle connection as an EOF the error chain
	// stringifies rather than wraps.
	if text := err.Error(); strings.HasSuffix(text, "EOF") || strings.Contains(text, "server closed idle connection") {
		return volume.Neutral
	}
	return volume.Stall
}
