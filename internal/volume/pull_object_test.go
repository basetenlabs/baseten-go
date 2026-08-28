//go:build linux || darwin

package volume

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestObjectReaderErrorsAreRedacted(t *testing.T) {
	fixture := newPullFixture(t)
	reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
		return Object{}, errors.New("https://signed.invalid/object?token=temporary-secret")
	})
	options := fixture.options(filepath.Join(t.TempDir(), "output"))
	options.Objects = reader
	_, err := fixture.client.Pull(t.Context(), options)
	if err == nil || !IsCode(err, ErrorTransfer) {
		t.Fatalf("pull error = %v, want %s", err, ErrorTransfer)
	}
	for _, forbidden := range []string{"signed.invalid", "temporary-secret", "/object"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("error leaked %q: %s", forbidden, err)
		}
	}
}

func TestObjectReaderWrappedCancellationIsCanonicalAndRedacted(t *testing.T) {
	fixture := newPullFixture(t)
	reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
		return Object{}, fmt.Errorf(
			"https://signed.invalid/object?credential=secret: %w",
			context.Canceled,
		)
	})
	options := fixture.options(filepath.Join(t.TempDir(), "output"))
	options.Objects = reader
	_, err := fixture.client.Pull(t.Context(), options)
	if err == nil ||
		!IsCode(err, ErrorCanceled) ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("wrapped reader cancellation = %v", err)
	}
	if unwrapped := errors.Unwrap(err); unwrapped != context.Canceled {
		t.Fatalf("unwrapped reader error = %v, want context.Canceled", unwrapped)
	}
	rendered := fmt.Sprintf("%+v", err)
	for _, forbidden := range []string{"signed.invalid", "credential", "secret", "/object"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("wrapped cancellation leaked %q: %s", forbidden, rendered)
		}
	}
}

func TestObjectReaderObservationIsSnapshottedAfterBodyClose(t *testing.T) {
	content := []byte("observed transfer")
	digest := testFixtureDigest(content)
	observation := &TransferObservation{}
	reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
		return Object{
			Body: &observationOnCloseBody{
				Reader:      bytes.NewReader(content),
				observation: observation,
			},
			Size:        int64(len(content)),
			Kind:        ObjectKindChunk,
			Encoding:    ObjectEncodingIdentity,
			Observation: observation,
		}, nil
	})
	length := uint64(len(content))
	body, err := newTestVolumeClient(t).readVerifiedObject(
		t.Context(),
		reader,
		digest,
		expectedObject{
			kind:              ObjectKindChunk,
			maxDecodedBytes:   length,
			exactDecodedBytes: &length,
		},
		newByteGate(2<<20),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer body.release()
	if body.observation.RetryCount != 1 || !body.observation.StallObserved {
		t.Fatalf("snapshotted observation = %+v", body.observation)
	}
}

func TestVerifiedObjectReservesWorstCaseMemoryBeforeRead(t *testing.T) {
	content := []byte("budgeted")
	digest := testFixtureDigest(content)
	expected := expectedObject{
		kind:              ObjectKindChunk,
		maxDecodedBytes:   uint64(len(content)),
		exactDecodedBytes: totalPointer(uint64(len(content))),
	}
	limits := decodeLimits(expected.maxDecodedBytes)
	reservation, overflow := addUint64(expected.maxDecodedBytes, limits.MaxMemoryBytes)
	if overflow {
		t.Fatal("test reservation overflowed")
	}
	budget := newByteGate(reservation)
	held, err := budget.acquire(t.Context(), reservation)
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var waitOnce sync.Once
	budget.waitHook = func() {
		waitOnce.Do(func() { close(waiting) })
	}
	readCalled := make(chan struct{})
	reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
		close(readCalled)
		return Object{
			Body:     io.NopCloser(bytes.NewReader(content)),
			Size:     int64(len(content)),
			Kind:     ObjectKindChunk,
			Encoding: ObjectEncodingIdentity,
		}, nil
	})
	type readResult struct {
		body *verifiedObjectBody
		err  error
	}
	done := make(chan readResult, 1)
	client := newTestVolumeClient(t)
	go func() {
		body, err := client.readVerifiedObject(
			t.Context(),
			reader,
			digest,
			expected,
			budget,
		)
		done <- readResult{body: body, err: err}
	}()
	<-waiting
	select {
	case <-readCalled:
		t.Fatal("ObjectReader.ReadObject ran before worst-case memory was available")
	default:
	}
	held.release()
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	result.body.release()
	if got := budget.bytesInUse(); got != 0 {
		t.Fatalf("object read leaked %d budget bytes", got)
	}
}

func TestVerifiedObjectBudgetCancellationDoesNotReadOrLeak(t *testing.T) {
	expected := expectedObject{kind: ObjectKindChunk, maxDecodedBytes: 1}
	limits := decodeLimits(expected.maxDecodedBytes)
	reservation, overflow := addUint64(expected.maxDecodedBytes, limits.MaxMemoryBytes)
	if overflow {
		t.Fatal("test reservation overflowed")
	}
	budget := newByteGate(reservation)
	held, err := budget.acquire(t.Context(), reservation)
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var waitOnce sync.Once
	budget.waitHook = func() {
		waitOnce.Do(func() { close(waiting) })
	}
	readCalled := false
	reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
		readCalled = true
		return Object{}, errors.New("unexpected read")
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	client := newTestVolumeClient(t)
	go func() {
		_, err := client.readVerifiedObject(
			ctx,
			reader,
			testDigest(0x72),
			expected,
			budget,
		)
		done <- err
	}()
	<-waiting
	cancel()
	err = <-done
	if err == nil || !IsCode(err, ErrorCanceled) || readCalled {
		t.Fatalf("budget cancellation = read %t, error %v", readCalled, err)
	}
	if got := budget.bytesInUse(); got != reservation {
		t.Fatalf("canceled waiter changed budget usage to %d, want %d", got, reservation)
	}
	held.release()
	if got := budget.bytesInUse(); got != 0 {
		t.Fatalf("budget cancellation leaked %d bytes", got)
	}
}

func TestObjectReaderErrorBodyIsClosedAndRedacted(t *testing.T) {
	observation := &TransferObservation{}
	body := &errorObjectBody{
		observation: observation,
		closeErr: fmt.Errorf(
			"https://signed.invalid/close?credential=secret: %w",
			context.Canceled,
		),
	}
	reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
		return Object{Body: body, Observation: observation}, fmt.Errorf(
			"https://signed.invalid/read?credential=secret: %w",
			context.Canceled,
		)
	})
	budget := newByteGate(2 << 20)
	_, attempt, err := newTestVolumeClient(t).readVerifiedObjectObserved(
		t.Context(),
		reader,
		testDigest(0x71),
		expectedObject{kind: ObjectKindChunk, maxDecodedBytes: 1},
		budget,
	)
	if err == nil ||
		!IsCode(err, ErrorCanceled) ||
		!errors.Is(err, context.Canceled) ||
		!body.closed {
		t.Fatalf("error body result = closed %t, error %v", body.closed, err)
	}
	if attempt.observation.RetryCount != 1 {
		t.Fatalf("close-time observation = %+v", attempt.observation)
	}
	if got := budget.bytesInUse(); got != 0 {
		t.Fatalf("error body leaked %d budget bytes", got)
	}
	rendered := fmt.Sprintf("%+v", err)
	for _, forbidden := range []string{"signed.invalid", "credential", "secret", "/read", "/close"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("reader error leaked %q: %s", forbidden, rendered)
		}
	}
}

func TestPullTransferLatencyExcludesDecoderAndCallerWork(t *testing.T) {
	content := []byte("decoded")
	digest := testFixtureDigest(content)
	decoderBlocked := make(chan struct{})
	releaseDecoder := make(chan struct{})
	client := newTestVolumeClient(t)
	client.decoder = DecoderFunc(func(
		_ context.Context,
		dst io.Writer,
		src io.Reader,
		_ DecodeLimits,
	) error {
		encoded, err := io.ReadAll(src)
		if err != nil {
			return err
		}
		close(decoderBlocked)
		<-releaseDecoder
		_, err = dst.Write(encoded)
		return err
	})
	reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
		return Object{
			Body:     io.NopCloser(bytes.NewReader(content)),
			Size:     int64(len(content)),
			Kind:     ObjectKindChunk,
			Encoding: ObjectEncodingZstd,
		}, nil
	})
	type timedResult struct {
		body *verifiedObjectBody
		err  error
	}
	started := time.Now()
	done := make(chan timedResult, 1)
	go func() {
		body, err := client.readVerifiedObject(
			t.Context(),
			reader,
			digest,
			expectedObject{
				kind:              ObjectKindChunk,
				maxDecodedBytes:   uint64(len(content)),
				exactDecodedBytes: totalPointer(uint64(len(content))),
			},
			newByteGate(2<<20),
		)
		done <- timedResult{body: body, err: err}
	}()
	<-decoderBlocked
	time.Sleep(50 * time.Millisecond)
	close(releaseDecoder)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.body.release()
	totalElapsed := time.Since(started)
	if result.body.transferLatency+25*time.Millisecond >= totalElapsed {
		t.Fatalf(
			"transfer latency %s included local decoder delay in total %s",
			result.body.transferLatency,
			totalElapsed,
		)
	}

	gate := newAdaptiveRequestGate(adaptiveGateConfig{
		min:                    1,
		max:                    4,
		initial:                2,
		increaseAfterSuccesses: 1,
		now:                    func() time.Time { return time.Unix(1, 0) },
	})
	gate.gradient.baseline = max(result.body.transferLatency.Seconds(), 1e-9)
	permit, err := gate.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	permit.completeWithLatency(transferSuccess, result.body.transferLatency)
	if got := gate.currentLimit(); got < 2 {
		t.Fatalf("local post-transfer delay caused a soft cut to %d", got)
	}
}

func TestVerifiedObjectEnforcesDecoderResourceContract(t *testing.T) {
	t.Run("strict limits", func(t *testing.T) {
		encoded := []byte("decoded")
		digest := testFixtureDigest(encoded)
		var received DecodeLimits
		client := newTestVolumeClient(t)
		client.decoder = DecoderFunc(func(
			_ context.Context,
			dst io.Writer,
			src io.Reader,
			limits DecodeLimits,
		) error {
			received = limits
			_, err := io.Copy(dst, src)
			return err
		})
		reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
			return Object{
				Body:     io.NopCloser(bytes.NewReader(encoded)),
				Size:     int64(len(encoded)),
				Kind:     ObjectKindChunk,
				Encoding: ObjectEncodingZstd,
			}, nil
		})
		exact := uint64(len(encoded))
		budget := newByteGate(2 << 20)
		body, err := client.readVerifiedObject(
			t.Context(),
			reader,
			digest,
			expectedObject{
				kind:              ObjectKindChunk,
				maxDecodedBytes:   exact,
				exactDecodedBytes: &exact,
			},
			budget,
		)
		if err != nil {
			t.Fatal(err)
		}
		want := DecodeLimits{
			MaxEncodedBytes: exact + metadataEncodingOverhead,
			MaxDecodedBytes: exact,
			MaxWindowBytes:  minZstdResourceBytes,
			MaxMemoryBytes:  minZstdResourceBytes,
		}
		if received != want {
			t.Fatalf("decode limits = %+v, want %+v", received, want)
		}
		body.release()
		if got := budget.bytesInUse(); got != 0 {
			t.Fatalf("released decoder budget = %d, want 0", got)
		}
	})

	t.Run("encoded bound", func(t *testing.T) {
		maxDecoded := uint64(1)
		limits := decodeLimits(maxDecoded)
		encoded := make([]byte, int(limits.MaxEncodedBytes+1))
		client := newTestVolumeClient(t)
		reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
			return Object{
				Body: io.NopCloser(bytes.NewReader(encoded)), Size: -1,
				Kind: ObjectKindChunk, Encoding: ObjectEncodingZstd,
			}, nil
		})
		_, err := client.readVerifiedObject(
			t.Context(),
			reader,
			testDigest(1),
			expectedObject{kind: ObjectKindChunk, maxDecodedBytes: maxDecoded},
			newByteGate(2<<20),
		)
		if err == nil || !IsCode(err, ErrorPreconditionFailed) {
			t.Fatalf("encoded bound error = %v, want %s", err, ErrorPreconditionFailed)
		}
	})

	t.Run("decoded bound", func(t *testing.T) {
		client := newTestVolumeClient(t)
		client.decoder = DecoderFunc(func(
			_ context.Context,
			dst io.Writer,
			_ io.Reader,
			_ DecodeLimits,
		) error {
			_, err := dst.Write([]byte("too large"))
			return err
		})
		reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
			return Object{
				Body: io.NopCloser(strings.NewReader("frame")), Size: 5,
				Kind: ObjectKindChunk, Encoding: ObjectEncodingZstd,
			}, nil
		})
		_, err := client.readVerifiedObject(
			t.Context(),
			reader,
			testDigest(2),
			expectedObject{kind: ObjectKindChunk, maxDecodedBytes: 1},
			newByteGate(2<<20),
		)
		if err == nil || !IsCode(err, ErrorPreconditionFailed) {
			t.Fatalf("decoded bound error = %v, want %s", err, ErrorPreconditionFailed)
		}
	})

	t.Run("trailing frame", func(t *testing.T) {
		decoded := []byte("a")
		digest := testFixtureDigest(decoded)
		client := newTestVolumeClient(t)
		client.decoder = DecoderFunc(func(
			_ context.Context,
			dst io.Writer,
			src io.Reader,
			_ DecodeLimits,
		) error {
			var first [1]byte
			if _, err := io.ReadFull(src, first[:]); err != nil {
				return err
			}
			_, err := dst.Write(first[:])
			return err
		})
		reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
			return Object{
				Body: io.NopCloser(strings.NewReader("abc")), Size: 3,
				Kind: ObjectKindChunk, Encoding: ObjectEncodingZstd,
			}, nil
		})
		_, err := client.readVerifiedObject(
			t.Context(),
			reader,
			digest,
			expectedObject{kind: ObjectKindChunk, maxDecodedBytes: 1},
			newByteGate(2<<20),
		)
		if err == nil || !IsCode(err, ErrorIntegrity) {
			t.Fatalf("trailing frame error = %v, want %s", err, ErrorIntegrity)
		}
	})

	t.Run("decoder wrapped cancellation", func(t *testing.T) {
		client := newTestVolumeClient(t)
		client.decoder = DecoderFunc(func(
			context.Context,
			io.Writer,
			io.Reader,
			DecodeLimits,
		) error {
			return fmt.Errorf(
				"https://signed.invalid/frame?credential=secret: %w",
				context.DeadlineExceeded,
			)
		})
		reader := ObjectReaderFunc(func(context.Context, ObjectRequest) (Object, error) {
			return Object{
				Body:     io.NopCloser(strings.NewReader("frame")),
				Size:     5,
				Kind:     ObjectKindChunk,
				Encoding: ObjectEncodingZstd,
			}, nil
		})
		_, err := client.readVerifiedObject(
			t.Context(),
			reader,
			testDigest(3),
			expectedObject{kind: ObjectKindChunk, maxDecodedBytes: 1},
			newByteGate(2<<20),
		)
		if err == nil ||
			!IsCode(err, ErrorCanceled) ||
			!errors.Is(err, context.DeadlineExceeded) ||
			errors.Unwrap(err) != context.DeadlineExceeded {
			t.Fatalf("decoder cancellation error = %v", err)
		}
		if rendered := fmt.Sprintf("%+v", err); strings.Contains(rendered, "secret") ||
			strings.Contains(rendered, "signed.invalid") {
			t.Fatalf("decoder cancellation leaked boundary details: %s", rendered)
		}
	})
}
