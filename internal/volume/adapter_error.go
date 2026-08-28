package volume

import (
	"errors"
	"fmt"
	"io"
)

// AdapterErrorKind is a storage-neutral boundary failure category.
type AdapterErrorKind string

const (
	AdapterErrorKindUnknown      AdapterErrorKind = "unknown"
	AdapterErrorKindCredentials  AdapterErrorKind = "credentials"
	AdapterErrorKindThrottling   AdapterErrorKind = "throttling"
	AdapterErrorKindUnavailable  AdapterErrorKind = "unavailable"
	AdapterErrorKindIntegrity    AdapterErrorKind = "integrity"
	AdapterErrorKindCancellation AdapterErrorKind = "cancellation"
)

// AdapterErrorReason is a stable, detail-safe boundary failure reason.
type AdapterErrorReason string

const (
	AdapterErrorReasonUnspecified        AdapterErrorReason = "unspecified"
	AdapterErrorReasonExpiredCredentials AdapterErrorReason = "expired_credentials"
	AdapterErrorReasonThrottled          AdapterErrorReason = "throttled"
	AdapterErrorReasonUnavailable        AdapterErrorReason = "unavailable"
	AdapterErrorReasonIntegrity          AdapterErrorReason = "integrity"
	AdapterErrorReasonCanceled           AdapterErrorReason = "canceled"
)

// AdapterError is the only caller-supplied error state retained across object
// reader, object uploader, and upload session boundaries. Kind and Reason are
// normalized to the constants above. Adapters must classify failures at their
// boundary; raw transport, endpoint, credential, and storage errors are never
// retained by the engine.
type AdapterError struct {
	Kind          AdapterErrorKind
	Reason        AdapterErrorReason
	Retryable     bool
	StallObserved bool
}

func (e *AdapterError) Error() string {
	if e == nil {
		return "<nil>"
	}
	normalized := normalizeAdapterError(e)
	return "adapter " + string(normalized.Kind) + " failure: " + string(normalized.Reason)
}

// Format prevents malformed caller-supplied labels from appearing in alternate
// formatting.
func (e *AdapterError) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, e.Error())
}

func normalizeAdapterError(reported *AdapterError) *AdapterError {
	if reported == nil {
		return nil
	}
	normalized := &AdapterError{
		Kind:          reported.Kind,
		Reason:        reported.Reason,
		Retryable:     reported.Retryable,
		StallObserved: reported.StallObserved,
	}
	switch normalized.Kind {
	case AdapterErrorKindCredentials,
		AdapterErrorKindThrottling,
		AdapterErrorKindUnavailable,
		AdapterErrorKindIntegrity,
		AdapterErrorKindCancellation:
	default:
		normalized.Kind = AdapterErrorKindUnknown
	}
	switch normalized.Reason {
	case AdapterErrorReasonExpiredCredentials,
		AdapterErrorReasonThrottled,
		AdapterErrorReasonUnavailable,
		AdapterErrorReasonIntegrity,
		AdapterErrorReasonCanceled:
	default:
		normalized.Reason = AdapterErrorReasonUnspecified
	}
	return normalized
}

func adapterErrorFrom(err error) *AdapterError {
	var reported *AdapterError
	if !errors.As(err, &reported) {
		return nil
	}
	return normalizeAdapterError(reported)
}

type boundaryErrorClassification struct {
	contextCause error
	adapter      *AdapterError
}

func classifyBoundaryError(err error) boundaryErrorClassification {
	return boundaryErrorClassification{
		contextCause: canonicalContextCause(err),
		adapter:      adapterErrorFrom(err),
	}
}

func adapterErrorIsCancellation(err error) bool {
	classification := adapterErrorFrom(err)
	return classification != nil && classification.Kind == AdapterErrorKindCancellation
}
