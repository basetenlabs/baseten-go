package client

// VolumeErrorReason is the stable constant naming what specifically went
// wrong with a volume operation. It mirrors the service's own reason strings,
// so a reason the service adds tomorrow flows through a [VolumeError] without
// new API here — the constants below are the ones a caller has a reason to
// branch on, not the whole registry.
type VolumeErrorReason string

const (
	// VolumeErrorReasonUploadSessionExpired means the push outlived its
	// session. There is no way to extend one, so the work has to start over.
	VolumeErrorReasonUploadSessionExpired VolumeErrorReason = "UPLOAD_SESSION_EXPIRED"

	// VolumeErrorReasonCASConflict means someone else published while this
	// push was running: the view of the volume was stale.
	VolumeErrorReasonCASConflict VolumeErrorReason = "CAS_CONFLICT"

	VolumeErrorReasonNotFound           VolumeErrorReason = "NOT_FOUND"
	VolumeErrorReasonPermissionDenied   VolumeErrorReason = "PERMISSION_DENIED"
	VolumeErrorReasonAmbiguousPrefix    VolumeErrorReason = "AMBIGUOUS_PREFIX"
	VolumeErrorReasonRateLimited        VolumeErrorReason = "RATE_LIMITED"
	VolumeErrorReasonChunkTooLarge      VolumeErrorReason = "CHUNK_TOO_LARGE"
	VolumeErrorReasonManifestTooLarge   VolumeErrorReason = "MANIFEST_TOO_LARGE"
	VolumeErrorReasonServiceUnavailable VolumeErrorReason = "UNAVAILABLE"
)

// VolumeError is what a volume operation returns when the volume service
// refused or failed it. Match with errors.As:
//
//	var ve *client.VolumeError
//	if errors.As(err, &ve) && ve.Reason == client.VolumeErrorReasonCASConflict { ... }
//
// Errors that are not the service's own — a local filesystem failure, a
// cancelled context, a malformed response — come back as themselves,
// wrapped with context, not as a VolumeError with an invented reason.
type VolumeError struct {
	// Reason is the stable constant to branch on. A message is for humans
	// and changes between releases.
	Reason VolumeErrorReason

	// Message is the service's human-readable description.
	Message string

	// Err is the underlying error, when there is one.
	Err error
}

func (e *VolumeError) Error() string {
	if e.Message != "" {
		return string(e.Reason) + ": " + e.Message
	}
	return string(e.Reason)
}

func (e *VolumeError) Unwrap() error {
	return e.Err
}
