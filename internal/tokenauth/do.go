package tokenauth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// Doer sends an HTTP request.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// A few resends cover a new token being invalidated in the same event as the
// old one. Past that, the refusal is returned, since tokens that keep getting
// refused point at something a new token cannot fix.
const maxResends = 2

// Do sends req through next with "Authorization: Bearer <token>", the token
// from get. When the server refuses the token (see [Refused]), Do calls get
// again with the refused token and sends the request again, at most twice,
// waiting resendDelay before the second resend. An empty token sends the
// request without Authorization, once.
//
// Revocation refuses every token issued before a cutoff that can fall a moment
// after the revoking event, so a token minted right after it can be refused
// too; resendDelay lets the next one be minted past the cutoff.
func Do(next Doer, req *http.Request, get func(ctx context.Context, refused string) (string, error), resendDelay time.Duration) (*http.Response, error) {
	// A body that cannot be rebuilt can be sent only once, so such a request
	// is never sent again.
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	var refused string
	for resend := 0; ; resend++ {
		token, err := get(req.Context(), refused)
		if err != nil {
			// Do closes the request body even on error, as http.Client does.
			if req.Body != nil {
				req.Body.Close()
			}
			return nil, err
		}
		attempt := req
		if token != "" {
			// Do must not modify the caller's request.
			attempt = req.Clone(req.Context())
			attempt.Header.Set("Authorization", "Bearer "+token)
		}
		if resend > 0 && req.GetBody != nil {
			if attempt.Body, err = req.GetBody(); err != nil {
				return nil, err
			}
		}
		resp, err := next.Do(attempt)
		if err != nil {
			return nil, err
		}
		if token == "" || !replayable || resend >= maxResends || !Refused(resp) {
			return resp, nil
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		refused = token
		if resend > 0 {
			timer := time.NewTimer(resendDelay)
			select {
			case <-req.Context().Done():
				timer.Stop()
				return nil, req.Context().Err()
			case <-timer.C:
			}
		}
	}
}

// Refused reports whether the server refused a request's token before its
// stated expiry, for example after it was revoked. The request was refused
// before any work was done, so sending it again with a new token is safe even
// when it has side effects.
func Refused(resp *http.Response) bool {
	if resp.StatusCode != http.StatusUnauthorized {
		return false
	}
	// Sandboxes, and the control plane for sandbox tokens, send this header.
	if resp.Header.Get("x-blaxel-error-code") == "TOKEN_REVOKED" {
		return true
	}
	// The RFC 6750 response to a refused access token.
	for _, challenge := range resp.Header.Values("WWW-Authenticate") {
		if strings.Contains(challenge, `error="invalid_token"`) {
			return true
		}
	}
	return false
}
