package transfer

import (
	"context"
	"time"

	"github.com/basetenlabs/baseten-go/internal/volume"
	"github.com/basetenlabs/baseten-go/internal/volume/limiter"
)

// maxAdaptiveConcurrency is the ceiling the adaptive limiter may climb to. It
// is a guard on sockets and file descriptors rather than a target: the limiter
// is expected to settle well below it, and nothing tries to reach it.
const maxAdaptiveConcurrency = 512

// defaultLimiter chooses how concurrency is governed when the caller supplies
// no Limiter of their own.
//
// A caller who set Concurrency.ChunkOperations has named a number, and gets
// exactly that: a fixed pool of that size. Adapting away from it would defeat
// the reason the option exists — capping the load a transfer puts on a shared
// machine, a metered link, or an origin someone else is also using — so a
// limiter that grew past it would be the wrong kind of helpful.
//
// Everyone else gets the adaptive limiter, which finds the origin's capacity
// rather than guessing at it. No fixed default can: a number chosen for a fast
// link overwhelms a slow one, and a number chosen for a slow link leaves most
// of a fast one unused.
func defaultLimiter(c volume.Concurrency) volume.Limiter {
	if c.ChunkOperations > 0 {
		return volume.NewSemaphoreLimiter(c.ChunkOperations)
	}
	return newAdaptiveLimiter(limiter.DataPathLimiterConfig(maxAdaptiveConcurrency))
}

// adaptiveLimiter adapts the vendored AIMD limiter to the volume.Limiter seam.
//
// One limiter serves one transfer. The vendored package hands out limiters
// through a manager that keys them by host, which is what a long-lived process
// talking to many origins needs; a single push or pull talks to one origin for
// its whole life, so a per-run limiter is both the simpler fit and the more
// honest one — its state is exactly as old as the transfer it is steering,
// with nothing carried in from a previous run against a different link.
//
// The manager is used with one fixed key rather than reaching for a limiter
// directly, because the vendored package exposes no single-limiter
// constructor. Going through it costs a map lookup per acquire, against an
// operation that moves megabytes; adding a constructor would mean editing the
// file whose whole value is that it was not edited.
type adaptiveLimiter struct {
	inner *limiter.AdaptiveLimiterManager
}

// oneOrigin is the manager key. Every acquire in a run uses it, which is what
// makes the manager hand back one limiter for the whole transfer.
const oneOrigin = "origin"

func newAdaptiveLimiter(cfg limiter.AdaptiveLimiterConfig) volume.Limiter {
	return &adaptiveLimiter{inner: limiter.NewAdaptiveLimiterManager(cfg)}
}

func (a *adaptiveLimiter) Acquire(ctx context.Context) (*volume.Permit, error) {
	permit, err := a.inner.Acquire(ctx, oneOrigin)
	if err != nil {
		return nil, err
	}
	return volume.NewPermit(func(outcome volume.Outcome, elapsed time.Duration) {
		// An untimed completion arrives with a zero duration and must not feed
		// the latency baseline the soft-stall detector measures inflation
		// against. A deduplicated chunk returns instantly and a metadata
		// request is a different size and shape from a chunk; either one in
		// the sample drags the baseline down, which makes ordinary transfers
		// look inflated and holds the limit below what the origin would serve.
		if elapsed <= 0 {
			permit.Complete(adaptiveOutcome(outcome))
			return
		}
		permit.CompleteWithLatency(adaptiveOutcome(outcome), elapsed)
	}), nil
}

// adaptiveOutcome translates the seam's outcome into the vendored limiter's.
// The two enumerations mean the same three things and are deliberately not
// shared: the seam is what the engines speak, and the vendored package is kept
// free of anything this repository invented.
func adaptiveOutcome(o volume.Outcome) limiter.Outcome {
	switch o {
	case volume.Success:
		return limiter.OutcomeSuccess
	case volume.Stall:
		return limiter.OutcomeStall
	default:
		return limiter.OutcomeNeutral
	}
}
