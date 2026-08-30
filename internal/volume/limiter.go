package volume

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Outcome is what a finished operation tells the limiter about the origin's
// health. The three values are not degrees of success: they say whether the
// operation carried usable evidence about capacity.
type Outcome int

const (
	// Neutral carries no evidence. An operation that made no request — a
	// deduplicated chunk — and a request retried past a connection the peer
	// had already closed are both Neutral. The second is the one worth being
	// careful about: connection reuse churn is a property of pooling, not of
	// the origin being busy, and counting it as backpressure holds an adaptive
	// limiter far below the concurrency the origin would have served.
	Neutral Outcome = iota

	// Success is a request that completed without the origin pushing back.
	Success

	// Stall is the origin asking for less: a 5xx or 429, a connect failure, a
	// timeout, or a local resource exhaustion, whether or not a retry
	// eventually succeeded.
	Stall
)

// Limiter governs how many object operations run at once. The interface is
// the one an adaptive limiter needs — a permit that reports how its operation
// went, so the limit can move with the origin — rather than the smaller one a
// fixed pool would need.
type Limiter interface {
	// Acquire blocks until a slot is free or ctx is done.
	Acquire(ctx context.Context) (*Permit, error)
}

// Permit is one in-flight operation's slot. Exactly one Complete call must
// follow a successful Acquire.
type Permit struct {
	release func(Outcome, time.Duration)
	done    bool
	start   time.Time
}

// NewPermit builds a permit that calls release when completed. It exists so an
// implementation outside this package can satisfy Limiter.
func NewPermit(release func(Outcome, time.Duration)) *Permit {
	return &Permit{release: release, start: time.Now()}
}

// Complete returns the permit, reporting what the operation observed. The
// elapsed time is measured from Acquire.
func (p *Permit) Complete(outcome Outcome) {
	p.complete(outcome, time.Since(p.start))
}

// CompleteUntimed returns the permit without contributing a latency sample.
// Metadata operations use it: a chunkmap upload is a different size and shape
// from a chunk, and letting its latency into the sample would move the
// baseline an adaptive limiter measures inflation against.
func (p *Permit) CompleteUntimed(outcome Outcome) {
	p.complete(outcome, 0)
}

func (p *Permit) complete(outcome Outcome, elapsed time.Duration) {
	if p == nil || p.done {
		return
	}
	p.done = true
	p.release(outcome, elapsed)
}

// semaphoreLimiter is a fixed number of slots that never moves. It cannot find
// the origin's capacity, only stay under a number someone chose, which is
// exactly why it is what a caller who pinned that number gets: an adaptive
// limiter would climb off the cap they asked for.
type semaphoreLimiter struct {
	slots chan struct{}
}

// NewSemaphoreLimiter returns a Limiter allowing capacity concurrent
// operations.
func NewSemaphoreLimiter(capacity int) Limiter {
	if capacity < 1 {
		capacity = 1
	}
	return &semaphoreLimiter{slots: make(chan struct{}, capacity)}
}

func (l *semaphoreLimiter) Acquire(ctx context.Context) (*Permit, error) {
	select {
	case l.slots <- struct{}{}:
		return NewPermit(func(Outcome, time.Duration) { <-l.slots }), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ByteGate caps how many bytes of chunk data are resident at once,
// independently of how many operations are in flight. The two limits are
// separate because they protect different things: the limiter protects the
// origin, this protects the process. A limit high enough to saturate a fast
// link is also high enough to exhaust a small container's memory.
//
// It is always acquired after a Limiter slot. One order, consistently, is what
// keeps the pair from deadlocking; taking bytes second also means a stalled
// origin backpressures the file reads rather than filling memory with data
// nothing is ready to send.
//
// It is a fixed budget and stays one even when the Limiter beside it adapts.
// The two answer different questions: how much the origin will bear is
// discovered by watching the origin, while how much memory this process may
// use is a fact about the machine that no amount of watching the network
// reveals. An adaptive byte budget would grow on exactly the evidence — a fast,
// healthy origin — that says nothing about whether the container has the
// memory to match.
type ByteGate struct {
	mu        sync.Mutex
	available chan struct{}
	limit     int64
	inFlight  int64
}

// ErrByteBudget reports a request that could never fit the byte budget.
var ErrByteBudget = errors.New("request is larger than the whole byte budget")

// NewByteGate returns a gate admitting at most limit bytes at once. A limit
// below one chunk is raised to it, since a single chunk must always fit.
func NewByteGate(limit int64) *ByteGate {
	if limit < ChunkSize {
		limit = ChunkSize
	}
	return &ByteGate{available: make(chan struct{}, 1), limit: limit}
}

// Acquire blocks until n bytes fit within the budget. A single request larger
// than the whole budget would never fit, so it is an error rather than a hang.
func (g *ByteGate) Acquire(ctx context.Context, n int64) error {
	if n > g.limit {
		return ErrByteBudget
	}
	for {
		g.mu.Lock()
		if g.inFlight+n <= g.limit {
			g.inFlight += n
			g.mu.Unlock()
			return nil
		}
		g.mu.Unlock()

		// Wait for a release rather than spinning. A wakeup does not
		// guarantee room — another waiter may take it first — so the loop
		// re-checks.
		select {
		case <-g.available:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Release returns n bytes to the budget.
func (g *ByteGate) Release(n int64) {
	g.mu.Lock()
	g.inFlight -= n
	g.mu.Unlock()
	select {
	case g.available <- struct{}{}:
	default:
	}
}

// Concurrency tunes how much of a transfer runs at once. A zero field takes
// the default.
type Concurrency struct {
	// FileJobs is how many files are processed concurrently on push.
	FileJobs int

	// ChunkOperations pins how many object operations may be in flight. Zero
	// does not mean "some default": it means the limit is not pinned, and a
	// transfer adapts it to what the origin will bear. Setting it caps the
	// load a transfer places on a shared machine or a metered link, and is
	// honoured exactly.
	ChunkOperations int

	// MaxBytesInFlight caps the chunk data resident in memory.
	MaxBytesInFlight int64
}

// Concurrency defaults.
const (
	DefaultFileJobs         = 16
	DefaultMaxBytesInFlight = 2 << 30
)

// WithDefaults fills in the fields that have a default.
//
// ChunkOperations is not one of them, and is deliberately left at zero. There
// is no number here that would be right: one chosen for a fast link overwhelms
// a slow one, and one chosen for a slow link wastes a fast one. Zero is the
// signal to adapt instead of guessing, so filling it in would erase the
// distinction the caller was making.
func (c Concurrency) WithDefaults() Concurrency {
	if c.FileJobs <= 0 {
		c.FileJobs = DefaultFileJobs
	}
	if c.MaxBytesInFlight <= 0 {
		c.MaxBytesInFlight = DefaultMaxBytesInFlight
	}
	return c
}
