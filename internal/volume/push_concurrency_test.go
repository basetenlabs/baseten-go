package volume

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestUploadObservationDrivesAIMDAndLatencySampling(t *testing.T) {
	newGate := func() *adaptiveRequestGate {
		gate := newAdaptiveRequestGate(adaptiveGateConfig{
			min:                    1,
			max:                    8,
			initial:                4,
			decreaseFactor:         0.5,
			increaseAfterSuccesses: 1,
			now:                    func() time.Time { return time.Unix(1, 0) },
		})
		gate.gradient.baseline = time.Second.Seconds()
		return gate
	}
	run := func(
		t *testing.T,
		result UploadObjectResult,
		uploadErr error,
	) (*adaptiveRequestGate, bool, uint64, error) {
		t.Helper()
		gate := newGate()
		created, logicalBytes, err := newTestVolumeClient(t).uploadOne(
			t.Context(),
			ObjectUploaderFunc(func(
				_ context.Context,
				object UploadObject,
			) (UploadObjectResult, error) {
				_, _ = io.Copy(io.Discard, object.Body)
				return result, uploadErr
			}),
			&pushObject{data: []byte("payload")},
			gate,
			newByteGate(1<<20),
		)
		return gate, created, logicalBytes, err
	}
	assertGate := func(
		t *testing.T,
		gate *adaptiveRequestGate,
		wantLimit int,
		wantLatencySamples float64,
	) {
		t.Helper()
		gate.mu.Lock()
		defer gate.mu.Unlock()
		if gate.limit != wantLimit || gate.gradient.bucketCount != wantLatencySamples {
			t.Fatalf(
				"gate = limit %d latency samples %.0f, want %d and %.0f",
				gate.limit,
				gate.gradient.bucketCount,
				wantLimit,
				wantLatencySamples,
			)
		}
	}

	t.Run("hidden retry stall cuts without sampling", func(t *testing.T) {
		observation := &TransferObservation{RetryCount: 1, StallObserved: true}
		gate, created, _, err := run(
			t,
			UploadObjectResult{Created: true, Observation: observation},
			nil,
		)
		if err != nil || !created {
			t.Fatalf("upload = created %t, error %v", created, err)
		}
		assertGate(t, gate, 2, 0)
	})

	t.Run("retry without stall is neutral", func(t *testing.T) {
		observation := &TransferObservation{RetryCount: 1}
		gate, _, _, err := run(
			t,
			UploadObjectResult{Created: true, Observation: observation},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		assertGate(t, gate, 4, 0)
	})

	t.Run("clean transfer grows and samples", func(t *testing.T) {
		gate, created, _, err := run(t, UploadObjectResult{Created: true}, nil)
		if err != nil || !created {
			t.Fatalf("upload = created %t, error %v", created, err)
		}
		assertGate(t, gate, 5, 1)
	})

	t.Run("dedup is neutral", func(t *testing.T) {
		observation := &TransferObservation{StallObserved: true}
		gate, created, logicalBytes, err := run(
			t,
			UploadObjectResult{Observation: observation},
			nil,
		)
		if err != nil || created || logicalBytes != 0 {
			t.Fatalf(
				"dedup = created %t logical bytes %d error %v",
				created,
				logicalBytes,
				err,
			)
		}
		assertGate(t, gate, 4, 0)
	})

	t.Run("cancellation is neutral and redacted", func(t *testing.T) {
		observation := &TransferObservation{StallObserved: true}
		gate, _, _, err := run(
			t,
			UploadObjectResult{Observation: observation},
			fmt.Errorf("https://signed.invalid/object?credential=secret: %w", context.Canceled),
		)
		if err == nil || !IsCode(err, ErrorCanceled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled upload error = %v", err)
		}
		if rendered := fmt.Sprintf("%+v", err); strings.Contains(rendered, "secret") ||
			strings.Contains(rendered, "signed.invalid") {
			t.Fatalf("canceled upload leaked adapter details: %s", rendered)
		}
		assertGate(t, gate, 4, 0)
	})
}
