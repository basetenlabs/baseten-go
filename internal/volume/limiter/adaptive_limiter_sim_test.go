package limiter

// Vendored verbatim alongside the limiter, apart from this note, the package
// name, and a set of hygiene renames: identifiers and strings that named the
// source's own service, package or path (a config helper and its two tests,
// the report environment variable, and the example command) carry neutral names
// here. One fix to the source's own harness: tryAcquire counts its admission
// through the in-flight seam as the real fast path does (the package doc's
// fifth divergence, the kind a re-sync carries upstream). Nothing else
// differs — no scenario, threshold, seed or assertion is changed. See the
// package doc for the full list of deliberate divergences.

// Simulation harness for AdaptiveLimiter: drives the *real* limiter against a
// processor-sharing queue model of an origin, under the injected fake clock
// (deterministic, runs in milliseconds of wall time). A port of the
// validation harness that proved the vendored limiter, so results can be
// examined side by side (see TestSimReport, which prints the same format).
//
// # Origin model
//
// The origin serves at capacity C_eff(N) bytes/s shared equally over the N
// outstanding requests, each additionally capped at a per-stream rate
// (CHUNK/baseLat — one stream alone can't go faster). Request latency is
// *emergent* (completion − issue): a request issued into a queue that then
// grows experiences the congestion honestly, which matters because issue-time
// latency snapshots systematically feed the detector stale-low samples during
// climbs (a bug the original validation harness had).
//
// Two regimes fall out naturally:
//   - below the knee N* = C·baseLat/CHUNK: the per-stream cap binds, latency
//     is flat ≈ baseLat, throughput scales with N;
//   - above it: shared capacity binds, latency grows ~linearly with N, and
//     throughput plateaus then *declines* (inverted U) via a thrash term that
//     degrades C_eff — matching the measured localhost push curve
//     (1061 MB/s at 8 chunks → 762 MB/s at 256).
//
// Discrete stalls (timeouts / S3 503s) fire only at DEEP saturation (>2×
// knee) or a socket boundary: S3 throttles per-prefix request *rate* (~3500/s,
// far above these transfers), and timeouts need real queue blowup.
//
// # Go additions beyond the original grid
//
//   - simBrownout: an outage window in which issued requests hang and fail on
//     transport-timeout tails (10/30/90s) — the stale-cohort drain
//     scenario (see TestSim_BrownoutTimeoutTailsDoNotDrainToMin).
//   - an availability-first scenario (Initial=Max).
//
// Thresholds are set ~5-10 points below observed multi-seed results so the
// tests catch regressions in the control loop, not noise.

import (
	"fmt"
	"math"
	"os"
	"testing"
	"time"
)

const (
	simChunk = 8.0 * 1_048_576.0
	// 2 GiB memory cap in 8 MiB chunks (the second call-site gate in the
	// consuming engine; kept for scoring fidelity).
	simMemCapChunks = 256
	// Matches the data-path config on a 10-core client host.
	simCores = 10
)

// tryAcquire is a non-blocking Acquire for the sim harness: the closed-loop
// drive admits until the limiter (or the memory cap) says stop, without
// needing goroutines against the virtual clock.
func (l *AdaptiveLimiter) tryAcquire() *Permit {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.waiters.Len() == 0 && l.inflight < l.effectiveLimitLocked() {
		l.inflight++
		// Through the seam, as Acquire's fast path does: permits complete
		// through the real release, which decrements it — an admission that
		// skipped the increment would drive a wired-up gauge negative.
		l.addInFlight(1)
		return &Permit{l: l, gen: l.cuts}
	}
	return nil
}

// dataPathCfg is the data-path transfer config on a 10-core host — the config
// every grid scenario ran under.
func dataPathCfg() AdaptiveLimiterConfig {
	return AdaptiveLimiterConfig{
		Min:                    simCores,
		Max:                    512,
		Initial:                simCores * 2,
		DecreaseFactor:         0.5,
		DecreaseCooldown:       time.Second,
		IncreaseAfterSuccesses: 1,
	}
}

// availabilityFirstCfg mirrors a deployed availability-first posture:
// Initial=Max, so a transfer starts wide open and adapts down.
func availabilityFirstCfg() AdaptiveLimiterConfig {
	return AdaptiveLimiterConfig{
		Min:                    4,
		Max:                    64,
		IncreaseAfterSuccesses: 2,
		// Initial defaults to Max; cooldown/factor default to 1s / 0.5.
	}
}

// largeNodeDataPathCfg is DataPathLimiterConfig's posture on any node with
// >=16 effective CPUs. CPU count must not raise the network-control floor or
// open hundreds of requests at startup on 96/192-core GPU nodes.
func largeNodeDataPathCfg() AdaptiveLimiterConfig {
	return AdaptiveLimiterConfig{
		Min:                    4,
		Max:                    512,
		Initial:                32,
		DecreaseFactor:         0.5,
		DecreaseCooldown:       time.Second,
		IncreaseAfterSuccesses: 1,
	}
}

type simOrigin struct {
	// capacity is the shared service capacity at/above the knee, bytes/s
	// (pre-thrash).
	capacity float64
	// baseLat is the per-request latency below the knee, seconds.
	baseLat float64
	// thrash is the inverted-U slope: efficiency loss per knee-multiple past
	// the knee.
	thrash float64
	// cv is the coefficient of variation of per-request work (lognormal).
	cv float64
	// stallP is the probability a request issued past 2x knee hard-stalls
	// (timeout/503).
	stallP float64
	// sockLimit is a hard in-flight boundary (ENOBUFS-style discrete stall).
	sockLimit int
}

func (o simOrigin) knee() float64 {
	return max(o.capacity*o.baseLat/simChunk, 1.0)
}

func (o simOrigin) throughputAt(n int) float64 {
	nf := float64(max(n, 1))
	knee := o.knee()
	if nf <= knee {
		return nf * simChunk / o.baseLat
	}
	return o.capacity / (1.0 + o.thrash*(nf-knee)/knee)
}

// peak is the best achievable throughput across admissible N (within the
// memory cap): the score denominator.
func (o simOrigin) peak() float64 {
	best := 0.0
	for n := 1; n <= simMemCapChunks; n++ {
		best = max(best, o.throughputAt(n))
	}
	return best
}

// simRng is xorshift64 + Box-Muller: deterministic per-seed noise, no
// dependency. Bit-identical to the harness RNG it reproduces.
type simRng struct{ s uint64 }

func newSimRng(seed uint64) *simRng {
	return &simRng{s: 0x9e37_79b9_7f4a_7c15 ^ seed*0xd134_2543_de82_ef95}
}

func (r *simRng) nextF64() float64 {
	r.s ^= r.s << 13
	r.s ^= r.s >> 7
	r.s ^= r.s << 17
	return float64(r.s>>11) / float64(uint64(1)<<53)
}

func (r *simRng) gauss() float64 {
	u1 := max(r.nextF64(), 1e-12)
	u2 := r.nextF64()
	return math.Sqrt(-2.0*math.Log(u1)) * math.Cos(2.0*math.Pi*u2)
}

// simShift replaces (capacity, baseLat) mid-run at `at`.
type simShift struct {
	at       time.Duration
	capacity float64
	baseLat  float64
}

// simBrownout is an outage window: every request issued in [start, end) hangs
// (consumes no origin capacity — a dial that will never answer) and fails at
// issue + a transport-timeout tail cycling through 10/30/90s. This is the
// congestion-event profile the stale-cohort filter exists for.
type simBrownout struct {
	start, end time.Duration
}

var simTimeoutTailsSec = []float64{10, 30, 90}

type simInFlight struct {
	permit    *Permit
	remaining float64
	issued    time.Time
	stall     bool
	// failAt non-zero marks a hung (brownout) request: it consumes no
	// capacity and fails as a stall at this instant.
	failAt time.Time
}

type simResult struct {
	// tputFrac is mean throughput over the second half, as a fraction of
	// simOrigin.peak (post-shift origin when shifting).
	tputFrac float64
	// meanLimit is the mean effective limit over the second half.
	meanLimit float64
	// minLimit is the lowest effective limit observed at any completion in
	// the second half (drain detector; Go addition).
	minLimit int
}

// simulate is a closed-loop drive of the real limiter against the origin for
// `dur` of virtual time.
func simulate(
	cfg AdaptiveLimiterConfig,
	origin simOrigin,
	dur time.Duration,
	seed uint64,
	shift *simShift,
	brownout *simBrownout,
) simResult {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := newAdaptiveLimiter(cfg.withDefaults(), "sim", clk.Now)
	rng := newSimRng(seed)
	start := clk.Now()
	half := dur / 2
	var reqs []*simInFlight
	doneBytes := 0.0
	limitSum, limitN := 0.0, 0.0
	minLimit := math.MaxInt
	shifted := false
	timeoutIdx := 0

	// Score against the post-shift origin (the measurement window is the
	// second half, after the shift).
	scored := origin
	if shift != nil {
		scored.capacity, scored.baseLat = shift.capacity, shift.baseLat
	}

	for {
		now := clk.Now()
		elapsed := now.Sub(start)
		if elapsed >= dur {
			break
		}
		if shift != nil && !shifted && elapsed >= shift.at {
			origin.capacity, origin.baseLat = shift.capacity, shift.baseLat
			shifted = true
		}

		// Admit up to the composed limit (concurrency slot AND memory cap).
		for len(reqs) < simMemCapChunks {
			permit := l.tryAcquire()
			if permit == nil {
				break
			}
			// Per-request work noise (mean-preserving lognormal).
			work := simChunk
			if origin.cv > 0 {
				work *= math.Exp(origin.cv*rng.gauss() - origin.cv*origin.cv/2)
			}
			n1 := len(reqs) + 1
			deep := float64(n1) > 2.0*origin.knee()
			r := &simInFlight{
				permit:    permit,
				remaining: work,
				issued:    now,
				stall:     (deep && rng.nextF64() < origin.stallP) || n1 > origin.sockLimit,
			}
			if brownout != nil && elapsed >= brownout.start && elapsed < brownout.end {
				r.stall = true
				r.failAt = now.Add(time.Duration(
					simTimeoutTailsSec[timeoutIdx%len(simTimeoutTailsSec)] * float64(time.Second)))
				timeoutIdx++
			}
			reqs = append(reqs, r)
		}

		// Processor sharing over live (non-hung) requests: advance virtual
		// time to the next completion at the current shared rate, or to the
		// next hung-request failure, whichever is sooner.
		live := 0
		minRem := math.MaxFloat64
		for _, r := range reqs {
			if r.failAt.IsZero() {
				live++
				minRem = min(minRem, r.remaining)
			}
		}
		n := float64(max(live, 1))
		knee := origin.knee()
		eff := 1.0
		if n > knee {
			eff = 1.0 / (1.0 + origin.thrash*(n-knee)/knee)
		}
		rate := min(origin.capacity*eff/n, simChunk/origin.baseLat)
		dt := 0.05 // all hung: idle until the next timeout fires
		if live > 0 {
			dt = max(minRem/rate, 1e-6)
		}
		for _, r := range reqs {
			if !r.failAt.IsZero() {
				if until := r.failAt.Sub(now).Seconds(); until < dt {
					dt = max(until, 1e-6)
				}
			}
		}
		// Never score completions beyond the requested horizon. Long WAN
		// service intervals can otherwise jump seconds past dur while the
		// denominator remains the fixed half-window.
		dt = min(dt, (dur - elapsed).Seconds())
		clk.advance(time.Duration(dt * float64(time.Second)))
		now = clk.Now()
		for _, r := range reqs {
			if r.failAt.IsZero() {
				r.remaining -= rate * dt
			}
		}

		// Complete finished requests through the real permit API.
		inWindow := now.Sub(start) > half
		for i := 0; i < len(reqs); {
			r := reqs[i]
			finished := r.remaining <= 1e-3
			if !r.failAt.IsZero() {
				finished = !now.Before(r.failAt)
			}
			if !finished {
				i++
				continue
			}
			reqs[i] = reqs[len(reqs)-1]
			reqs = reqs[:len(reqs)-1]
			if inWindow {
				limitSum += float64(l.Limit())
				limitN++
			}
			if r.stall {
				r.permit.Complete(OutcomeStall)
			} else {
				if inWindow {
					// Goodput is one nominal chunk per completion: the
					// lognormal `work` noise models per-request *service
					// time* variability, not payload size — every real chunk
					// is simChunk bytes on the wire.
					doneBytes += simChunk
				}
				r.permit.CompleteWithLatency(OutcomeSuccess, now.Sub(r.issued))
			}
			if inWindow {
				minLimit = min(minLimit, l.Limit())
			}
		}
	}

	window := (dur - half).Seconds()
	return simResult{
		tputFrac:  doneBytes / window / scored.peak(),
		meanLimit: limitSum / max(limitN, 1),
		minLimit:  minLimit,
	}
}

// runSeeds runs 3 seeds; returns (min tput fraction, mean limit across seeds,
// min of minLimit across seeds).
func runSeeds(
	cfg AdaptiveLimiterConfig,
	origin simOrigin,
	dur time.Duration,
	shift *simShift,
	brownout *simBrownout,
) (float64, float64, int) {
	minFrac := math.MaxFloat64
	limitSum := 0.0
	minLimit := math.MaxInt
	for seed := uint64(1); seed <= 3; seed++ {
		r := simulate(cfg, origin, dur, seed, shift, brownout)
		minFrac = min(minFrac, r.tputFrac)
		limitSum += r.meanLimit
		minLimit = min(minLimit, r.minLimit)
	}
	return minFrac, limitSum / 3.0, minLimit
}

// TestSimReport is the port-fidelity report: the full 12-scenario grid, 5
// seeds each, printed in the original format for side-by-side comparison.
// Skipped by default (it is a report, not an assertion); run with
//
//	VOLUME_SIM_REPORT=1 go test ./internal/volume/limiter -run TestSimReport -v
func TestSimReport(t *testing.T) {
	if os.Getenv("VOLUME_SIM_REPORT") == "" {
		t.Skip("manual port-fidelity report; set VOLUME_SIM_REPORT=1 to run")
	}
	scenarios := []struct {
		label  string
		origin simOrigin
		durS   int
		shift  *simShift
	}{
		{"cluster-internal", simOrigin{1061e6, 0.063, 0.0126, 0.10, 0.0, 400}, 60, nil},
		{"same-region", simOrigin{1000e6, 0.100, 0.005, 0.30, 0.02, 1000}, 120, nil},
		{"wan", simOrigin{630e6, 0.600, 0.002, 0.50, 0.05, 1000}, 120, nil},
		{"wan-quiet high-variance", simOrigin{630e6, 0.600, 0.002, 0.70, 0.0, 1000}, 120, nil},
		{"slow link", simOrigin{100e6, 0.200, 0.01, 0.30, 0.02, 1000}, 120, nil},
		{"100G cluster", simOrigin{8000e6, 0.010, 0.0126, 0.10, 0.0, 4000}, 60, nil},
		{"200G cluster", simOrigin{16000e6, 0.008, 0.0126, 0.10, 0.0, 4000}, 60, nil},
		{"wan eu->usw", simOrigin{630e6, 0.700, 0.002, 0.50, 0.03, 2000}, 180, nil},
		{"wan fat 2GB/s", simOrigin{2000e6, 0.700, 0.002, 0.50, 0.02, 2000}, 180, nil},
		{"wan 40G via s3", simOrigin{5000e6, 0.700, 0.002, 0.50, 0.02, 2000}, 180, nil},
		{"wan 40G direct", simOrigin{5000e6, 0.150, 0.002, 0.40, 0.0, 2000}, 180, nil},
		{
			"cluster shift", simOrigin{1061e6, 0.063, 0.0126, 0.10, 0.0, 400}, 90,
			&simShift{at: 30 * time.Second, capacity: 400e6, baseLat: 0.063},
		},
	}
	for _, sc := range scenarios {
		dur := time.Duration(sc.durS) * time.Second
		var fracs []float64
		limitSum := 0.0
		for seed := uint64(1); seed <= 5; seed++ {
			r := simulate(dataPathCfg(), sc.origin, dur, seed, sc.shift, nil)
			fracs = append(fracs, r.tputFrac)
			limitSum += r.meanLimit
		}
		mean, low := 0.0, math.MaxFloat64
		for _, f := range fracs {
			mean += f
			low = min(low, f)
		}
		mean /= float64(len(fracs))
		fmt.Printf("  %-28s conc=%4.0f tput mean=%3.0f%% min=%3.0f%%\n",
			sc.label, limitSum/5.0, 100*mean, 100*low)
	}
}

// --- Ported grid assertion scenarios (thresholds unchanged) ----------------

// Localhost / cluster-internal push (the measured curve: 1061 MB/s peak at ~8
// chunks, degrading to ~762 at 256). No discrete stalls below the socket
// boundary — the case that motivated the gradient detector.
func TestSim_ClusterInternalHoldsNearPeak(t *testing.T) {
	o := simOrigin{1061e6, 0.063, 0.0126, 0.10, 0.0, 400}
	minFrac, meanLimit, _ := runSeeds(dataPathCfg(), o, 60*time.Second, nil, nil)
	if minFrac <= 0.90 {
		t.Errorf("cluster-internal: %.0f%%, want > 90%%", minFrac*100)
	}
	if meanLimit >= 100 {
		t.Errorf("must pin near the knee (~8), not park at the ceiling: %.0f", meanLimit)
	}
}

// 100 Gbps fabric: ~1000 completions/s and zero stall signal. Ungated AIMD
// out-runs cooldown cuts by 3 orders of magnitude and parks at ~75% — the
// staged-growth gate must self-limit the climb.
func TestSim_100GFabricHoldsNearPeak(t *testing.T) {
	o := simOrigin{8000e6, 0.010, 0.0126, 0.10, 0.0, 4000}
	minFrac, meanLimit, _ := runSeeds(dataPathCfg(), o, 60*time.Second, nil, nil)
	if minFrac <= 0.90 {
		t.Errorf("100G fabric: %.0f%%, want > 90%%", minFrac*100)
	}
	if meanLimit >= 120 {
		t.Errorf("knee ~10, got mean limit %.0f, want < 120", meanLimit)
	}
}

// EU -> us-west WAN: 0.7s/chunk, high variance, sparse deep-saturation 503s.
// False gradient cuts are expensive here (each drains a long-latency pipe);
// the noise-scaled gates and latency-scaled cooldown carry this case.
func TestSim_WanEuUswestFillsPipe(t *testing.T) {
	o := simOrigin{630e6, 0.700, 0.002, 0.50, 0.03, 2000}
	minFrac, _, _ := runSeeds(dataPathCfg(), o, 180*time.Second, nil, nil)
	if minFrac <= 0.70 {
		t.Errorf("wan eu->usw: %.0f%%, want > 70%%", minFrac*100)
	}
}

func TestSim_LargeNodeBoundedStartupFillsWan(t *testing.T) {
	o := simOrigin{630e6, 0.700, 0.002, 0.50, 0.03, 2000}
	minFrac, meanLimit, _ := runSeeds(largeNodeDataPathCfg(), o, 180*time.Second, nil, nil)
	if minFrac <= 0.70 {
		t.Errorf("large-node WAN: %.0f%%, want > 70%%", minFrac*100)
	}
	if meanLimit <= 32 {
		t.Errorf("mean limit %.0f did not grow beyond bounded startup 32", meanLimit)
	}
}

// High-variance WAN with NO discrete stalls at all (cv=0.7): the pure
// false-positive test — noise alone must not collapse the limit.
func TestSim_WanHighVarianceNoFalseCollapse(t *testing.T) {
	o := simOrigin{630e6, 0.600, 0.002, 0.70, 0.0, 1000}
	minFrac, _, _ := runSeeds(dataPathCfg(), o, 180*time.Second, nil, nil)
	if minFrac <= 0.80 {
		t.Errorf("wan high-variance: %.0f%%, want > 80%%", minFrac*100)
	}
}

// 40 Gbps WAN via S3 (0.7s/chunk): knee ~417 is ABOVE the 256-chunk memory
// cap, so the memory cap binds. Latency stays flat below the knee — the
// gradient must never cut, and the limiter must reach the cap.
func TestSim_40GWanMemcapBoundReachesCap(t *testing.T) {
	o := simOrigin{5000e6, 0.700, 0.002, 0.50, 0.02, 2000}
	minFrac, meanLimit, _ := runSeeds(dataPathCfg(), o, 180*time.Second, nil, nil)
	if minFrac <= 0.95 {
		t.Errorf("40G via S3: %.0f%%, want > 95%%", minFrac*100)
	}
	if meanLimit <= 256 {
		t.Errorf("must climb past the memory cap (256), got %.0f", meanLimit)
	}
}

// Regime shift: origin capacity drops 1061 -> 400 MB/s mid-run. The baseline
// must re-anchor to the new latency (floor self-heal) instead of latching the
// old regime and pinning at the floor.
func TestSim_RegimeShiftReanchors(t *testing.T) {
	o := simOrigin{1061e6, 0.063, 0.0126, 0.10, 0.0, 400}
	shift := &simShift{at: 30 * time.Second, capacity: 400e6, baseLat: 0.063}
	minFrac, _, _ := runSeeds(dataPathCfg(), o, 90*time.Second, shift, nil)
	if minFrac <= 0.85 {
		t.Errorf("regime shift: %.0f%%, want > 85%%", minFrac*100)
	}
}

// --- Go-specific scenarios (availability-first profile) ----------------------

// TestSim_BrownoutTimeoutTailsDoNotDrainToMin is the sim-level regression for
// the stale-cohort drain: a 5s origin brownout hangs a window's worth of
// requests, which then fail on 10/30/90s transport-timeout tails — stale
// stalls trickling in for ~90s after the origin has recovered. One congestion
// event must cost ~one cut, not a drain to Min (each stale arrival landing in
// its own cooldown window).
func TestSim_BrownoutTimeoutTailsDoNotDrainToMin(t *testing.T) {
	// Knee at ~45 with Max=64: the limiter sits high and stable, no organic
	// stalls (2x knee > Max) — every stall in the run comes from the
	// brownout cohort.
	o := simOrigin{630e6, 0.600, 0.002, 0.20, 0.0, 1000}
	brownout := &simBrownout{start: 30 * time.Second, end: 35 * time.Second}
	minFrac, _, minLimit := runSeeds(availabilityFirstCfg(), o, 150*time.Second, nil, brownout)
	if minLimit <= 8 {
		t.Errorf("limit drained to %d after a single brownout (Min=4): stale timeout tails cascaded", minLimit)
	}
	if minFrac <= 0.50 {
		t.Errorf("brownout recovery throughput: %.0f%%, want > 50%%", minFrac*100)
	}
}

// TestSim_AvailabilityFirstInitialMaxAdaptsDown pins the availability-first config:
// starting at Max past the origin's knee, deep-saturation stalls (429/503/
// timeouts) must pull the limit down toward the knee instead of letting it
// park at Max, and the floor must never be the resting state.
func TestSim_AvailabilityFirstInitialMaxAdaptsDown(t *testing.T) {
	// Same-region S3 profile: knee ~11.9, Max=64 > 2x knee — organic deep
	// stalls exist, as they do for a WAN origin.
	o := simOrigin{1000e6, 0.100, 0.005, 0.30, 0.02, 1000}
	minFrac, meanLimit, _ := runSeeds(availabilityFirstCfg(), o, 120*time.Second, nil, nil)
	if minFrac <= 0.70 {
		t.Errorf("availability-first profile: %.0f%%, want > 70%%", minFrac*100)
	}
	if meanLimit >= 60 {
		t.Errorf("mean limit %.0f parked at Max=64; stalls must adapt it down", meanLimit)
	}
	if meanLimit <= float64(availabilityFirstCfg().Min) {
		t.Errorf("mean limit %.0f pinned at Min; availability-first config must not rest on the floor", meanLimit)
	}
}
