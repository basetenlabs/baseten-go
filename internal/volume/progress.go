package volume

import "sync"

// Phase names what a transfer is doing. Totals reset when the phase changes,
// so a caller rendering a bar starts a new one rather than watching the old
// one jump backwards.
type Phase string

const (
	PhaseScan     Phase = "scan"
	PhaseUpload   Phase = "upload"
	PhaseCommit   Phase = "commit"
	PhaseResolve  Phase = "resolve"
	PhaseDownload Phase = "download"
	PhasePublish  Phase = "publish"
)

// Progress is a transfer's state at one moment. Counts within a phase only
// ever increase.
type Progress struct {
	Phase Phase

	Files      int64
	TotalFiles int64

	Bytes      int64
	TotalBytes int64
}

// ProgressFunc receives progress updates. It is called from whichever
// goroutine made progress, but never concurrently with itself, so an
// implementation needs no locking of its own. It should return quickly: a slow
// callback holds up the transfer.
type ProgressFunc func(Progress)

// ProgressReporter serializes progress updates onto a callback. A nil callback
// makes every method a no-op, so callers need not check.
type ProgressReporter struct {
	mu    sync.Mutex
	fn    ProgressFunc
	state Progress
}

// NewProgressReporter returns a reporter feeding fn, which may be nil.
func NewProgressReporter(fn ProgressFunc) *ProgressReporter {
	return &ProgressReporter{fn: fn}
}

// SetPhase moves to a new phase with fresh totals and zeroed counts.
func (r *ProgressReporter) SetPhase(phase Phase, totalFiles, totalBytes int64) {
	if r == nil || r.fn == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = Progress{Phase: phase, TotalFiles: totalFiles, TotalBytes: totalBytes}
	r.fn(r.state)
}

// Add records progress within the current phase.
func (r *ProgressReporter) Add(files, bytes int64) {
	if r == nil || r.fn == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Files += files
	r.state.Bytes += bytes
	r.fn(r.state)
}
