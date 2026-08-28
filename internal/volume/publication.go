package volume

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// PublishOutcome is the upload session's authoritative publication state.
type PublishOutcome string

const (
	// PublishOutcomePublished means the manifest is definitely published.
	PublishOutcomePublished PublishOutcome = "published"
	// PublishOutcomeNotPublished means publication definitely did not happen,
	// so retrying the same idempotent publication is safe.
	PublishOutcomeNotPublished PublishOutcome = "not_published"
	// PublishOutcomeUnknown means publication may have happened. Callers must
	// reconcile the session before retrying or discarding local result state.
	PublishOutcomeUnknown PublishOutcome = "unknown"
)

// PublishFailureReason is a safe, storage-neutral failure classification.
type PublishFailureReason string

const (
	PublishFailureReasonUnspecified      PublishFailureReason = "unspecified"
	PublishFailureReasonTransportFailure PublishFailureReason = "transport_failure"
	PublishFailureReasonRejected         PublishFailureReason = "rejected"
	PublishFailureReasonCanceled         PublishFailureReason = "canceled"
	PublishFailureReasonInvalidResult    PublishFailureReason = "invalid_result"
)

// PublishResult is domain-neutral publication state returned by an upload
// session.
//
// A successful direct publication returns Published with a nil error. A
// successful reconciliation also returns Published with a nil error and sets
// Reconciled; Sequence and PointerChanged may remain nil when reconciliation
// can confirm publication but cannot recover those optional fields.
//
// A failed call returns NotPublished or Unknown together with a non-nil error
// and a safe FailureReason. The engine discards arbitrary transport details and
// retains only canonical context errors and AdapterError classifications.
// Outcome, rather than the raw error, determines whether retry is safe.
type PublishResult struct {
	Outcome        PublishOutcome
	FailureReason  PublishFailureReason
	Sequence       *uint64
	PointerChanged *bool
	Reconciled     bool
}

// PushPublicationError reports a push whose publication did not produce an
// unambiguous clean success. Result is non-nil exactly when publication may
// have happened. On an Unknown outcome, preserve Result and reconcile the
// upload session; retry only when RetrySafe reports true.
type PushPublicationError struct {
	Result      *PushResult
	Publication PublishResult

	cause error
}

func (e *PushPublicationError) Error() string {
	if e == nil || e.cause == nil {
		return string(ErrorPublicationFailed)
	}
	return e.cause.Error()
}

// Format prevents result data and discarded adapter causes from appearing in
// alternate error formatting.
func (e *PushPublicationError) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, e.Error())
}

func (e *PushPublicationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// RetrySafe reports whether the adapter proved publication did not happen.
func (e *PushPublicationError) RetrySafe() bool {
	return e != nil && e.Publication.Outcome == PublishOutcomeNotPublished
}

// PublicationMayHaveHappened reports whether retry requires reconciliation.
func (e *PushPublicationError) PublicationMayHaveHappened() bool {
	if e == nil {
		return false
	}
	return e.Publication.Outcome != PublishOutcomeNotPublished
}

func normalizePublishResult(
	result PublishResult,
	publishErr error,
) (PublishResult, bool) {
	invalid := false
	contradictoryFailure := false
	switch result.Outcome {
	case PublishOutcomePublished, PublishOutcomeNotPublished, PublishOutcomeUnknown:
	default:
		result.Outcome = PublishOutcomeUnknown
		invalid = true
	}

	switch result.FailureReason {
	case "",
		PublishFailureReasonUnspecified,
		PublishFailureReasonTransportFailure,
		PublishFailureReasonRejected,
		PublishFailureReasonCanceled,
		PublishFailureReasonInvalidResult:
	default:
		result.FailureReason = PublishFailureReasonInvalidResult
		invalid = true
	}

	if result.Outcome != PublishOutcomePublished {
		result.Sequence = nil
		result.PointerChanged = nil
		if result.Reconciled {
			result.Reconciled = false
			invalid = true
			contradictoryFailure = true
		}
	}

	contextCause := canonicalContextCause(publishErr)
	switch result.Outcome {
	case PublishOutcomePublished:
		if publishErr != nil || result.FailureReason != "" {
			invalid = true
		}
		if invalid {
			result.FailureReason = PublishFailureReasonInvalidResult
		} else {
			result.FailureReason = ""
		}
	case PublishOutcomeNotPublished, PublishOutcomeUnknown:
		if publishErr == nil {
			invalid = true
			contradictoryFailure = true
		}
		if contradictoryFailure && result.Outcome == PublishOutcomeNotPublished {
			result.Outcome = PublishOutcomeUnknown
		}
		switch {
		case invalid:
			result.FailureReason = PublishFailureReasonInvalidResult
		case contextCause != nil || adapterErrorIsCancellation(publishErr):
			result.FailureReason = PublishFailureReasonCanceled
		case result.FailureReason == "":
			result.FailureReason = PublishFailureReasonUnspecified
		}
	}
	return result, invalid
}

func newPushPublicationError(
	publication PublishResult,
	publishErr error,
) *PushPublicationError {
	code := ErrorPublicationFailed
	message := "upload session confirmed that the volume was not published"
	switch publication.Outcome {
	case PublishOutcomeUnknown:
		code = ErrorPublicationUnknown
		message = "upload session could not determine whether the volume was published"
	case PublishOutcomePublished:
		message = "upload session returned an invalid error with a published result"
	}
	classification := classifyBoundaryError(publishErr)
	return &PushPublicationError{
		Publication: publication,
		cause: &Error{
			Code:      code,
			Operation: "publish volume",
			Message:   message,
			cause:     classification.contextCause,
			adapter:   classification.adapter,
		},
	}
}

func canonicalContextCause(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		return context.Canceled
	default:
		return nil
	}
}
