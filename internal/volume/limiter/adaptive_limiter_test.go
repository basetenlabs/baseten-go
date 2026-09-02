package limiter

// Vendored verbatim alongside the limiter, apart from this note, the package
// name, one comment whose citation named the original source tree, and one
// fix to a defect in the source's own fake: cancelAtGrantContext now closes
// its done channel exactly once (the package doc's fourth divergence, the
// kind a re-sync carries upstream). No assertion or threshold differs from
// the original — see the package doc for the full list of deliberate
// divergences.

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// cancelAtGrantContext becomes canceled on the second Err call, after Acquire
// has selected a grant but before it returns the permit. From the second call
// on it stays canceled: a Context's Err may be consulted any number of times,
// so the fake must hold its answer rather than panic on a third look.
type cancelAtGrantContext struct {
	context.Context
	done      chan struct{}
	errCalls  atomic.Int32
	closeOnce sync.Once
}

func (c *cancelAtGrantContext) Done() <-chan struct{} { return c.done }

func (c *cancelAtGrantContext) Err() error {
	if c.errCalls.Add(1) == 1 {
		return nil
	}
	c.closeOnce.Do(func() { close(c.done) })
	return context.Canceled
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func testLimiter(t *testing.T, cfg AdaptiveLimiterConfig, clk *fakeClock) *AdaptiveLimiter {
	t.Helper()
	return newAdaptiveLimiter(cfg.withDefaults(), "test-host", clk.Now)
}

func mustAcquire(t *testing.T, l *AdaptiveLimiter) *Permit {
	t.Helper()
	p, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return p
}

func TestAdaptiveLimiter_StallShrinksSuccessGrows(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 2, Max: 16, DecreaseCooldown: time.Second}, clk)

	if got := l.Limit(); got != 16 {
		t.Fatalf("initial limit = %d, want 16 (defaults to Max)", got)
	}

	mustAcquire(t, l).Complete(OutcomeStall)
	if got := l.Limit(); got != 8 {
		t.Fatalf("limit after stall = %d, want 8 (16*0.5)", got)
	}

	// Cooldown debounces a second immediate stall.
	mustAcquire(t, l).Complete(OutcomeStall)
	if got := l.Limit(); got != 8 {
		t.Fatalf("limit after debounced stall = %d, want 8", got)
	}

	// After cooldown, another stall shrinks again.
	clk.advance(2 * time.Second)
	mustAcquire(t, l).Complete(OutcomeStall)
	if got := l.Limit(); got != 4 {
		t.Fatalf("limit after second stall = %d, want 4", got)
	}
}

func TestAdaptiveLimiter_RecoversToMax(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	// IncreaseAfterSuccesses=1 makes recovery deterministic: +1 per success.
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 1, Max: 4, Initial: 1, IncreaseAfterSuccesses: 1}, clk)

	if got := l.Limit(); got != 1 {
		t.Fatalf("initial limit = %d, want 1", got)
	}
	for want := 2; want <= 4; want++ {
		mustAcquire(t, l).Complete(OutcomeSuccess)
		if got := l.Limit(); got != want {
			t.Fatalf("limit = %d, want %d", got, want)
		}
	}
	// Capped at Max.
	mustAcquire(t, l).Complete(OutcomeSuccess)
	if got := l.Limit(); got != 4 {
		t.Fatalf("limit = %d, want 4 (capped at Max)", got)
	}
}

func TestAdaptiveLimiter_NeverBelowMin(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 3, Max: 64, DecreaseCooldown: time.Nanosecond}, clk)
	for i := 0; i < 20; i++ {
		clk.advance(time.Millisecond)
		mustAcquire(t, l).Complete(OutcomeStall)
	}
	if got := l.Limit(); got != 3 {
		t.Fatalf("limit = %d, want 3 (Min floor)", got)
	}
}

func TestAdaptiveLimiter_BlocksAtCapacityThenAdmits(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 1, Max: 1, Initial: 1}, clk)

	rel1 := mustAcquire(t, l)

	admitted := make(chan struct{})
	go func() {
		rel2, err := l.Acquire(context.Background())
		if err == nil {
			rel2.Complete(OutcomeNeutral)
		}
		close(admitted)
	}()

	select {
	case <-admitted:
		t.Fatal("second Acquire admitted while at capacity")
	case <-time.After(50 * time.Millisecond):
	}

	rel1.Complete(OutcomeNeutral) // free the slot
	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("second Acquire never admitted after release")
	}
}

func TestAdaptiveLimiter_AcquireRespectsContext(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 1, Max: 1, Initial: 1}, clk)

	rel := mustAcquire(t, l)
	defer rel.Complete(OutcomeNeutral)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx); err == nil {
		t.Fatal("Acquire should have failed with context deadline")
	}
}

func TestAdaptiveLimiter_AcquireRejectsAlreadyCancelledContext(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 1, Max: 1, Initial: 1}, clk)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire error = %v, want context.Canceled", err)
	}
	l.mu.Lock()
	inflight := l.inflight
	l.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("inflight = %d, want 0", inflight)
	}
}

func TestAdaptiveLimiter_AcquireRejectsCancellationAtGrant(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 1, Max: 1, Initial: 1}, clk)
	held := mustAcquire(t, l)

	ctx := &cancelAtGrantContext{Context: t.Context(), done: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		permit, err := l.Acquire(ctx)
		if permit != nil {
			permit.Complete(OutcomeNeutral)
		}
		result <- err
	}()

	deadline := time.After(time.Second)
	for {
		l.mu.Lock()
		queued := l.waiters.Len() == 1
		l.mu.Unlock()
		if queued {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Acquire did not queue")
		default:
			runtime.Gosched()
		}
	}

	held.Complete(OutcomeNeutral)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire did not return after cancellation and grant")
	}

	l.mu.Lock()
	inflight := l.inflight
	l.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("inflight = %d, want 0", inflight)
	}
}

func TestAdaptiveLimiter_ConcurrencyNeverExceedsLimit(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	const maxConc = 4
	l := testLimiter(t, AdaptiveLimiterConfig{Min: maxConc, Max: maxConc, Initial: maxConc}, clk)

	var inflight, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := l.Acquire(context.Background())
			if err != nil {
				return
			}
			cur := inflight.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inflight.Add(-1)
			rel.Complete(OutcomeSuccess)
		}()
	}
	wg.Wait()
	if peak.Load() > maxConc {
		t.Fatalf("peak concurrency = %d, want <= %d", peak.Load(), maxConc)
	}
}

func TestAdaptiveLimiter_ReleaseIdempotent(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 1, Max: 2, Initial: 2}, clk)
	rel := mustAcquire(t, l)
	rel.Complete(OutcomeStall)
	rel.Complete(OutcomeStall) // no-op; must not double-decrement inflight or double-shrink
	if got := l.Limit(); got != 1 {
		t.Fatalf("limit = %d, want 1 (single decrease only)", got)
	}
	// inflight should be 0; a fresh acquire at Max=2 must succeed immediately.
	rel2 := mustAcquire(t, l)
	rel2.Complete(OutcomeNeutral)
	// Mixed-method double completion is also a no-op.
	rel2.CompleteWithLatency(OutcomeStall, time.Second)
	if got := l.Limit(); got != 1 {
		t.Fatalf("limit = %d, want 1 (double completion must not cut again)", got)
	}
}

func waitForWaiters(t *testing.T, l *AdaptiveLimiter, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		got := l.waiters.Len()
		l.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiters never reached %d", n)
}

// TestAdaptiveLimiter_HighFanOutNoStarvation is the merge-gating check: under
// the thousands-of-chunks fan-out this limiter targets, releases must not turn
// into an O(n^2) thundering herd or starve waiters, and slots must not leak.
func TestAdaptiveLimiter_HighFanOutNoStarvation(t *testing.T) {
	// Real clock so the AIMD dynamics actually move during the run.
	l := newAdaptiveLimiter(
		AdaptiveLimiterConfig{Min: 2, Max: 16, Initial: 16, DecreaseCooldown: time.Millisecond}.withDefaults(),
		"test", time.Now,
	)

	const n = 2000
	var inflight, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			if i%7 == 0 { // exercise the cancellation path concurrently
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(context.Background(), time.Duration(i%3)*time.Millisecond)
				defer cancel()
			}
			rel, err := l.Acquire(ctx)
			if err != nil {
				return
			}
			cur := inflight.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(time.Microsecond)
			inflight.Add(-1)
			out := OutcomeSuccess
			if i%5 == 0 {
				out = OutcomeStall
			}
			rel.Complete(out)
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("high fan-out did not drain — possible starvation/deadlock")
	}

	if p := peak.Load(); p > 16 {
		t.Fatalf("peak inflight = %d, want <= 16 (Max)", p)
	}
	l.mu.Lock()
	leakedInflight := l.inflight
	leakedWaiters := l.waiters.Len()
	l.mu.Unlock()
	if leakedInflight != 0 {
		t.Fatalf("inflight leaked = %d, want 0", leakedInflight)
	}
	if leakedWaiters != 0 {
		t.Fatalf("waiters leaked = %d, want 0", leakedWaiters)
	}
}

// TestAdaptiveLimiter_FIFOFairness verifies queued waiters are granted in
// arrival order (no starvation / line-jumping).
func TestAdaptiveLimiter_FIFOFairness(t *testing.T) {
	l := newAdaptiveLimiter(
		AdaptiveLimiterConfig{Min: 1, Max: 1, Initial: 1}.withDefaults(),
		"test", time.Now,
	)
	rel0, err := l.Acquire(context.Background()) // occupy the single slot
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	order := make(chan int, 2)
	go func() {
		if r, err := l.Acquire(context.Background()); err == nil {
			order <- 1
			r.Complete(OutcomeNeutral)
		}
	}()
	waitForWaiters(t, l, 1) // ensure waiter 1 is enqueued before waiter 2
	go func() {
		if r, err := l.Acquire(context.Background()); err == nil {
			order <- 2
			r.Complete(OutcomeNeutral)
		}
	}()
	waitForWaiters(t, l, 2)

	rel0.Complete(OutcomeNeutral) // frees the slot; head (waiter 1) must win
	if first := <-order; first != 1 {
		t.Fatalf("FIFO violated: waiter %d granted before waiter 1", first)
	}
	if second := <-order; second != 2 {
		t.Fatalf("expected waiter 2 second, got %d", second)
	}
}

// TestAdaptiveLimiterManager_InjectableClock guards that the manager threads a
// custom clock into the limiters it creates (so cooldown is testable).
func TestAdaptiveLimiterManager_InjectableClock(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	m := newAdaptiveLimiterManager(
		AdaptiveLimiterConfig{Min: 2, Max: 16, DecreaseCooldown: time.Second},
		clk.Now,
	)
	rel, _ := m.Acquire(context.Background(), "h")
	rel.Complete(OutcomeStall)
	l := m.limiterForTest("h")
	if got := l.Limit(); got != 8 {
		t.Fatalf("limit after stall = %d, want 8", got)
	}
	// Same instant: cooldown must debounce (proves the injected clock is used).
	rel, _ = m.Acquire(context.Background(), "h")
	rel.Complete(OutcomeStall)
	if got := l.Limit(); got != 8 {
		t.Fatalf("limit after debounced stall = %d, want 8 (manager clock not threaded?)", got)
	}
	clk.advance(2 * time.Second)
	rel, _ = m.Acquire(context.Background(), "h")
	rel.Complete(OutcomeStall)
	if got := l.Limit(); got != 4 {
		t.Fatalf("limit after cooldown stall = %d, want 4", got)
	}
}

func TestAdaptiveLimiterManager_PerHostIsolation(t *testing.T) {
	m := NewAdaptiveLimiterManager(AdaptiveLimiterConfig{Min: 1, Max: 8})
	aPermit, _ := m.Acquire(context.Background(), "a.example.com")
	bPermit, _ := m.Acquire(context.Background(), "b.example.com")
	a := m.limiterForTest("a.example.com")
	b := m.limiterForTest("b.example.com")
	if a == b {
		t.Fatal("expected distinct limiters per host")
	}
	aPermit.Complete(OutcomeStall)
	bPermit.Complete(OutcomeNeutral)
	if a.Limit() == b.Limit() {
		t.Fatalf("stall on a (%d) should not affect b (%d)", a.Limit(), b.Limit())
	}
}

func TestDataPathLimiterConfigBoundsCPUScaling(t *testing.T) {
	old := runtime.GOMAXPROCS(3)
	t.Cleanup(func() { runtime.GOMAXPROCS(old) })
	cfg := DataPathLimiterConfig(512)
	if cfg.Min != 4 || cfg.Initial != 8 {
		t.Fatalf("3 CPUs: Min/Initial = %d/%d, want 4/8", cfg.Min, cfg.Initial)
	}
	runtime.GOMAXPROCS(192)
	cfg = DataPathLimiterConfig(512)
	if cfg.Min != 4 || cfg.Initial != 32 {
		t.Fatalf("192 CPUs: Min/Initial = %d/%d, want 4/32", cfg.Min, cfg.Initial)
	}
}

// --- latency-scaled cooldown (drain-to-Min regression) ----------------------

// TestAdaptiveLimiter_CooldownStretchedByLatency is the regression test for
// the stale-cohort drain: at a cut, the pre-cut in-flight requests keep
// failing with pre-cut latency (10/30/90s transport-timeout tails), and a
// fixed 1s cooldown would land one halving per second until Min. The cooldown
// must stretch to 2x the observed transfer latency.
func TestAdaptiveLimiter_CooldownStretchedByLatency(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 1, Max: 64, DecreaseCooldown: time.Second}, clk)

	// One timed transfer establishes the short latency EWMA at 5s.
	mustAcquire(t, l).CompleteWithLatency(OutcomeSuccess, 5*time.Second)

	// Congestion event: first stall cuts 64 -> 32.
	mustAcquire(t, l).Complete(OutcomeStall)
	if got := l.Limit(); got != 32 {
		t.Fatalf("limit after first stall = %d, want 32", got)
	}

	// Stale cohort: stalls trickle in past the 1s wall-clock cooldown but
	// within the 2x5s=10s latency-stretched window. NONE may cut.
	for i := 0; i < 8; i++ {
		clk.advance(time.Second) // t = 1s..8s after the cut
		mustAcquire(t, l).Complete(OutcomeStall)
		if got := l.Limit(); got != 32 {
			t.Fatalf("stale stall at +%ds cut the limit to %d, want 32 (one cut per flush window)", i+1, got)
		}
	}

	// Past the flush window a fresh stall cuts again (real, persistent
	// congestion must still shrink the limit).
	clk.advance(3 * time.Second) // t = 11s > 10s
	mustAcquire(t, l).Complete(OutcomeStall)
	if got := l.Limit(); got != 16 {
		t.Fatalf("limit after post-flush stall = %d, want 16", got)
	}
}

// TestAdaptiveLimiter_UntimedCallersKeepFixedCooldown pins the fallback: with
// no latency samples ever, the cooldown is the configured wall-clock value
// (plain AIMD, pre-gradient behaviour).
func TestAdaptiveLimiter_UntimedCallersKeepFixedCooldown(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 1, Max: 64, DecreaseCooldown: time.Second}, clk)

	mustAcquire(t, l).Complete(OutcomeStall)
	clk.advance(1100 * time.Millisecond)
	mustAcquire(t, l).Complete(OutcomeStall)
	if got := l.Limit(); got != 16 {
		t.Fatalf("limit = %d, want 16 (two cuts, fixed 1s cooldown)", got)
	}
}

// TestAdaptiveLimiter_StaleCohortStallsDoNotCascade is the other half of the
// drain-to-Min fix: stalls from permits issued *before* the last cut are stale
// (the cut already answered that congestion) and must not cut again — even
// when they trickle in over 10/30/90s transport-timeout tails, far past any
// latency-stretched cooldown.
func TestAdaptiveLimiter_StaleCohortStallsDoNotCascade(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 1, Max: 64, DecreaseCooldown: time.Second}, clk)

	// Fill a window's worth of in-flight requests (the pre-cut cohort).
	cohort := make([]*Permit, 32)
	for i := range cohort {
		cohort[i] = mustAcquire(t, l)
	}

	// The first timeout cuts 64 -> 32.
	cohort[0].Complete(OutcomeStall)
	if got := l.Limit(); got != 32 {
		t.Fatalf("limit after first stall = %d, want 32", got)
	}

	// The rest of the cohort fails spread across ~90s of timeout tails, each
	// arrival in its own cooldown window. None may cut.
	for i, p := range cohort[1:] {
		clk.advance(3 * time.Second)
		p.Complete(OutcomeStall)
		if got := l.Limit(); got != 32 {
			t.Fatalf("stale cohort stall %d cut the limit to %d, want 32", i+1, got)
		}
	}

	// A fresh post-cut request that stalls DOES cut — the cohort filter must
	// not mask real persistent congestion.
	clk.advance(2 * time.Second)
	mustAcquire(t, l).Complete(OutcomeStall)
	if got := l.Limit(); got != 16 {
		t.Fatalf("limit after fresh-generation stall = %d, want 16", got)
	}
}

// --- gradient detector (ported tests) ---------------------------------------

// feed drives n timed successes at latency lat, advancing the virtual clock by
// dt between completions.
func feed(t *testing.T, l *AdaptiveLimiter, clk *fakeClock, n int, lat, dt time.Duration) {
	t.Helper()
	for i := 0; i < n; i++ {
		p := mustAcquire(t, l)
		clk.advance(dt)
		p.CompleteWithLatency(OutcomeSuccess, lat)
	}
}

func TestAdaptiveLimiter_UntimedCompletionsNeverEngageGradient(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	// Only Complete() (no latency): behaves exactly like plain AIMD.
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 1, Max: 64, Initial: 2, IncreaseAfterSuccesses: 1}, clk)
	for i := 0; i < 100; i++ {
		p := mustAcquire(t, l)
		clk.advance(10 * time.Millisecond)
		p.Complete(OutcomeSuccess)
	}
	if got := l.Limit(); got != 64 {
		t.Fatalf("limit = %d, want 64 (ungated linear growth to max)", got)
	}
}

func TestAdaptiveLimiter_WarmupDefersBaselineThenFlatLatencyGrowsFast(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 1, Max: 512, Initial: 4, IncreaseAfterSuccesses: 1}, clk)
	// 2 generations = 8 samples + a full bucket must elapse before the
	// baseline seeds; flat latency after that keeps ratio ~1 -> fast zone.
	feed(t, l, clk, 200, 50*time.Millisecond, 20*time.Millisecond)
	if got := l.Limit(); got <= 150 {
		t.Fatalf("limit = %d, want > 150 (flat latency must not throttle growth)", got)
	}
}

func TestAdaptiveLimiter_LatencyInflationTripsSoftCut(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 1, Max: 512, Initial: 4, IncreaseAfterSuccesses: 1}, clk)
	// Establish a flat 50ms baseline...
	feed(t, l, clk, 200, 50*time.Millisecond, 20*time.Millisecond)
	grown := l.Limit()
	// ...then inflate latency 4x: the short EWMA crosses the cut gate and the
	// limit is cut gently (x0.7), repeatedly (cooldown-spaced).
	feed(t, l, clk, 300, 200*time.Millisecond, 20*time.Millisecond)
	if got := l.Limit(); got >= grown/2 {
		t.Fatalf("sustained 4x latency must cut: %d -> %d", grown, got)
	}
}

func TestAdaptiveLimiter_UntimedBurstCannotPoisonBaseline(t *testing.T) {
	// Documents the contract rather than the mechanism: dedup/metadata
	// completions go through Complete() (no latency). If they were timed, the
	// baseline would collapse and real chunks would read as inflated.
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 1, Max: 512, Initial: 4, IncreaseAfterSuccesses: 1}, clk)
	feed(t, l, clk, 200, 50*time.Millisecond, 20*time.Millisecond)
	before := l.Limit()
	// A burst of untimed (dedup-style) completions: no gradient effect.
	for i := 0; i < 50; i++ {
		mustAcquire(t, l).Complete(OutcomeSuccess)
	}
	if got := l.Limit(); got < before {
		t.Fatalf("untimed successes must still grow, never cut: %d -> %d", before, got)
	}
}

// TestAdaptiveLimiter_InitialMaxHoldsUntilHardCut pins the Initial=Max
// (availability-first) trade-off: warming up past the knee seeds the baseline
// on already-inflated latency, so the gradient holds (ratio ~1) instead of
// false-cutting; only a hard stall moves the limit. After the cut re-anchors
// the baseline lower, the gradient becomes live.
func TestAdaptiveLimiter_InitialMaxHoldsUntilHardCut(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := testLimiter(t, AdaptiveLimiterConfig{Min: 1, Max: 64, IncreaseAfterSuccesses: 1}, clk)
	// All warmup samples arrive at the same (inflated) 400ms: baseline seeds
	// at 400ms, ratio ~1 -> hold zone, no cuts, no growth past Max anyway.
	feed(t, l, clk, 300, 400*time.Millisecond, 10*time.Millisecond)
	if got := l.Limit(); got != 64 {
		t.Fatalf("limit = %d, want 64 (steady inflated latency must not false-cut)", got)
	}
}
