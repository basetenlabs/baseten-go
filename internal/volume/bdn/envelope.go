package bdn

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/basetenlabs/baseten-go/internal/volume"
)

// errorEnvelope is the wire shape of every error the service produces.
type errorEnvelope struct {
	Error struct {
		Code     string            `json:"code"`
		Message  string            `json:"message"`
		Reason   string            `json:"reason"`
		Domain   string            `json:"domain"`
		Metadata map[string]string `json:"metadata"`
		Details  []errorDetail     `json:"details"`
	} `json:"error"`
}

// errorDetail is one typed detail. Unrecognized types are ignored: the catalog
// is allowed to grow, and a client that failed on an unfamiliar entry would
// break on a server that added one.
type errorDetail struct {
	Type         string `json:"type"`
	RetryDelayMS int64  `json:"retry_delay_ms"`
}

// decodeError turns a failed response into a [volume.Error].
//
// A response without the envelope — a proxy's bare 502, say — still produces
// an error, with the fields derived from the status. That derivation is lossy
// in a way the envelope is not, since several codes share a status, so it is
// used only when there is nothing better.
func decodeError(resp *rawResponse) error {
	err := &volume.Error{HTTPStatus: resp.status}

	var envelope errorEnvelope
	if jsonErr := json.Unmarshal(resp.body, &envelope); jsonErr == nil && envelope.Error.Code != "" {
		err.Code = envelope.Error.Code
		err.Message = envelope.Error.Message
		err.Reason = envelope.Error.Reason
		err.Domain = envelope.Error.Domain
		err.Metadata = envelope.Error.Metadata
		for _, detail := range envelope.Error.Details {
			if detail.Type == "RetryInfo" && detail.RetryDelayMS > 0 {
				err.RetryDelay = time.Duration(detail.RetryDelayMS) * time.Millisecond
			}
		}
		return err
	}

	err.Code = codeForStatus(resp.status)
	err.Reason = err.Code
	err.Message = http.StatusText(resp.status)
	return err
}

// codeForStatus maps an HTTP status back to a canonical code, for responses
// that carried no envelope.
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return "UNIMPLEMENTED"
	case http.StatusConflict:
		return "ABORTED"
	case http.StatusGone:
		return "FAILED_PRECONDITION"
	case http.StatusRequestEntityTooLarge:
		return "OUT_OF_RANGE"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	case http.StatusGatewayTimeout:
		return "DEADLINE_EXCEEDED"
	default:
		if status >= 500 {
			return "INTERNAL"
		}
		return "UNKNOWN"
	}
}
