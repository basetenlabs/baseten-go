package volume

// Reason is the stable constant naming what specifically went wrong with a
// volume operation. It mirrors the service's own reason strings, so a reason
// the service adds tomorrow flows through an [Error] without new API here —
// the constants below are the ones a caller has a reason to branch on, not
// the whole registry.
type Reason string

const (
	// ReasonUploadSessionExpired means the push outlived its session. There
	// is no way to extend one, so the work has to start over.
	ReasonUploadSessionExpired Reason = "UPLOAD_SESSION_EXPIRED"

	// ReasonCASConflict means someone else published while this push was
	// running: the view of the volume was stale.
	ReasonCASConflict Reason = "CAS_CONFLICT"

	ReasonNotFound           Reason = "NOT_FOUND"
	ReasonPermissionDenied   Reason = "PERMISSION_DENIED"
	ReasonAmbiguousPrefix    Reason = "AMBIGUOUS_PREFIX"
	ReasonRateLimited        Reason = "RATE_LIMITED"
	ReasonChunkTooLarge      Reason = "CHUNK_TOO_LARGE"
	ReasonManifestTooLarge   Reason = "MANIFEST_TOO_LARGE"
	ReasonServiceUnavailable Reason = "UNAVAILABLE"
)

// Error is what a volume operation returns when the volume service refused
// or failed it. Match with errors.As:
//
//	var ve *volume.Error
//	if errors.As(err, &ve) && ve.Reason == volume.ReasonCASConflict { ... }
//
// Errors that are not the service's own — a local filesystem failure, a
// cancelled context, a malformed response — come back as themselves,
// wrapped with context, not as an Error with an invented reason.
type Error struct {
	// Reason is the stable constant to branch on. A message is for humans
	// and changes between releases.
	Reason Reason

	// Message is the service's human-readable description.
	Message string

	// Err is the underlying error, when there is one.
	Err error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return string(e.Reason) + ": " + e.Message
	}
	return string(e.Reason)
}

func (e *Error) Unwrap() error {
	return e.Err
}
