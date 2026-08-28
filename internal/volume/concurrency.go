package volume

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// This is a dependency-free adaptation of a validated latency-gradient
// data-path limiter. Only the request gate and latency-gradient state belong in
// this module; metrics, host management, and observability remain outside it.
//
// These constants were validated as a coupled set and must be evaluated
// together when tuning behavior.
const (
	gradientShortAlpha            = 0.05
	gradientBucketDuration        = time.Second
	gradientBaselineAlpha         = 0.05
	gradientInflatedBaselineAlpha = 0.002
	gradientSigmaAlpha            = 0.02
	gradientSigmaFloor            = 0.02
	gradientGrowSigma             = 2.0
	gradientGrowMin               = 0.08
	gradientGrowMax               = 0.45
	gradientCutSigma              = 5.0
	gradientCutMin                = 0.35
	gradientCutMax                = 1.2
	gradientSoftCutFactor         = 0.7
	gradientWarmupGenerations     = 2.0
	gradientCooldownLatencyFactor = 2.0
)

type transferOutcome uint8

const (
	transferNeutral transferOutcome = iota
	transferSuccess
	transferStall
)

type adaptiveGateConfig struct {
	min                    int
	max                    int
	initial                int
	decreaseFactor         float64
	decreaseCooldown       time.Duration
	increaseAfterSuccesses int
	now                    func() time.Time
}

// adaptiveRequestGate applies additive increase and multiplicative decrease
// (AIMD) to data-path request concurrency. Transfer stalls trigger hard cuts;
// sustained latency inflation triggers soft cuts. The independent byteGate
// bounds resident transfer and decoder memory.
type adaptiveRequestGate struct {
	mu                     sync.Mutex
	min                    int
	max                    int
	initial                int
	limit                  int
	inUse                  int
	successes              int
	decreaseFactor         float64
	decreaseCooldown       time.Duration
	nextDecrease           time.Time
	increaseAfterSuccesses int
	now                    func() time.Time
	changed                chan struct{}
	gradient               latencyGradient
}

type latencyGradient struct {
	short       float64
	baseline    float64
	bucketStart time.Time
	bucketSum   float64
	bucketCount float64
	sigma       float64
}

func (gradient latencyGradient) ratio() (float64, bool) {
	if gradient.baseline <= 0 {
		return 0, false
	}
	return gradient.short / gradient.baseline, true
}

func (gradient latencyGradient) growGate() float64 {
	return 1 + min(max(gradientGrowSigma*gradient.sigma, gradientGrowMin), gradientGrowMax)
}

func (gradient latencyGradient) cutGate() float64 {
	return 1 + min(max(gradientCutSigma*gradient.sigma, gradientCutMin), gradientCutMax)
}

func newDataPathRequestGate(maximum int) *adaptiveRequestGate {
	cores := max(runtime.GOMAXPROCS(0), 1)
	minimum, initial := dataPathRequestGatePosture(maximum, cores)
	return newAdaptiveRequestGate(adaptiveGateConfig{
		min:                    minimum,
		max:                    maximum,
		initial:                initial,
		decreaseFactor:         0.5,
		decreaseCooldown:       time.Second,
		increaseAfterSuccesses: 1,
		now:                    time.Now,
	})
}

func dataPathRequestGatePosture(maximum int, cores int) (int, int) {
	minimum := min(4, maximum)
	cores = max(cores, 1)
	startupByCores := 32
	if cores < 16 {
		startupByCores = cores * 2
	}
	initial := min(max(max(startupByCores, 8), minimum), 32, maximum)
	return minimum, initial
}

func newAdaptiveRequestGate(config adaptiveGateConfig) *adaptiveRequestGate {
	minimum := max(config.min, 1)
	maximum := max(config.max, minimum)
	initial := min(max(config.initial, minimum), maximum)
	factor := config.decreaseFactor
	if factor <= 0 || factor >= 1 {
		factor = 0.5
	}
	increaseAfter := max(config.increaseAfterSuccesses, 1)
	now := config.now
	if now == nil {
		now = time.Now
	}
	return &adaptiveRequestGate{
		min:                    minimum,
		max:                    maximum,
		initial:                initial,
		limit:                  initial,
		decreaseFactor:         factor,
		decreaseCooldown:       max(config.decreaseCooldown, 0),
		increaseAfterSuccesses: increaseAfter,
		now:                    now,
		changed:                make(chan struct{}),
		gradient:               latencyGradient{sigma: 0.05},
	}
}

func (gate *adaptiveRequestGate) acquire(ctx context.Context) (*requestPermit, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gate.mu.Lock()
		if err := ctx.Err(); err != nil {
			gate.mu.Unlock()
			return nil, err
		}
		if gate.inUse < gate.limit {
			gate.inUse++
			gate.mu.Unlock()
			return &requestPermit{gate: gate}, nil
		}
		changed := gate.changed
		gate.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (gate *adaptiveRequestGate) currentLimit() int {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.limit
}

func (gate *adaptiveRequestGate) release(
	outcome transferOutcome,
	latency *time.Duration,
) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.inUse == 0 {
		panic("volume: adaptive request permit released without acquisition")
	}
	now := gate.now()
	softStall := false
	if outcome == transferSuccess && latency != nil {
		softStall = gate.observeLatencyLocked(now, *latency)
	}
	switch {
	case outcome == transferStall:
		gate.cutLocked(now, gate.decreaseFactor)
	case outcome == transferSuccess && softStall:
		gate.cutLocked(now, gradientSoftCutFactor)
	case outcome == transferSuccess:
		gate.increaseLocked()
	case outcome == transferNeutral:
	default:
		panic("volume: unknown adaptive request outcome")
	}
	gate.inUse--
	close(gate.changed)
	gate.changed = make(chan struct{})
}

func (gate *adaptiveRequestGate) observeLatencyLocked(now time.Time, latency time.Duration) bool {
	seconds := max(latency.Seconds(), 1e-9)
	gradient := &gate.gradient
	if gradient.short == 0 {
		gradient.short = seconds
	} else {
		gradient.short += gradientShortAlpha * (seconds - gradient.short)
	}
	if gradient.bucketStart.IsZero() {
		gradient.bucketStart = now
	}
	gradient.bucketSum += seconds
	gradient.bucketCount++
	if now.Sub(gradient.bucketStart) >= gradientBucketDuration {
		warmed := gradient.baseline > 0 ||
			gradient.bucketCount >= gradientWarmupGenerations*float64(gate.initial)
		if warmed {
			mean := gradient.bucketSum / gradient.bucketCount
			if gradient.baseline == 0 {
				gradient.baseline = mean
			} else {
				ratio, _ := gradient.ratio()
				inflated := ratio >= gradient.growGate()
				alpha := gradientBaselineAlpha
				if mean > gradient.baseline && inflated && gate.limit > gate.min {
					alpha = gradientInflatedBaselineAlpha
				}
				gradient.baseline += alpha * (mean - gradient.baseline)
			}
			gradient.bucketStart = now
			gradient.bucketSum = 0
			gradient.bucketCount = 0
		}
	}
	ratio, ready := gradient.ratio()
	if !ready {
		return false
	}
	downside := 2 * max(1-ratio, 0.0)
	gradient.sigma = max(
		gradient.sigma+gradientSigmaAlpha*(downside-gradient.sigma),
		gradientSigmaFloor,
	)
	return ratio > gradient.cutGate()
}

func (gate *adaptiveRequestGate) cutLocked(now time.Time, factor float64) {
	if !gate.nextDecrease.IsZero() && now.Before(gate.nextDecrease) {
		return
	}
	latencyCooldown := time.Duration(
		gradientCooldownLatencyFactor * gate.gradient.short * float64(time.Second),
	)
	gate.nextDecrease = now.Add(max(gate.decreaseCooldown, latencyCooldown))
	next := max(int(float64(gate.limit)*factor), gate.min)
	gate.limit = min(next, gate.max)
	gate.successes = 0
}

func (gate *adaptiveRequestGate) increaseLocked() {
	step := gate.increaseAfterSuccesses
	if gate.gradient.short > 0 {
		ratio, ready := gate.gradient.ratio()
		if !ready {
			return
		}
		switch {
		case ratio < gate.gradient.growGate():
		case ratio < gate.gradient.cutGate():
			step = max(gate.limit, 1)
		default:
			return
		}
	}
	gate.successes++
	if gate.successes >= step {
		gate.successes = 0
		gate.limit = min(gate.limit+1, gate.max)
	}
}

type requestPermit struct {
	gate      *adaptiveRequestGate
	completed atomic.Bool
}

func (permit *requestPermit) complete(outcome transferOutcome) {
	if permit != nil && permit.completed.CompareAndSwap(false, true) {
		permit.gate.release(outcome, nil)
	}
}

func (permit *requestPermit) completeWithLatency(
	outcome transferOutcome,
	latency time.Duration,
) {
	if permit != nil && permit.completed.CompareAndSwap(false, true) {
		permit.gate.release(outcome, &latency)
	}
}

type byteGate struct {
	mu       sync.Mutex
	cap      uint64
	inUse    uint64
	changed  chan struct{}
	waitHook func()
}

var errByteReservationExceedsCapacity = errors.New("byte reservation exceeds capacity")

func newByteGate(capacity uint64) *byteGate {
	return &byteGate{
		cap:     max(capacity, uint64(1)),
		changed: make(chan struct{}),
	}
}

func (gate *byteGate) acquire(ctx context.Context, bytes uint64) (*bytePermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if bytes > gate.cap {
		return nil, errByteReservationExceedsCapacity
	}
	weight := max(bytes, uint64(1))
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gate.mu.Lock()
		if err := ctx.Err(); err != nil {
			gate.mu.Unlock()
			return nil, err
		}
		if weight <= gate.cap-gate.inUse {
			gate.inUse += weight
			gate.mu.Unlock()
			return &bytePermit{gate: gate, weight: weight}, nil
		}
		changed := gate.changed
		waitHook := gate.waitHook
		gate.mu.Unlock()
		if waitHook != nil {
			waitHook()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func byteGateError(operation string, err error) error {
	if contextErr := contextError(operation, err); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, errByteReservationExceedsCapacity) {
		return preconditionError(operation, "byte reservation exceeds the configured memory limit")
	}
	return protocolError(operation, "operation byte budget failed")
}

func (gate *byteGate) bytesInUse() uint64 {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.inUse
}

type bytePermit struct {
	gate     *byteGate
	weight   uint64
	released atomic.Bool
}

func (permit *bytePermit) release() {
	if permit == nil || !permit.released.CompareAndSwap(false, true) {
		return
	}
	permit.gate.mu.Lock()
	defer permit.gate.mu.Unlock()
	if permit.weight > permit.gate.inUse {
		panic("volume: byte permit exceeds bytes in use")
	}
	permit.gate.inUse -= permit.weight
	close(permit.gate.changed)
	permit.gate.changed = make(chan struct{})
}

func classifyTransferOutcome(
	err error,
	observation TransferObservation,
	transferred bool,
) transferOutcome {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return transferNeutral
	}
	if err != nil {
		classification := adapterErrorFrom(err)
		if classification != nil {
			switch classification.Kind {
			case AdapterErrorKindCancellation,
				AdapterErrorKindCredentials,
				AdapterErrorKindIntegrity:
				return transferNeutral
			}
			if classification.StallObserved {
				return transferStall
			}
		}
		var typed *Error
		if errors.As(err, &typed) && typed.Code != ErrorTransfer {
			return transferNeutral
		}
		if observation.StallObserved {
			return transferStall
		}
		if observation.RetryCount != 0 {
			return transferNeutral
		}
		if errors.As(err, &typed) && typed.Code == ErrorTransfer {
			return transferStall
		}
		return transferNeutral
	}
	if !transferred {
		return transferNeutral
	}
	if observation.StallObserved {
		return transferStall
	}
	if observation.RetryCount != 0 {
		return transferNeutral
	}
	return transferSuccess
}
