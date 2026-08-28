package volume

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAdaptiveRequestGateAppliesAIMDWithinBounds(t *testing.T) {
	now := time.Unix(1, 0)
	gate := newAdaptiveRequestGate(adaptiveGateConfig{
		min:                    2,
		max:                    8,
		initial:                4,
		decreaseFactor:         0.5,
		decreaseCooldown:       10 * time.Second,
		increaseAfterSuccesses: 1,
		now: func() time.Time {
			return now
		},
	})
	for _, want := range []int{5, 6, 7, 8, 8} {
		permit, err := gate.acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		permit.complete(transferSuccess)
		if got := gate.currentLimit(); got != want {
			t.Fatalf("limit after success = %d, want %d", got, want)
		}
	}

	permit, err := gate.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	permit.complete(transferStall)
	if got := gate.currentLimit(); got != 4 {
		t.Fatalf("limit after stall = %d, want 4", got)
	}
	permit, err = gate.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	permit.complete(transferStall)
	if got := gate.currentLimit(); got != 4 {
		t.Fatalf("limit during cooldown = %d, want 4", got)
	}
	now = now.Add(10 * time.Second)
	permit, err = gate.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	permit.complete(transferStall)
	if got := gate.currentLimit(); got != 2 {
		t.Fatalf("limit at floor = %d, want 2", got)
	}
	now = now.Add(10 * time.Second)
	permit, err = gate.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	permit.complete(transferStall)
	if got := gate.currentLimit(); got != 2 {
		t.Fatalf("limit below floor = %d, want 2", got)
	}
}

func TestDataPathRequestGatePostureCapsHighCoreStartup(t *testing.T) {
	tests := []struct {
		name        string
		maximum     int
		cores       int
		wantMinimum int
		wantInitial int
	}{
		{name: "single", maximum: 1, cores: 256, wantMinimum: 1, wantInitial: 1},
		{name: "below floor", maximum: 3, cores: 1, wantMinimum: 3, wantInitial: 3},
		{name: "minimum startup", maximum: 64, cores: 1, wantMinimum: 4, wantInitial: 8},
		{name: "scaled startup", maximum: 64, cores: 6, wantMinimum: 4, wantInitial: 12},
		{name: "high core cap", maximum: 512, cores: 256, wantMinimum: 4, wantInitial: 32},
		{name: "maximum below cap", maximum: 16, cores: 256, wantMinimum: 4, wantInitial: 16},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			minimum, initial := dataPathRequestGatePosture(test.maximum, test.cores)
			if minimum != test.wantMinimum || initial != test.wantInitial {
				t.Fatalf(
					"posture = minimum %d initial %d, want %d and %d",
					minimum,
					initial,
					test.wantMinimum,
					test.wantInitial,
				)
			}
		})
	}
}

func TestAdaptiveRequestGateBlocksAndCancelsWithoutLeakingPermit(t *testing.T) {
	gate := newAdaptiveRequestGate(adaptiveGateConfig{
		min: 1, max: 2, initial: 1, decreaseFactor: 0.5,
	})
	first, err := gate.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := gate.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire error = %v", err)
	}

	acquired := make(chan *requestPermit, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		permit, err := gate.acquire(t.Context())
		if err != nil {
			acquired <- nil
			return
		}
		acquired <- permit
	}()
	<-started
	select {
	case <-acquired:
		t.Fatal("request gate admitted work above its limit")
	default:
	}
	first.complete(transferNeutral)
	second := <-acquired
	if second == nil {
		t.Fatal("request gate waiter failed to acquire")
	}
	second.complete(transferNeutral)

	again, err := gate.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	again.complete(transferNeutral)
	if got := gate.currentLimit(); got != 1 {
		t.Fatalf("neutral completions changed limit to %d", got)
	}
}

func TestAdaptiveRequestGateCutsOnSustainedLatencyInflation(t *testing.T) {
	now := time.Unix(1, 0)
	gate := newAdaptiveRequestGate(adaptiveGateConfig{
		min:                    1,
		max:                    512,
		initial:                4,
		decreaseFactor:         0.5,
		decreaseCooldown:       time.Second,
		increaseAfterSuccesses: 1,
		now: func() time.Time {
			return now
		},
	})
	feed := func(count int, latency time.Duration) {
		t.Helper()
		for range count {
			permit, err := gate.acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(20 * time.Millisecond)
			permit.completeWithLatency(transferSuccess, latency)
		}
	}
	feed(200, 50*time.Millisecond)
	grown := gate.currentLimit()
	if grown < 100 {
		t.Fatalf("flat latency limit = %d, want fast additive growth", grown)
	}
	feed(300, 200*time.Millisecond)
	if got := gate.currentLimit(); got >= grown/2 {
		t.Fatalf("inflated latency limit = %d, want less than half of %d", got, grown)
	}
}

func TestByteGateCapsOutstandingBytesAndReleasesOnCancellation(t *testing.T) {
	gate := newByteGate(100)
	first, err := gate.acquire(t.Context(), 60)
	if err != nil {
		t.Fatal(err)
	}
	second, err := gate.acquire(t.Context(), 40)
	if err != nil {
		t.Fatal(err)
	}
	if got := gate.bytesInUse(); got != 100 {
		t.Fatalf("bytes in use = %d, want 100", got)
	}

	acquired := make(chan *bytePermit, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		permit, err := gate.acquire(t.Context(), 1)
		if err != nil {
			acquired <- nil
			return
		}
		acquired <- permit
	}()
	<-started
	select {
	case <-acquired:
		t.Fatal("byte gate admitted work above its cap")
	default:
	}
	first.release()
	third := <-acquired
	if third == nil {
		t.Fatal("byte gate waiter failed to acquire")
	}
	if got := gate.bytesInUse(); got != 41 {
		t.Fatalf("bytes in use after release = %d, want 41", got)
	}
	third.release()
	second.release()
	if got := gate.bytesInUse(); got != 0 {
		t.Fatalf("bytes in use after releases = %d, want 0", got)
	}

	if _, err := gate.acquire(t.Context(), 1_000); !errors.Is(
		err,
		errByteReservationExceedsCapacity,
	) {
		t.Fatalf("oversized reservation error = %v", err)
	}
	if got := gate.bytesInUse(); got != 0 {
		t.Fatalf("oversized reservation changed bytes in use to %d", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := gate.acquire(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled byte acquire error = %v", err)
	}
	if got := gate.bytesInUse(); got != 0 {
		t.Fatalf("canceled acquire leaked bytes: %d", got)
	}
}

func TestClassifyTransferOutcome(t *testing.T) {
	tests := []struct {
		err         error
		observation TransferObservation
		transferred bool
		want        transferOutcome
	}{
		{transferred: true, want: transferSuccess},
		{transferred: false, want: transferNeutral},
		{
			observation: TransferObservation{RetryCount: 1},
			transferred: true,
			want:        transferNeutral,
		},
		{
			observation: TransferObservation{RetryCount: 1, StallObserved: true},
			transferred: true,
			want:        transferStall,
		},
		{
			observation: TransferObservation{StallObserved: true},
			transferred: false,
			want:        transferNeutral,
		},
		{
			err:         context.Canceled,
			observation: TransferObservation{StallObserved: true},
			transferred: true,
			want:        transferNeutral,
		},
		{
			err:         &Error{Code: ErrorIntegrity},
			observation: TransferObservation{StallObserved: true},
			transferred: true,
			want:        transferNeutral,
		},
		{err: &Error{Code: ErrorTransfer}, want: transferStall},
		{
			err:         &Error{Code: ErrorTransfer},
			observation: TransferObservation{RetryCount: 1},
			want:        transferNeutral,
		},
		{
			err:         &Error{Code: ErrorTransfer},
			observation: TransferObservation{RetryCount: 1, StallObserved: true},
			want:        transferStall,
		},
		{err: errors.New("opaque caller error"), want: transferNeutral},
	}
	for _, test := range tests {
		if got := classifyTransferOutcome(
			test.err,
			test.observation,
			test.transferred,
		); got != test.want {
			t.Fatalf("classifyTransferOutcome(%v) = %d, want %d", test.err, got, test.want)
		}
	}
}
