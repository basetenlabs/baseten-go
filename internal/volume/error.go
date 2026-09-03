package volume

import (
	"errors"
	"fmt"
	"time"
)

// ErrorDomain scopes a [Error.Reason], so a reason constant here can never be
// confused with one from another Baseten service.
const ErrorDomain = "bdn.baseten.co"

// Reasons a caller has a reason to branch on. The registry is larger; these
// are the ones that mean something specific to a push or a pull. Reason is the
// stable field — a message is for humans and changes between releases.
const (
	// ReasonUploadSessionExpired means the push outlived its session. There is
	// no way to extend one, so the work has to start over.
	ReasonUploadSessionExpired = "UPLOAD_SESSION_EXPIRED"

	// ReasonCASConflict means someone else published while this push was
	// running: the view of the volume was stale.
	ReasonCASConflict = "CAS_CONFLICT"

	ReasonUnauthenticated  = "UNAUTHENTICATED"
	ReasonPermissionDenied = "PERMISSION_DENIED"
	ReasonNotFound         = "NOT_FOUND"
	ReasonAmbiguousPrefix  = "AMBIGUOUS_PREFIX"
	ReasonChunkTooLarge    = "CHUNK_TOO_LARGE"
	ReasonManifestTooLarge = "MANIFEST_TOO_LARGE"
	ReasonRateLimited      = "RATE_LIMITED"
	ReasonUnavailable      = "UNAVAILABLE"
)

// Error is a structured error response from the volume service.
//
// Branch on Reason, not on Message and not on HTTPStatus: several codes share
// a status, and the message is explicitly unstable.
type Error struct {
	// Code is the canonical gRPC code, the coarse error class.
	Code string

	// Reason is the stable constant naming what specifically went wrong.
	Reason string

	// Domain scopes Reason, and is always ErrorDomain for errors the service
	// produced. An empty domain means the response carried no structured
	// error and the fields below were derived from the HTTP status alone.
	Domain string

	// Message is for logs and people. It changes between releases.
	Message string

	// HTTPStatus is the status the response carried.
	HTTPStatus int

	// Metadata carries structured context, for example the expected and
	// current sequence numbers of a conflict.
	Metadata map[string]string

	// RetryDelay is the delay the server suggested, when it sent one. It is
	// reported for visibility; the retry policy follows the Retry-After header
	// instead.
	RetryDelay time.Duration
}

func (e *Error) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = e.Code
	}
	if e.Message == "" {
		return fmt.Sprintf("%s (HTTP %d)", reason, e.HTTPStatus)
	}
	return fmt.Sprintf("%s: %s (HTTP %d)", reason, e.Message, e.HTTPStatus)
}

// HasReason reports whether err is a service error with the given reason.
func HasReason(err error, reason string) bool {
	var e *Error
	return errors.As(err, &e) && e.Reason == reason
}
