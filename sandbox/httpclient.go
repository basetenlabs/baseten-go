package sandbox

import (
	"net/http"
	"time"
)

// responseHeaderTimeout bounds how long a server may accept a connection
// without sending response headers, matching the value the Blaxel codebase
// uses. A hung server then fails the request instead of hanging it until the
// operating system gives up. Header arrival is all it bounds: a long body,
// such as a command's streamed output, is not affected.
const responseHeaderTimeout = 30 * time.Second

// newDefaultHTTPClient builds the client used when the caller supplies none.
// http.DefaultClient carries no timeout of any kind, and the timeout lives
// only on a transport, so the default transport is cloned to carry one — at
// the cost of this client's connections not sharing the process-wide pool.
// A default transport that is not a plain *http.Transport is a wrapper such
// as a tracer, and is used as-is so its instrumentation survives.
func newDefaultHTTPClient() *http.Client {
	transport, isPlain := http.DefaultTransport.(*http.Transport)
	if !isPlain {
		return http.DefaultClient
	}
	cloned := transport.Clone()
	cloned.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{Transport: cloned}
}
