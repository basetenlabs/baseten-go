package volume

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type maliciousMarkedAdapterCause struct {
	message string
	cause   error
	is      error
}

func (cause *maliciousMarkedAdapterCause) Error() string {
	return cause.message
}

func (*maliciousMarkedAdapterCause) SafeForAdapterError() {}

func (cause *maliciousMarkedAdapterCause) Unwrap() error {
	return cause.cause
}

func (cause *maliciousMarkedAdapterCause) Is(target error) bool {
	return cause.is != nil && target == cause.is
}

type classifiedBoundaryFixture struct {
	boundaryErr    error
	discarded      []error
	classification *AdapterError
}

func newClassifiedBoundaryError() classifiedBoundaryFixture {
	rawCause := errors.New("raw signed URL credential")
	nestedRawCause := errors.New("nested S3 authorization failure")
	classification := &AdapterError{
		Kind:          AdapterErrorKindThrottling,
		Reason:        AdapterErrorReasonThrottled,
		Retryable:     true,
		StallObserved: true,
	}
	maliciousCause := &maliciousMarkedAdapterCause{
		message: "malicious safe marker",
		cause: errors.Join(
			fmt.Errorf("https://storage.invalid/private: %w", nestedRawCause),
			fmt.Errorf("nested classification: %w", classification),
		),
	}
	return classifiedBoundaryFixture{
		boundaryErr: fmt.Errorf(
			"https://signed.invalid/object?credential=secret: %w",
			errors.Join(rawCause, maliciousCause),
		),
		discarded:      []error{rawCause, nestedRawCause, maliciousCause},
		classification: classification,
	}
}

func assertClassifiedBoundaryError(
	t *testing.T,
	err error,
	discarded []error,
	original *AdapterError,
) {
	t.Helper()
	if err == nil {
		t.Fatal("boundary error is nil")
	}
	var classification *AdapterError
	if !errors.As(err, &classification) {
		t.Fatalf("boundary error has no AdapterError: %v", err)
	}
	if classification == original {
		t.Fatal("boundary retained caller-owned AdapterError")
	}
	var repeatedClassification *AdapterError
	if !errors.As(err, &repeatedClassification) || repeatedClassification == classification {
		t.Fatal("boundary did not return an independent AdapterError classification")
	}
	if classification.Kind != AdapterErrorKindThrottling ||
		classification.Reason != AdapterErrorReasonThrottled ||
		!classification.Retryable ||
		!classification.StallObserved {
		t.Fatalf("adapter classification = %+v", classification)
	}
	for _, cause := range discarded {
		if errors.Is(err, cause) {
			t.Fatalf("boundary error retained discarded cause %T: %v", cause, err)
		}
	}
	if errors.Is(err, original) {
		t.Fatal("boundary retained caller-owned AdapterError in unwrap chain")
	}
	var retainedMarker *maliciousMarkedAdapterCause
	if errors.As(err, &retainedMarker) {
		t.Fatalf("boundary error retained malicious marker: %v", err)
	}
	nested := fmt.Errorf("nested boundary: %w", err)
	formatted := []error{
		err,
		classification,
		nested,
		errors.Join(err, errors.New("stable package-owned detail")),
	}
	for _, value := range formatted {
		for _, format := range []string{"%s", "%q", "%v", "%+v", "%#v"} {
			rendered := fmt.Sprintf(format, value)
			for _, forbidden := range []string{
				"signed.invalid",
				"credential=secret",
				"raw signed URL credential",
				"storage.invalid",
				"nested S3 authorization failure",
				"malicious safe marker",
			} {
				if strings.Contains(rendered, forbidden) {
					t.Fatalf("%T %s formatting leaked %q: %s", value, format, forbidden, rendered)
				}
			}
		}
	}
	leaves := errorLeaves(err)
	if len(leaves) != 1 {
		t.Fatalf("boundary unwrap leaves = %v, want one terminal Error", leaves)
	}
	terminal, ok := leaves[0].(*Error)
	if !ok || errors.Unwrap(terminal) != nil {
		t.Fatalf("boundary unwrap leaf = %T %v, want terminal Error", leaves[0], leaves[0])
	}
}

func errorLeaves(err error) []error {
	if err == nil {
		return nil
	}
	switch current := err.(type) {
	case interface{ Unwrap() []error }:
		var leaves []error
		for _, child := range current.Unwrap() {
			leaves = append(leaves, errorLeaves(child)...)
		}
		return leaves
	case interface{ Unwrap() error }:
		if child := current.Unwrap(); child != nil {
			return errorLeaves(child)
		}
	}
	return []error{err}
}

func assertCanonicalContextLeaves(
	t *testing.T,
	err error,
	canonical error,
) {
	t.Helper()
	leaves := errorLeaves(err)
	if len(leaves) != 1 || leaves[0] != canonical {
		t.Fatalf("context boundary unwrap leaves = %v, want only %v", leaves, canonical)
	}
}

func TestAdapterBoundaryCanonicalizesContextWithoutRetainingMarker(t *testing.T) {
	for _, canonical := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(canonical.Error(), func(t *testing.T) {
			rawCause := errors.New("signed request failed with secret")
			original := &AdapterError{
				Kind:      AdapterErrorKindCancellation,
				Reason:    AdapterErrorReasonCanceled,
				Retryable: true,
			}
			maliciousCause := &maliciousMarkedAdapterCause{
				message: "context impersonator with credential",
				cause:   errors.Join(rawCause, original),
				is:      canonical,
			}
			normalized := transferError("adapter call", maliciousCause)
			if !IsCode(normalized, ErrorCanceled) || !errors.Is(normalized, canonical) {
				t.Fatalf("canonical context classification = %v", normalized)
			}
			if errors.Is(normalized, rawCause) || errors.Is(normalized, maliciousCause) {
				t.Fatalf("canonical context retained adapter causes: %v", normalized)
			}
			var classification *AdapterError
			if !errors.As(normalized, &classification) {
				t.Fatalf("canonical context lost AdapterError: %v", normalized)
			}
			if classification == original {
				t.Fatal("canonical context retained caller-owned AdapterError")
			}
			assertCanonicalContextLeaves(t, normalized, canonical)
		})
	}
}

func TestAdapterErrorHasNoCauseUnwrapPath(t *testing.T) {
	classification := &AdapterError{
		Kind:          AdapterErrorKindUnavailable,
		Reason:        AdapterErrorReasonUnavailable,
		Retryable:     true,
		StallObserved: true,
	}
	if _, ok := any(classification).(interface{ Unwrap() error }); ok {
		t.Fatal("AdapterError unexpectedly exposes a cause unwrap path")
	}
	if _, ok := any(classification).(interface{ Unwrap() []error }); ok {
		t.Fatal("AdapterError unexpectedly exposes a multi-cause unwrap path")
	}
	rawCause := errors.New("raw credential body")
	maliciousCause := &maliciousMarkedAdapterCause{
		message: "marked signed URL failure",
		cause:   fmt.Errorf("nested transport detail: %w", rawCause),
	}
	normalized := transferError("adapter call", maliciousCause)
	if unwrapped := errors.Unwrap(normalized); unwrapped != nil {
		t.Fatalf("unclassified boundary unwrap = %v, want nil", unwrapped)
	}
	if errors.Is(normalized, rawCause) || errors.Is(normalized, maliciousCause) {
		t.Fatalf("unclassified boundary retained adapter causes: %v", normalized)
	}
	var retainedMarker *maliciousMarkedAdapterCause
	if errors.As(normalized, &retainedMarker) {
		t.Fatalf("unclassified boundary retained malicious marker: %v", normalized)
	}
	for _, format := range []string{"%s", "%q", "%v", "%+v", "%#v"} {
		rendered := fmt.Sprintf(
			format,
			errors.Join(classification, normalized),
		)
		for _, forbidden := range []string{
			"http://",
			"https://",
			"credential",
			"signed URL",
			"transport detail",
		} {
			if strings.Contains(rendered, forbidden) {
				t.Fatalf("%s AdapterError formatting leaked %q: %s", format, forbidden, rendered)
			}
		}
	}
}

func TestAdapterErrorNormalizesUnsafeLabelsAndClassifiesCancellation(t *testing.T) {
	malformed := &AdapterError{
		Kind:   AdapterErrorKind("https://signed.invalid"),
		Reason: AdapterErrorReason("credential=secret"),
	}
	if rendered := fmt.Sprintf("%#v", malformed); strings.Contains(rendered, "signed.invalid") ||
		strings.Contains(rendered, "credential=secret") {
		t.Fatalf("malformed classification leaked through formatting: %s", rendered)
	}

	normalized := transferError("adapter call", &AdapterError{
		Kind:   AdapterErrorKindCancellation,
		Reason: AdapterErrorReasonCanceled,
	})
	if !IsCode(normalized, ErrorCanceled) ||
		errors.Is(normalized, context.Canceled) {
		t.Fatalf("adapter cancellation = %v", normalized)
	}
}

func TestAdapterErrorStableClassificationsRemainDistinct(t *testing.T) {
	tests := []struct {
		kind   AdapterErrorKind
		reason AdapterErrorReason
	}{
		{AdapterErrorKindCredentials, AdapterErrorReasonExpiredCredentials},
		{AdapterErrorKindThrottling, AdapterErrorReasonThrottled},
		{AdapterErrorKindUnavailable, AdapterErrorReasonUnavailable},
		{AdapterErrorKindIntegrity, AdapterErrorReasonIntegrity},
		{AdapterErrorKindCancellation, AdapterErrorReasonCanceled},
	}
	for _, test := range tests {
		t.Run(string(test.reason), func(t *testing.T) {
			normalized := transferError("adapter call", &AdapterError{
				Kind:      test.kind,
				Reason:    test.reason,
				Retryable: true,
			})
			var classification *AdapterError
			if !errors.As(normalized, &classification) {
				t.Fatalf("classification missing from %v", normalized)
			}
			if classification.Kind != test.kind ||
				classification.Reason != test.reason ||
				!classification.Retryable {
				t.Fatalf("classification = %+v", classification)
			}
		})
	}
}
