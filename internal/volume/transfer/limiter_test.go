package transfer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/volume"
)

// holdCapacity reports how many permits the limiter will hand out at once, by
// taking them until one blocks. Everything taken is returned Neutral, which
// carries no evidence, so measuring does not move the limit it measures.
func holdCapacity(t *testing.T, l volume.Limiter) int {
	t.Helper()
	var held []*volume.Permit
	for len(held) <= 600 {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		permit, err := l.Acquire(ctx)
		cancel()
		if err != nil {
			break
		}
		held = append(held, permit)
	}
	if len(held) > 600 {
		t.Fatal("the limiter never blocked")
	}
	for _, permit := range held {
		permit.CompleteUntimed(volume.Neutral)
	}
	return len(held)
}

// feed runs n successful operations through the limiter, timed or untimed.
func feed(t *testing.T, l volume.Limiter, n int, timed bool) {
	t.Helper()
	for range n {
		permit, err := l.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if timed {
			permit.Complete(volume.Success)
		} else {
			permit.CompleteUntimed(volume.Success)
		}
	}
}

// feedSlow runs one timed success that really took latency to complete, so the
// sample the detector receives is a transfer's rather than a stopwatch's
// rounding error.
func feedSlow(t *testing.T, l volume.Limiter, latency time.Duration) {
	t.Helper()
	permit, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(latency)
	permit.Complete(volume.Success)
}

// TestPinnedConcurrencyIsHonouredExactly covers the caller who named a number.
// Capping the load a transfer puts on a shared machine or a metered link only
// works if the cap is the cap, so a pinned count must neither adapt upward nor
// be treated as a starting suggestion.
func TestPinnedConcurrencyIsHonouredExactly(t *testing.T) {
	const pinned = 3
	l := defaultLimiter(volume.Concurrency{ChunkOperations: pinned})

	if got := holdCapacity(t, l); got != pinned {
		t.Fatalf("pinned limiter handed out %d permits, want %d", got, pinned)
	}
	// Success is what an adaptive limiter grows on. A pinned one must not.
	feed(t, l, 50, false)
	if got := holdCapacity(t, l); got != pinned {
		t.Errorf("pinned limiter grew to %d after successes; the cap must not adapt", got)
	}
}

// TestUnpinnedConcurrencyAdapts covers the caller who named nothing. They get a
// limiter that finds the origin's capacity, which means starting well above one
// and moving with the evidence rather than sitting where it was born.
func TestUnpinnedConcurrencyAdapts(t *testing.T) {
	l := defaultLimiter(volume.Concurrency{})

	initial := holdCapacity(t, l)
	if initial < 8 {
		t.Fatalf("adaptive limiter started at %d, want the configured floor of at least 8", initial)
	}

	// A stall is the origin asking for less, and must be obeyed at once: the
	// whole point of adapting is that pushback costs concurrency.
	permit, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	permit.Complete(volume.Stall)

	after := holdCapacity(t, l)
	if after >= initial {
		t.Errorf("limit was %d after a stall, %d before; a stall must shrink it", after, initial)
	}
	if after < 1 {
		t.Errorf("limit collapsed to %d; a stalled origin must still drain", after)
	}
}

// TestTimedCompletionsReachTheDetector pins the adapter's one real decision:
// which completions carry a latency sample.
//
// The distinction is observable without reaching inside the limiter. Untimed
// successes leave the latency detector with nothing, so the limiter runs as
// plain AIMD and grows. Timed successes feed it, and while its baseline is
// still warming up it holds growth deliberately — a fast link completes
// requests faster than the baseline can learn what normal looks like, so
// growing through that window would climb the whole range on no evidence.
//
// So: growth after untimed successes and no growth after timed ones is the
// signature of the samples arriving where they should. An adapter that sent
// everything down one path would show the same behaviour for both.
func TestTimedCompletionsReachTheDetector(t *testing.T) {
	untimed := defaultLimiter(volume.Concurrency{})
	start := holdCapacity(t, untimed)
	feed(t, untimed, 40, false)
	grown := holdCapacity(t, untimed)
	if grown <= start {
		t.Fatalf("untimed successes left the limit at %d (from %d); plain AIMD should have grown it", grown, start)
	}

	timed := defaultLimiter(volume.Concurrency{})
	feed(t, timed, 40, true)
	if held := holdCapacity(t, timed); held >= grown {
		t.Errorf("timed successes grew the limit to %d, as far as the untimed run's %d; "+
			"the latency samples are not reaching the detector", held, grown)
	}
}

// TestUntimedCompletionsStayOutOfTheBaseline is the other half of the adapter's
// routing decision, and the one with teeth.
//
// A real transfer is a mix: chunks that were actually fetched, and dedup hits
// and metadata requests that returned at once. The instant ones complete
// untimed. If they were fed to the detector as latency samples of nearly zero
// they would drag the baseline down — the detector's idea of "normal" would
// become an average of real transfers and non-transfers — and the very next
// real chunk would look several times slower than normal. That reads as
// congestion, and the limiter would cut concurrency on an origin that is
// perfectly healthy, on every transfer with a cache hit in it.
//
// The mix here is deliberate and so is the duration: the baseline only seeds
// after a bucket's worth of wall time and a warmup's worth of samples, so a
// faster test could not tell a poisoned baseline from an unseeded one.
func TestUntimedCompletionsStayOutOfTheBaseline(t *testing.T) {
	l := defaultLimiter(volume.Concurrency{})

	const realLatency = 8 * time.Millisecond
	deadline := time.Now().Add(1300 * time.Millisecond)
	for time.Now().Before(deadline) {
		// Two instant completions per real one: dedup-heavy, which is the
		// ordinary case for a re-push or a resumed download.
		feed(t, l, 2, false)
		feedSlow(t, l, realLatency)
	}
	// Once the baseline has seeded, the reading that matters is what the next
	// real transfers do to the limit — not how far it climbed getting here,
	// which is large either way and would hide the cut.
	seeded := holdCapacity(t, l)
	for range 5 {
		feedSlow(t, l, realLatency)
	}
	after := holdCapacity(t, l)

	// A soft stall cuts by a factor of 0.7, so anything close to the seeded
	// limit is ordinary movement and anything near or below three quarters of
	// it is the cut this test exists to catch.
	if after*10 < seeded*9 {
		t.Errorf("concurrency fell from %d to %d against a healthy origin: "+
			"instant completions reached the latency baseline, so real transfers looked inflated",
			seeded, after)
	}
}

// TestPinnedConcurrencyAdmitsExactlyN pins the contract a pinned
// Concurrency makes to a caller, and deliberately says nothing about which
// limiter serves it.
//
// The promise is about admission, not about a configured number: a caller who
// pins n does so to cap the load a transfer puts on a shared machine or a
// metered link, so what has to hold is that no more than n operations are ever
// in flight at once — and that it stays true under the completions that move an
// adaptive limiter, since a pinned one must not be moved by them.
//
// Written against defaultLimiter rather than against a constructor, so it
// survives the pinned arm being reimplemented. If pinned mode is ever served by
// the adaptive limiter with a coincident floor and ceiling, or by anything
// else, this test keeps guarding the contract without being rewritten. That is
// the point of it: the mechanism is an implementation detail, the cap is not.
func TestPinnedConcurrencyAdmitsExactlyN(t *testing.T) {
	const (
		pinned  = 4
		workers = 60
	)
	l := defaultLimiter(volume.Concurrency{ChunkOperations: pinned})

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
		wg       sync.WaitGroup
	)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			permit, err := l.Acquire(context.Background())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()

			// Rotate through every outcome a real transfer produces. Success
			// is what grows an adaptive limiter and a stall is what shrinks
			// one; a pinned limiter must be moved by neither.
			switch i % 3 {
			case 0:
				permit.Complete(volume.Success)
			case 1:
				permit.Complete(volume.Stall)
			default:
				permit.CompleteUntimed(volume.Neutral)
			}
		}(i)
	}
	wg.Wait()

	if peak != pinned {
		t.Errorf("peak concurrent holders %d, want exactly %d", peak, pinned)
	}
	// And the cap still holds afterwards, so nothing in that mix moved it.
	if got := holdCapacity(t, l); got != pinned {
		t.Errorf("capacity after the run %d, want %d", got, pinned)
	}
}
