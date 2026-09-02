// Package limiter is an additive-increase/multiplicative-decrease concurrency
// limiter with a latency-gradient soft-stall detector, for governing how many
// object operations a transfer runs against one origin at a time.
//
// It is vendored from a simulation-validated implementation maintained
// elsewhere rather than written here, and is kept as close to that source as
// the excisions below allow, because its value is that it has been driven
// against a queue model of an origin across a wide range of link classes and
// is known to settle near the throughput knee. There is no upstream module
// to depend on, so it is synchronised by hand: a change made here does not
// reach the original, and a fix made there does not reach this copy.
//
// Five deliberate divergences from that source, all recorded so a reader does
// not mistake them for sloppiness, and so that anyone re-synchronising can tell
// a deliberate change from drift. The list is meant to be exhaustive: apart
// from these five and the package clause, every remaining difference from the
// source is a comment.
//
// The first three are removals this module requires — on a re-sync they are
// re-applied:
//
//   - Metrics are removed, since this module has no metrics dependency and
//     will not take one. The three reporting helpers are kept as empty
//     functions with all of their call sites intact, so the hot path stays
//     byte-identical to the source it was proven in; emptying three one-line
//     functions is a smaller risk than editing nine call sites by hand.
//   - The config field that labelled those metrics is gone, rather than left
//     as a knob that does nothing.
//   - Identifiers and strings that named the source's own services, packages
//     or paths are renamed in the sim test file. They are hygiene only: no
//     behaviour, no thresholds, no assertions change. Each test file says so
//     at its top.
//
// The last two point the other way: fixes to defects in the source's own
// tests, which belong upstream — a re-sync should carry them to the original
// rather than quietly restore the defect:
//
//   - The cancel-at-grant test fake closes its done channel exactly once, so
//     consulting Err a third time answers instead of panicking; a Context's
//     Err carries no limit on how often it may be called.
//   - The sim harness's non-blocking acquire counts its admission through the
//     in-flight seam as the real fast path does, since the permits it hands
//     out complete through the real release, which counts the exit.
package limiter

import (
	"container/list"
	"context"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Outcome reports how a limited operation finished so the AdaptiveLimiter can
// steer concurrency. Only stalls shrink the window; clean successes grow it;
// everything else (4xx, validation, caller cancellation) is neutral so it
// neither rewards nor punishes the origin.
type Outcome int

const (
	// OutcomeNeutral leaves the concurrency limit unchanged. Use for results
	// that say nothing about origin saturation (e.g. HTTP 4xx, caller cancel).
	OutcomeNeutral Outcome = iota
	// OutcomeSuccess additively grows the limit toward Max.
	OutcomeSuccess
	// OutcomeStall multiplicatively shrinks the limit toward Min. Use for
	// timeouts, dial/TLS failures, and connection resets — the signals that an
	// upstream (S3/ECR/CDN) is too slow under the current concurrency.
	OutcomeStall
)

// Gradient detector tuning. Module constants, not config: nothing varies per
// call site, and each value is coupled to the others (the set was validated
// together in a processor-sharing queue simulation across 12 link classes —
// localhost, same-region S3, 100/200 Gbps fabrics, EU->us-west WAN, high
// variance, regime shifts; see adaptive_limiter_sim_test.go, which
// drives this exact limiter).
const (
	// gradShortAlpha is the fast EWMA weight for the short latency view
	// (~20-sample smoothing): quick enough to see queue growth, smooth enough
	// to not chase single samples.
	gradShortAlpha = 0.05
	// gradBucket is the baseline bucket width. The baseline ingests one value
	// (the bucket mean) per this interval, making its drift wall-clock-bounded
	// regardless of load. (A per-completion EWMA absorbs inflation faster the
	// more loaded the origin is — exactly backwards.)
	gradBucket = time.Second
	// gradBaselineAlpha is the baseline EWMA weight per bucket in the normal
	// (un-inflated, or at-floor) state: tracks genuine latency changes in ~20s.
	gradBaselineAlpha = 0.05
	// gradBaselineInflatedAlpha is the baseline weight for *upward* moves while
	// inflation is suspected: near-frozen, so the baseline cannot absorb the
	// very signal being detected.
	gradBaselineInflatedAlpha = 0.002
	// gradSigmaAlpha is the EWMA weight of the sigma (ratio noise) estimate.
	gradSigmaAlpha = 0.02
	// gradSigmaFloor keeps gates from going hair-trigger on a perfectly quiet
	// link.
	gradSigmaFloor = 0.02
	// Grow gate = 1 + clamp(2*sigma, lo, hi): below it latency is flat — fill
	// the pipe fast.
	gradGrowGateSigma   = 2.0
	gradGrowGateClampLo = 0.08
	gradGrowGateClampHi = 0.45
	// Cut gate = 1 + clamp(5*sigma, lo, hi): above it latency inflation is far
	// outside noise — soft stall. The band between the gates is a hold/probe
	// zone (hysteresis).
	gradCutGateSigma   = 5.0
	gradCutGateClampLo = 0.35
	gradCutGateClampHi = 1.2
	// gradCutFactor is the soft-stall multiplicative factor. Gentler than the
	// hard-stall factor: it only needs to undo growth past the gate, and
	// cutting below the knee on a long-latency link drains the pipe (expensive
	// to refill). Hard stalls still use DecreaseFactor.
	gradCutFactor = 0.7
	// gradWarmupGenerations: the baseline seeds only after this many
	// generations (x Initial) of samples, so the seed sees a full latency mix,
	// not just the fastest finishers of the first wave (selection bias under
	// shared service).
	gradWarmupGenerations = 2.0
	// gradCooldownLatencyMult: the post-cut cooldown floor is the configured
	// DecreaseCooldown, stretched to this multiple of the current short
	// latency so the queue that existed before the cut can flush before the
	// next cut may fire. Requests already in flight at cut time keep
	// completing with pre-cut latency for ~one service time; a wall-clock-only
	// cooldown re-trips on those stale samples and cascades a single
	// congestion event into a drain to Min (a WAN queue flushes in seconds, a
	// fabric queue in ms).
	gradCooldownLatencyMult = 2.0
)

// AdaptiveLimiterConfig configures AIMD behaviour. Zero values are filled with
// sane defaults by NewAdaptiveLimiterManager.
type AdaptiveLimiterConfig struct {
	// Min is the concurrency floor (>= 1). The limiter never shrinks below
	// this, so a saturated origin still drains slowly instead of stalling
	// forever — important when upstream consumers handle slowdown better than
	// hard failure.
	Min int
	// Max is the concurrency ceiling.
	Max int
	// Initial is the starting limit; defaults to Max. Start optimistic and let
	// stalls pull it down.
	//
	// Note: the gradient detector's warmup scales with Initial (it seeds the
	// latency baseline after ~2×Initial samples), and with Initial=Max past
	// the origin's knee the baseline seeds on already-inflated latency — the
	// gradient then holds (ratio ≈ 1) and behaves like plain AIMD until the
	// first hard cut re-anchors it. That is the deliberate availability-first
	// trade-off of starting wide open.
	Initial int
	// DecreaseFactor multiplies the limit on a hard stall (0 < f < 1).
	// Default 0.5.
	DecreaseFactor float64
	// DecreaseCooldown is the minimum spacing between cuts, so a burst of
	// concurrent stalls is one decrease, not a collapse to Min. Default 1s.
	//
	// After each cut the effective cooldown is stretched to
	// max(DecreaseCooldown, 2×observed latency) when latency samples are being
	// fed (see gradCooldownLatencyMult), so pre-cut in-flight requests that
	// fail late — e.g. 10/30/90s transport-timeout tails — cannot land one
	// halving per cooldown window and drain the limit to Min from a single
	// congestion event.
	DecreaseCooldown time.Duration
	// IncreaseAfterSuccesses is the number of successes required to add one to
	// the limit in the fast-growth zone. 0 means "current limit" (≈ one step
	// per fully-utilised window), which yields gentle, self-pacing recovery.
	// In the gradient's hold/probe band the step is always the current limit
	// regardless of this setting.
	IncreaseAfterSuccesses int
}

// DataPathLimiterConfig is the AIMD posture for a client data path (bulk
// chunk transfer). Request concurrency is a network-control variable, not a
// CPU-work floor: large unrestricted GPU nodes can expose hundreds of CPUs
// while a WAN throughput knee sits below 64 requests. Keep a small progress
// floor, use cores×2 only as a small-host startup hint capped at 32, and treat
// max as the static fd/socket guard the limiter is expected to settle below.
// Linear (+1 per success) recovery fills fat pipes after warmup.
func DataPathLimiterConfig(maxConcurrency int) AdaptiveLimiterConfig {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	initial := min(runtime.GOMAXPROCS(0)*2, 32)
	initial = max(initial, 8)
	return AdaptiveLimiterConfig{
		Min:                    min(4, maxConcurrency),
		Max:                    maxConcurrency,
		Initial:                min(initial, maxConcurrency),
		DecreaseFactor:         0.5,
		DecreaseCooldown:       time.Second,
		IncreaseAfterSuccesses: 1,
	}
}

func (c AdaptiveLimiterConfig) withDefaults() AdaptiveLimiterConfig {
	if c.Min < 1 {
		c.Min = 1
	}
	if c.Max < c.Min {
		c.Max = c.Min
	}
	if c.Initial <= 0 || c.Initial > c.Max {
		c.Initial = c.Max
	}
	if c.Initial < c.Min {
		c.Initial = c.Min
	}
	if c.DecreaseFactor <= 0 || c.DecreaseFactor >= 1 {
		c.DecreaseFactor = 0.5
	}
	if c.DecreaseCooldown <= 0 {
		c.DecreaseCooldown = time.Second
	}
	if c.IncreaseAfterSuccesses < 0 {
		c.IncreaseAfterSuccesses = 0
	}
	return c
}

// gradient holds the latency-gradient soft-stall detector state. It watches
// request-latency inflation — the physical symptom of queueing at the origin
// (past the knee, latency grows ~linearly with in-flight count; below it,
// latency is flat):
//
//	ratio = short / long        short: fast EWMA of recent transfer latency
//	                            long:  slow "normal latency" baseline
//	ratio < grow gate  -> grow fast (configured increase step)
//	ratio in gate band -> grow gently (+1 per `limit` successes — probe)
//	ratio > cut gate   -> soft stall: gentle multiplicative cut (×0.7)
//
// Discrete stalls alone are not enough: a fast, homogeneous link (cluster
// fabric, localhost) never emits one — every request succeeds, so plain AIMD
// climbs to the ceiling and parks *past* the throughput peak (measured ~25%
// loss on the workload this was built for). The gradient supplies the missing
// signal. Lives inside the limiter's mutex; all updates are a few arithmetic
// ops per completion.
type gradient struct {
	// short is a fast EWMA of real-transfer latency (seconds). 0 until the
	// first sample.
	short float64
	// long is the slow baseline: "normal" latency (seconds). 0 until warmup
	// completes.
	long float64
	// Current bucket accumulator for the baseline. bucketStart is zero until
	// the first sample.
	bucketStart time.Time
	bucketSum   float64
	bucketN     float64
	// sigma is a downside-only noise estimate of the ratio (E|noise|
	// equivalent). Queue inflation only ever pushes the ratio *up*, so noise
	// is estimated from downward deviations only (×2 for the symmetric-noise
	// equivalent): estimating from all deviations lets real inflation widen
	// the very gates meant to catch it.
	sigma float64
}

func newGradient() gradient {
	return gradient{sigma: 0.05} // sane prior; adapts within a few buckets
}

// ratio returns short/long and whether the baseline has warmed up.
func (g *gradient) ratio() (float64, bool) {
	if g.long <= 0 {
		return 0, false
	}
	return g.short / g.long, true
}

func (g *gradient) growGate() float64 {
	return 1 + clamp(gradGrowGateSigma*g.sigma, gradGrowGateClampLo, gradGrowGateClampHi)
}

func (g *gradient) cutGate() float64 {
	return 1 + clamp(gradCutGateSigma*g.sigma, gradCutGateClampLo, gradCutGateClampHi)
}

func clamp(v, lo, hi float64) float64 {
	return math.Min(math.Max(v, lo), hi)
}

// AdaptiveLimiter is an AIMD concurrency limiter for a single upstream host,
// with a latency-gradient "soft stall" detector. The limit grows additively on
// clean successes and shrinks multiplicatively (cooldown-debounced) on stall
// signals; sustained latency inflation past the noise-scaled cut gate is a
// soft stall (gentle ×0.7 cut).
//
// Acquire blocks until a slot is free (it never rejects), so callers degrade
// by slowing down rather than failing — the limiter is a back-pressure valve,
// not a circuit breaker.
//
// Blocked acquirers wait in a FIFO queue and are granted exactly one wakeup
// each when a slot frees, so a release is O(slots-freed) rather than
// O(waiters): the large fan-out this is built for does not turn into an O(n²)
// thundering herd, and there is no starvation.
//
// Why request count and not a byte window: the throughput-vs-concurrency curve
// of a request-oriented transfer against a contended origin is an inverted U
// (a peak, then decline from queueing/thrash), which a bandwidth-delay byte
// window (BBR) structurally cannot find. AIMD does not model the curve at all
// — it reacts to the feedback the origin emits past the peak.
type AdaptiveLimiter struct {
	cfg   AdaptiveLimiterConfig
	host  string
	nowFn func() time.Time

	mu        sync.Mutex
	limit     float64 // fractional for smooth additive increase
	inflight  int
	successes int
	// nextCutOK is the earliest time the next cut (hard or soft) may fire.
	nextCutOK time.Time
	// cuts is the cut generation, incremented on every applied cut. Permits
	// record it at acquire time; a hard stall whose permit predates the latest
	// cut is *stale* — the request was issued into the pre-cut window, so its
	// failure reports congestion the cut already responded to. Without this,
	// a single congestion event drains the limit to Min: the pre-cut cohort
	// fails on transport-timeout tails (10/30/90s), far past any
	// latency-scaled cooldown (which stretches by ~success latency, not
	// failure latency), landing one halving per cooldown window for the whole
	// tail. Soft stalls are not generation-filtered: their signal comes from
	// success latency, which is exactly the horizon the stretched cooldown
	// covers.
	cuts uint64
	grad gradient
	// waiters is a FIFO queue. The releaser stamps the cut generation at the
	// exact grant point, removes the head, and closes its channel.
	waiters list.List
}

type limiterWaiter struct {
	ready chan struct{}
	gen   uint64
}

func newAdaptiveLimiter(cfg AdaptiveLimiterConfig, host string, nowFn func() time.Time) *AdaptiveLimiter {
	if nowFn == nil {
		nowFn = time.Now
	}
	l := &AdaptiveLimiter{
		cfg:   cfg,
		host:  host,
		nowFn: nowFn,
		limit: float64(cfg.Initial),
		grad:  newGradient(),
	}
	l.setLimit(cfg.Initial) // safe pre-publish; not yet shared
	return l
}

// Permit is an acquired concurrency slot. Report the operation's outcome via
// exactly one of Complete or CompleteWithLatency; extra calls are no-ops.
// Dropping a Permit without completing it leaks the slot.
type Permit struct {
	l *AdaptiveLimiter
	// gen is the cut generation at acquire time (stale-stall detection).
	gen  uint64
	done atomic.Bool
}

// Complete reports the outcome (stall → multiplicative cut, success → additive
// growth) and releases the slot. No latency sample is fed: use this for
// completions whose timing says nothing about data-path congestion — dedup
// hits, tiny metadata requests, coalesced followers, and errors (a failed
// request's timing reflects the failure mode, not origin service latency).
func (p *Permit) Complete(o Outcome) {
	if p.done.CompareAndSwap(false, true) {
		p.l.release(o, 0, false, p.gen)
	}
}

// CompleteWithLatency reports the outcome plus the operation's network latency
// (time the request itself, not surrounding hashing/disk work) and releases
// the slot. The sample feeds the latency-gradient soft-stall detector; only
// call this for real transfers — near-instant responses (dedup, metadata)
// would drag the baseline low and poison the ratio.
func (p *Permit) CompleteWithLatency(o Outcome, latency time.Duration) {
	if p.done.CompareAndSwap(false, true) {
		p.l.release(o, latency, true, p.gen)
	}
}

// Acquire blocks until a concurrency slot is available or ctx is done. On
// success it returns a Permit that must be completed exactly once.
func (l *AdaptiveLimiter) Acquire(ctx context.Context) (*Permit, error) {
	start := l.nowFn()

	l.mu.Lock()
	if err := ctx.Err(); err != nil {
		l.mu.Unlock()
		return nil, err
	}
	// Fast path: a slot is free and nobody is queued ahead of us (preserve
	// FIFO — never jump the line even when capacity exists).
	if l.waiters.Len() == 0 && l.inflight < l.effectiveLimitLocked() {
		l.inflight++
		l.addInFlight(1)
		gen := l.cuts
		l.mu.Unlock()
		return &Permit{l: l, gen: gen}, nil
	}
	waiter := &limiterWaiter{ready: make(chan struct{})}
	elem := l.waiters.PushBack(waiter)
	l.mu.Unlock()

	select {
	case <-waiter.ready:
		// Granted: the releaser already incremented inflight on our behalf.
		if err := ctx.Err(); err != nil {
			l.observeWait(l.nowFn().Sub(start), waitOutcomeCancelled)
			l.release(OutcomeNeutral, 0, false, waiter.gen)
			return nil, err
		}
		// Generation is read at grant time — that is when the operation
		// actually starts running against the origin.
		l.observeWait(l.nowFn().Sub(start), waitOutcomeAcquired)
		return &Permit{l: l, gen: waiter.gen}, nil
	case <-ctx.Done():
		l.mu.Lock()
		select {
		case <-waiter.ready:
			// Raced with a grant between ctx firing and taking the lock: we
			// own a slot we won't use. Hand it back (and promote the next
			// waiter) through the normal release path.
			l.mu.Unlock()
			l.observeWait(l.nowFn().Sub(start), waitOutcomeCancelled)
			l.release(OutcomeNeutral, 0, false, 0)
		default:
			l.waiters.Remove(elem)
			l.mu.Unlock()
			l.observeWait(l.nowFn().Sub(start), waitOutcomeCancelled)
		}
		return nil, ctx.Err()
	}
}

func (l *AdaptiveLimiter) release(o Outcome, latency time.Duration, hasLatency bool, gen uint64) {
	now := l.nowFn()

	l.mu.Lock()
	if l.inflight > 0 {
		l.inflight--
		l.addInFlight(-1)
	}

	// 1. Ingest the latency sample into the gradient detector — clean
	//    successes only. A stalled/failed request's timing reflects the
	//    failure mode (timeout budget, retry loop), not origin service
	//    latency, and must not move the baseline or noise estimate.
	softStall := o == OutcomeSuccess && hasLatency && l.observeLatencyLocked(now, latency)

	// 2. Steer the limit: hard stalls cut by DecreaseFactor; gradient trips
	//    cut gently; successes grow at a rate staged by the latency ratio.
	//    A hard stall from a permit issued before the latest cut is stale —
	//    congestion the cut already answered — and is treated as neutral
	//    (see the `cuts` field for the drain-to-Min failure mode).
	limitChanged := false
	switch o {
	case OutcomeStall:
		if gen == l.cuts {
			limitChanged = l.cutLocked(now, l.cfg.DecreaseFactor, false)
		}
	case OutcomeSuccess:
		if softStall {
			limitChanged = l.cutLocked(now, gradCutFactor, true)
		} else {
			limitChanged = l.increaseLocked()
		}
	case OutcomeNeutral:
	}

	// 3. Grant any freed/created capacity to queued waiters.
	l.promoteLocked()
	limit := l.effectiveLimitLocked()
	l.mu.Unlock()

	if limitChanged {
		l.setLimit(limit)
	}
}

// promoteLocked grants freed/created capacity to queued waiters, head-first.
// It wakes exactly one waiter per available slot (O(slots-freed)), so a release
// never iterates the whole waiter set.
func (l *AdaptiveLimiter) promoteLocked() {
	for l.waiters.Len() > 0 && l.inflight < l.effectiveLimitLocked() {
		front := l.waiters.Front()
		l.waiters.Remove(front)
		l.inflight++
		// Publish the grant before waking the waiter; otherwise a very fast
		// completion can decrement the gauge before this increment appears.
		l.addInFlight(1)
		waiter := front.Value.(*limiterWaiter)
		waiter.gen = l.cuts
		close(waiter.ready)
	}
}

// observeLatencyLocked feeds one real-transfer latency sample into the
// gradient detector. Returns true when the sample pushes the ratio past the
// cut gate (a soft stall).
func (l *AdaptiveLimiter) observeLatencyLocked(now time.Time, latency time.Duration) bool {
	lat := latency.Seconds()
	g := &l.grad

	// Short view: fast EWMA.
	if g.short == 0 {
		g.short = lat
	} else {
		g.short += gradShortAlpha * (lat - g.short)
	}

	// Long baseline: bucketed by wall clock.
	if g.bucketStart.IsZero() {
		g.bucketStart = now
	}
	g.bucketSum += lat
	g.bucketN++
	if now.Sub(g.bucketStart) >= gradBucket {
		l.rollBaselineBucketLocked(now)
	}

	ratio, ok := g.ratio()
	if !ok {
		return false // still warming up: no gradient signal yet
	}

	// Noise scale, learned from DOWNSIDE deviations only (×2 for the
	// symmetric equivalent of E|x|): inflation only pushes the ratio up, so
	// the downside is pure noise in every state — no truncation bias, no
	// poisoning by real inflation. Floored against hair-trigger gates.
	g.sigma = math.Max(
		g.sigma+gradSigmaAlpha*(2*math.Max(1-ratio, 0)-g.sigma),
		gradSigmaFloor,
	)

	return ratio > g.cutGate()
}

// rollBaselineBucketLocked folds the completed bucket's mean into the long
// baseline (seeding it after warmup) and starts a new bucket.
//
// Warmup: the baseline doesn't seed until the bucket holds a representative
// mix (>= 2 generations of samples), not just the first wave's fastest
// finishers (selection bias under shared service). Until then the bucket
// keeps accumulating past its nominal width (on a slow link, one width holds
// too few samples) — resetting it would discard warmup progress and hold
// growth forever.
func (l *AdaptiveLimiter) rollBaselineBucketLocked(now time.Time) {
	g := &l.grad
	if g.long <= 0 && g.bucketN < gradWarmupGenerations*float64(l.cfg.Initial) {
		return // still warming up: keep accumulating
	}
	mean := g.bucketSum / g.bucketN
	if g.long <= 0 {
		g.long = mean
	} else {
		inflated := g.short/g.long >= g.growGate()
		// Floor self-heal: at the concurrency floor we cannot be causing
		// congestion, so observed latency IS the baseline by definition —
		// absorb at full speed. This makes a mis-seeded baseline (or a
		// permanently slower origin) self-correcting instead of a permanent
		// latch.
		atFloor := l.effectiveLimitLocked() <= l.cfg.Min
		alpha := gradBaselineAlpha
		if mean > g.long && inflated && !atFloor {
			alpha = gradBaselineInflatedAlpha
		}
		g.long += alpha * (mean - g.long)
	}
	g.bucketStart = now
	g.bucketSum = 0
	g.bucketN = 0
}

// cutLocked applies a multiplicative cut (hard or soft), debounced by the
// latency-scaled cooldown: requests queued before a cut keep completing with
// pre-cut latency for ~one service time, and re-tripping on those stale
// samples would cascade a single congestion event into a collapse to Min.
func (l *AdaptiveLimiter) cutLocked(now time.Time, factor float64, soft bool) bool {
	if !l.nextCutOK.IsZero() && now.Before(l.nextCutOK) {
		return false
	}
	cooldown := l.cfg.DecreaseCooldown
	if flush := time.Duration(gradCooldownLatencyMult * l.grad.short * float64(time.Second)); flush > cooldown {
		cooldown = flush
	}
	l.nextCutOK = now.Add(cooldown)
	l.cuts++ // everything in flight is now the stale generation
	old := l.limit
	l.limit = math.Max(float64(l.cfg.Min), l.limit*factor)
	l.successes = 0
	return l.limit != old
}

// increaseLocked applies the additive increase, staged by the latency ratio
// when the gradient has data: fast below the grow gate, gentle (+1 per `limit`
// successes ≈ 1/latency per second, so detector-lag overshoot is O(1) slots)
// in the gate band, held above the cut gate.
func (l *AdaptiveLimiter) increaseLocked() bool {
	step, grow := l.successStepLocked()
	if !grow {
		return false
	}

	l.successes++
	if l.successes < step {
		return false
	}
	l.successes = 0
	old := l.limit
	l.limit = math.Min(float64(l.cfg.Max), l.limit+1)
	changed := l.limit != old
	return changed
}

// successStepLocked returns the successes-per-increase for the current
// gradient state, and false to hold growth entirely.
func (l *AdaptiveLimiter) successStepLocked() (int, bool) {
	if l.grad.short == 0 {
		// No latency samples ever: plain AIMD (untimed callers).
		return l.fastStepLocked(), true
	}
	ratio, ok := l.grad.ratio()
	if !ok {
		// Samples arriving but baseline still warming up: HOLD. A fast link
		// completes ~1000 requests/s — ungated growth would climb through the
		// whole range before the baseline ever sees un-inflated latency
		// (validated failure mode).
		return 0, false
	}
	switch {
	case ratio < l.grad.growGate():
		return l.fastStepLocked(), true
	case ratio < l.grad.cutGate():
		// Hold/probe band: +1 per `limit` successes ≈ 1/latency per second.
		return max(int(l.limit), 1), true
	default:
		return 0, false // above the cut gate: hold
	}
}

// fastStepLocked is the successes-per-increase in the fast-growth zone:
// the configured IncreaseAfterSuccesses, where 0 means "current limit"
// (≈ one step per fully-utilised window — gentle, self-pacing recovery).
func (l *AdaptiveLimiter) fastStepLocked() int {
	step := l.cfg.IncreaseAfterSuccesses
	if step <= 0 {
		step = int(l.limit)
	}
	if step < 1 {
		step = 1
	}
	return step
}

func (l *AdaptiveLimiter) effectiveLimitLocked() int {
	n := int(l.limit)
	if n < l.cfg.Min {
		n = l.cfg.Min
	}
	return n
}

// Limit returns the current effective concurrency limit. Intended for tests
// and observability.
func (l *AdaptiveLimiter) Limit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.effectiveLimitLocked()
}

// setLimit, addInFlight and observeWait reported the limiter's state to a
// metrics registry in the implementation this was vendored from. They are
// kept as no-ops, rather than deleted, so that every call site in Acquire,
// release and the adjustment path stays exactly as it was proven: the
// argument for vendoring is that nobody retyped the hot path, and removing
// them would mean ten hand edits through it. Filling them in is the one
// change needed to observe this limiter from outside.
func (l *AdaptiveLimiter) setLimit(limit int) {}

func (l *AdaptiveLimiter) addInFlight(delta int) {}

func (l *AdaptiveLimiter) observeWait(d time.Duration, outcome string) {}

// AdaptiveLimiterManager hands out one AdaptiveLimiter per upstream host, all
// sharing the same AIMD policy. The consumers use a bounded set of cache,
// registry, and object-store hosts, so entries live for the process lifetime.
//
// Keying is per host: that is the right granularity for the generic limiter
// (ECR/CDN/registries throttle per host). A finer key (e.g. S3 host+prefix)
// would be an origin-specific concern and does not belong in a generic
// limiter; if a deployment ever needs it, the host the caller keys on is the
// single seam to widen the key — the limiter itself stays key-agnostic.
type AdaptiveLimiterManager struct {
	cfg   AdaptiveLimiterConfig
	nowFn func() time.Time

	mu       sync.Mutex
	limiters map[string]*AdaptiveLimiter
}

// NewAdaptiveLimiterManager creates a manager applying cfg (with defaults) to
// every per-host limiter it creates.
func NewAdaptiveLimiterManager(cfg AdaptiveLimiterConfig) *AdaptiveLimiterManager {
	return newAdaptiveLimiterManager(cfg, time.Now)
}

// newAdaptiveLimiterManager allows tests to inject a clock for cooldown and
// gradient timing.
func newAdaptiveLimiterManager(cfg AdaptiveLimiterConfig, nowFn func() time.Time) *AdaptiveLimiterManager {
	return &AdaptiveLimiterManager{
		cfg:      cfg.withDefaults(),
		nowFn:    nowFn,
		limiters: make(map[string]*AdaptiveLimiter),
	}
}

// Acquire obtains a slot from host's adaptive limiter, creating it on first
// use. ctx comes first to match the repository's context-bearing API
// convention.
func (m *AdaptiveLimiterManager) Acquire(ctx context.Context, host string) (*Permit, error) {
	m.mu.Lock()
	limiter := m.limiters[host]
	if limiter == nil {
		limiter = newAdaptiveLimiter(m.cfg, host, m.nowFn)
		m.limiters[host] = limiter
	}
	m.mu.Unlock()
	return limiter.Acquire(ctx)
}

// limiterForTest returns the current limiter without retaining it. Callers
// must not use it concurrently with manager Acquire; tests use it only for
// deterministic state inspection.
func (m *AdaptiveLimiterManager) limiterForTest(host string) *AdaptiveLimiter {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limiters[host]
}

const (
	waitOutcomeAcquired  = "acquired"
	waitOutcomeCancelled = "cancelled"
)
