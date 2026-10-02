package sandbox

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-go/client/sandboxapi"
)

// ProcessService runs and inspects processes in a sandbox. Get it from
// [Sandbox.Process].
type ProcessService struct {
	sandbox *Sandbox
}

func newProcessService(sandbox *Sandbox) *ProcessService {
	return &ProcessService{sandbox: sandbox}
}

// ProcessStatus is the status of a process in a sandbox. Other values may be
// added, so do not treat the constants as exhaustive.
type ProcessStatus string

// Values for ProcessStatus.
const (
	ProcessStatusRunning   ProcessStatus = "running"
	ProcessStatusCompleted ProcessStatus = "completed"
	ProcessStatusFailed    ProcessStatus = "failed"
	ProcessStatusKilled    ProcessStatus = "killed"
	ProcessStatusStopped   ProcessStatus = "stopped"
)

// ProcessInfo is the state of a process in a sandbox.
type ProcessInfo struct {
	// PID is the process ID.
	PID string

	// Name is the process's name, if it was given one.
	Name string

	// Command is the command the process runs.
	Command string

	// Status is the process's status.
	Status ProcessStatus

	// ExitCode is the process's exit code, once it has exited.
	ExitCode int

	// Stdout is the process's standard output so far.
	Stdout string

	// Stderr is the process's standard error so far.
	Stderr string

	// Logs is the process's standard output and standard error so far,
	// interleaved.
	Logs string

	// WorkingDir is the directory the process runs in.
	WorkingDir string

	// StartedAt is when the process started.
	StartedAt time.Time

	// CompletedAt is when the process exited, zero while it runs.
	CompletedAt time.Time

	// KeepAlive is whether the process keeps the sandbox from going idle.
	KeepAlive bool

	// MaxRestarts is how many times the process is restarted on failure.
	MaxRestarts int

	// RestartCount is how many times the process has been restarted.
	RestartCount int

	// RestartOnFailure is whether the process is restarted when it fails.
	RestartOnFailure bool

	// Stdin is whether the process accepts standard input.
	Stdin bool
}

// ProcessLogs is a process's captured output.
type ProcessLogs struct {
	// Stdout is the process's standard output.
	Stdout string

	// Stderr is the process's standard error.
	Stderr string

	// Logs is the process's standard output and standard error,
	// interleaved.
	Logs string
}

// ProcessExecOptions are the options for [ProcessService.Exec].
type ProcessExecOptions struct {
	// Command is the shell command to run. Required.
	Command string

	// WorkingDir is the directory to run the command in. Empty uses the
	// sandbox's default.
	WorkingDir string

	// Env are environment variables for the process, on top of the
	// sandbox's own.
	Env map[string]string

	// Name names the process, so it can be referred to by name instead of
	// its PID.
	Name string

	// Timeout kills the process after this long, rounded up to whole
	// seconds. Zero means never.
	Timeout time.Duration

	// KeepAlive keeps the sandbox from going idle while the process runs.
	KeepAlive bool

	// RestartOnFailure restarts the process when it fails.
	RestartOnFailure bool

	// MaxRestarts is how many times the process is restarted on failure.
	MaxRestarts int

	// Stdin keeps the process's standard input open for writing.
	Stdin bool

	// WaitForPorts returns only once the process listens on these ports.
	WaitForPorts []int

	// WaitForCompletion returns only once the process has exited. When
	// false, the process keeps running in the background.
	WaitForCompletion bool
}

// Exec starts a process and returns its state.
func (p *ProcessService) Exec(ctx context.Context, opts ProcessExecOptions) (*ProcessInfo, error) {
	if opts.Command == "" {
		return nil, errors.New("Command is required")
	}
	request := processRequestToAPI(ProcessExecStreamOptions{
		Command:          opts.Command,
		WorkingDir:       opts.WorkingDir,
		Env:              opts.Env,
		Name:             opts.Name,
		Timeout:          opts.Timeout,
		KeepAlive:        opts.KeepAlive,
		RestartOnFailure: opts.RestartOnFailure,
		MaxRestarts:      opts.MaxRestarts,
		Stdin:            opts.Stdin,
		WaitForPorts:     opts.WaitForPorts,
	})
	request.WaitForCompletion = optional(opts.WaitForCompletion)
	// Not retried, since a command may have side effects even when its
	// response is lost.
	response, err := p.sandbox.api.PostProcess(ctx, request)
	if err != nil {
		return nil, sandboxError(err)
	}
	info, err := processInfoFromAPI(response)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// ProcessExecStreamOptions are the options for [ProcessService.ExecStream].
type ProcessExecStreamOptions struct {
	// Command is the shell command to run. Required.
	Command string

	// WorkingDir is the directory to run the command in. Empty uses the
	// sandbox's default.
	WorkingDir string

	// Env are environment variables for the process, on top of the
	// sandbox's own.
	Env map[string]string

	// Name names the process, so it can be referred to by name instead of
	// its PID.
	Name string

	// Timeout kills the process after this long, rounded up to whole
	// seconds. Zero means never.
	Timeout time.Duration

	// KeepAlive keeps the sandbox from going idle while the process runs.
	KeepAlive bool

	// RestartOnFailure restarts the process when it fails.
	RestartOnFailure bool

	// MaxRestarts is how many times the process is restarted on failure.
	MaxRestarts int

	// Stdin keeps the process's standard input open for writing.
	Stdin bool

	// WaitForPorts starts streaming only once the process listens on these
	// ports.
	WaitForPorts []int
}

// ProcessStream is which output of a process some text came from. Other
// values may be added, so do not treat the constants as exhaustive.
type ProcessStream string

// Values for ProcessStream.
const (
	ProcessStreamStdout ProcessStream = "stdout"
	ProcessStreamStderr ProcessStream = "stderr"
)

// ProcessExecEvent is one event of [ProcessService.ExecStream]: either output,
// with TextStream and Text set, or, last, the process's exit, with Exit set.
type ProcessExecEvent struct {
	// TextStream is which output Text came from.
	TextStream ProcessStream

	// Text is a chunk of output exactly as written: not split into lines,
	// so output such as a prompt arrives without waiting for a newline. It
	// may be empty when the sandbox sends an empty chunk.
	Text string

	// Exit is the process's final state, set on the last event only.
	Exit *ProcessInfo
}

// ExecStream starts a process and yields its output as it is written, then
// its final state. An error is yielded as the second value and ends the
// iteration. Stopping the iteration early leaves the process running.
func (p *ProcessService) ExecStream(ctx context.Context, opts ProcessExecStreamOptions) iter.Seq2[ProcessExecEvent, error] {
	return func(yield func(ProcessExecEvent, error) bool) {
		if opts.Command == "" {
			yield(ProcessExecEvent{}, errors.New("Command is required"))
			return
		}
		request := processRequestToAPI(opts)
		request.WaitForCompletion = optional(true)
		// The sandbox streams only when asked for text/event-stream, though
		// what it sends is newline-delimited JSON. Not retried, since a
		// command may have side effects even when its response is lost.
		response, err := p.sandbox.api.PostProcessRaw(ctx, request)
		if err != nil {
			yield(ProcessExecEvent{}, sandboxError(err))
			return
		}
		defer response.Body.Close()
		scanner := bufio.NewScanner(response.Body)
		// One record per line, and a record can carry any amount of output.
		scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			var record struct {
				Type string `json:"type"`
				Data string `json:"data"`
			}
			if err := json.Unmarshal(line, &record); err != nil {
				yield(ProcessExecEvent{}, fmt.Errorf("process stream sent an unreadable record: %w", err))
				return
			}
			switch record.Type {
			case "stdout", "stderr":
				if !yield(ProcessExecEvent{TextStream: ProcessStream(record.Type), Text: record.Data}, nil) {
					return
				}
			case "result":
				var final sandboxapi.ProcessResponse
				if err := json.Unmarshal([]byte(record.Data), &final); err != nil {
					yield(ProcessExecEvent{}, fmt.Errorf("process stream sent an unreadable result: %w", err))
					return
				}
				info, err := processInfoFromAPI(&final)
				if err != nil {
					yield(ProcessExecEvent{}, err)
					return
				}
				yield(ProcessExecEvent{Exit: &info}, nil)
				return
			}
			// Other record types, such as keepalives, carry no output.
		}
		if err := scanner.Err(); err != nil {
			yield(ProcessExecEvent{}, err)
			return
		}
		yield(ProcessExecEvent{}, errors.New("process stream ended before reporting the process's exit"))
	}
}

// ProcessListOptions are the options for [ProcessService.List].
type ProcessListOptions struct{}

// List returns the sandbox's processes.
func (p *ProcessService) List(ctx context.Context, opts ProcessListOptions) ([]ProcessInfo, error) {
	processes, err := retryIdempotent(ctx, p.sandbox.retries.ReadMaxRetries, p.sandbox.retries.GatewayMaxRetries,
		func() (*sandboxapi.GetProcessResponse, error) {
			processes, err := p.sandbox.api.GetProcess(ctx)
			return processes, sandboxError(err)
		})
	if err != nil {
		return nil, err
	}
	infos := make([]ProcessInfo, 0, len(*processes))
	for i := range *processes {
		info, err := processInfoFromAPI(&(*processes)[i])
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// ProcessLogsOptions are the options for [ProcessService.Logs].
type ProcessLogsOptions struct {
	// Identifier is the process's PID or name. Required.
	Identifier string
}

// Logs returns a process's captured output.
func (p *ProcessService) Logs(ctx context.Context, opts ProcessLogsOptions) (*ProcessLogs, error) {
	if opts.Identifier == "" {
		return nil, errors.New("Identifier is required")
	}
	logs, err := retryIdempotent(ctx, p.sandbox.retries.ReadMaxRetries, p.sandbox.retries.GatewayMaxRetries,
		func() (*sandboxapi.ProcessLogs, error) {
			logs, err := p.sandbox.api.GetProcessLogs(ctx, opts.Identifier)
			return logs, sandboxError(err)
		})
	if err != nil {
		return nil, err
	}
	return &ProcessLogs{Stdout: logs.Stdout, Stderr: logs.Stderr, Logs: logs.Logs}, nil
}

// ProcessGetOptions are the options for [ProcessService.Get].
type ProcessGetOptions struct {
	// Identifier is the process's PID or name. Required.
	Identifier string
}

// Get returns a process's state.
func (p *ProcessService) Get(ctx context.Context, opts ProcessGetOptions) (*ProcessInfo, error) {
	if opts.Identifier == "" {
		return nil, errors.New("Identifier is required")
	}
	process, err := retryIdempotent(ctx, p.sandbox.retries.ReadMaxRetries, p.sandbox.retries.GatewayMaxRetries,
		func() (*sandboxapi.ProcessResponse, error) {
			process, err := p.sandbox.api.GetProcessIdentifier(ctx, opts.Identifier)
			return process, sandboxError(err)
		})
	if err != nil {
		return nil, err
	}
	info, err := processInfoFromAPI(process)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

const (
	defaultProcessWaitTimeout      = 60 * time.Second
	defaultProcessWaitPollInterval = time.Second
)

// ProcessWaitOptions are the options for [ProcessService.Wait].
type ProcessWaitOptions struct {
	// Identifier is the process's PID or name. Required.
	Identifier string

	// Timeout is how long to wait. Zero uses 60 seconds, and a negative
	// value waits with no limit. The process keeps running regardless.
	Timeout time.Duration

	// PollInterval is how often to check the process. Zero uses 1 second.
	PollInterval time.Duration
}

// Wait waits for a process to exit and returns its final state, whether it
// completed, failed, was killed, or was stopped. A failed process is returned,
// not an error. If the wait times out, it returns a [*ProcessWaitTimeoutError]
// and the process keeps running.
func (p *ProcessService) Wait(ctx context.Context, opts ProcessWaitOptions) (*ProcessInfo, error) {
	if opts.Identifier == "" {
		return nil, errors.New("Identifier is required")
	}
	timeout := cmp.Or(opts.Timeout, defaultProcessWaitTimeout)
	pollInterval := cmp.Or(opts.PollInterval, defaultProcessWaitPollInterval)
	waitCtx, cancel := ctx, context.CancelFunc(func() {})
	if timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	var lastErr error
	for {
		process, err := p.sandbox.api.GetProcessIdentifier(waitCtx, opts.Identifier)
		if err == nil {
			lastErr = nil
			switch ProcessStatus(process.Status) {
			case ProcessStatusCompleted, ProcessStatusFailed, ProcessStatusKilled, ProcessStatusStopped:
				info, err := processInfoFromAPI(process)
				if err != nil {
					return nil, err
				}
				return &info, nil
			case ProcessStatusRunning:
			default:
				// Unlike elsewhere, an unknown status is an error rather than
				// waited on, keeping existing behavior.
				return nil, fmt.Errorf("process %s has unknown status %s", opts.Identifier, process.Status)
			}
		} else if waitCtx.Err() == nil {
			// An error from the wait's own end is not the last poll's error,
			// so it is left for the timeout handling below.
			if err = sandboxError(err); !isRetryableProcessWaitError(err) {
				return nil, err
			}
			lastErr = err
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &ProcessWaitTimeoutError{Identifier: opts.Identifier, Err: lastErr}
		case <-timer.C:
		}
	}
}

// isRetryableProcessWaitError reports whether a wait should ride out an
// error: a dropped connection, or a response the sandbox may answer
// differently on the next poll.
func isRetryableProcessWaitError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	return isTransientResetError(err)
}

// ProcessStopOptions are the options for [ProcessService.Stop].
type ProcessStopOptions struct {
	// Identifier is the process's PID or name. Required.
	Identifier string
}

// Stop asks a process to exit gracefully. It then ends with
// [ProcessStatusStopped].
func (p *ProcessService) Stop(ctx context.Context, opts ProcessStopOptions) error {
	if opts.Identifier == "" {
		return errors.New("Identifier is required")
	}
	// Not retried, since a lost response cannot tell whether it stopped.
	_, err := p.sandbox.api.DeleteProcess(ctx, opts.Identifier)
	return sandboxError(err)
}

// ProcessKillOptions are the options for [ProcessService.Kill].
type ProcessKillOptions struct {
	// Identifier is the process's PID or name. Required.
	Identifier string
}

// Kill kills a process. It then ends with [ProcessStatusKilled].
func (p *ProcessService) Kill(ctx context.Context, opts ProcessKillOptions) error {
	if opts.Identifier == "" {
		return errors.New("Identifier is required")
	}
	// Not retried, since a lost response cannot tell whether it was killed.
	_, err := p.sandbox.api.DeleteProcessKill(ctx, opts.Identifier)
	return sandboxError(err)
}

// ProcessWriteStdinOptions are the options for [ProcessService.WriteStdin].
type ProcessWriteStdinOptions struct {
	// Identifier is the process's PID or name, which must have been started
	// with [ProcessExecOptions.Stdin]. Required.
	Identifier string

	// Data is written to the process's standard input as is. Include any
	// trailing newline the process expects.
	Data []byte
}

// WriteStdin writes to a process's standard input.
func (p *ProcessService) WriteStdin(ctx context.Context, opts ProcessWriteStdinOptions) error {
	if opts.Identifier == "" {
		return errors.New("Identifier is required")
	}
	// Not retried, since a repeat could write the bytes twice.
	_, err := p.sandbox.api.PostProcessStdin(ctx, opts.Identifier, sandboxapi.AdvancedRequest{Body: bytes.NewReader(opts.Data)})
	return sandboxError(err)
}

// ProcessCloseStdinOptions are the options for [ProcessService.CloseStdin].
type ProcessCloseStdinOptions struct {
	// Identifier is the process's PID or name. Required.
	Identifier string
}

// CloseStdin closes a process's standard input, so it reads end of input.
func (p *ProcessService) CloseStdin(ctx context.Context, opts ProcessCloseStdinOptions) error {
	if opts.Identifier == "" {
		return errors.New("Identifier is required")
	}
	// Retried, since closing an already closed input changes nothing.
	_, err := retryIdempotent(ctx, p.sandbox.retries.ReadMaxRetries, p.sandbox.retries.GatewayMaxRetries,
		func() (*sandboxapi.SuccessResponse, error) {
			response, err := p.sandbox.api.DeleteProcessStdin(ctx, opts.Identifier)
			return response, sandboxError(err)
		})
	return err
}

// ProcessStreamLogsOptions are the options for [ProcessService.StreamLogs].
type ProcessStreamLogsOptions struct {
	// Identifier is the process's PID or name. Required.
	Identifier string
}

// ProcessLogLine is one line of a process's output.
type ProcessLogLine struct {
	// TextStream is which output Text came from, empty when the sandbox did
	// not report it.
	TextStream ProcessStream

	// Text is the line, without its line ending. It may be empty, for an
	// empty line.
	Text string
}

// StreamLogs yields a process's output line by line, from the start, until
// the process exits. An error is yielded as the second value and ends the
// iteration. Stopping the iteration early leaves the process running.
func (p *ProcessService) StreamLogs(ctx context.Context, opts ProcessStreamLogsOptions) iter.Seq2[ProcessLogLine, error] {
	return func(yield func(ProcessLogLine, error) bool) {
		if opts.Identifier == "" {
			yield(ProcessLogLine{}, errors.New("Identifier is required"))
			return
		}
		// Not retried, since a resent stream would repeat lines already
		// yielded.
		response, err := p.sandbox.api.GetProcessLogsStreamRaw(ctx, opts.Identifier)
		if err != nil {
			yield(ProcessLogLine{}, sandboxError(err))
			return
		}
		defer response.Body.Close()
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSuffix(scanner.Text(), "\r")
			if strings.HasPrefix(line, "[keepalive]") {
				continue
			}
			var logLine ProcessLogLine
			if text, ok := strings.CutPrefix(line, "stdout:"); ok {
				logLine = ProcessLogLine{TextStream: ProcessStreamStdout, Text: text}
			} else if text, ok := strings.CutPrefix(line, "stderr:"); ok {
				logLine = ProcessLogLine{TextStream: ProcessStreamStderr, Text: text}
			} else {
				logLine = ProcessLogLine{Text: line}
			}
			if !yield(logLine, nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			yield(ProcessLogLine{}, err)
		}
	}
}

func processRequestToAPI(opts ProcessExecStreamOptions) sandboxapi.ProcessRequest {
	request := sandboxapi.ProcessRequest{
		Command:          opts.Command,
		WorkingDir:       optional(opts.WorkingDir),
		Env:              optionalMap(opts.Env),
		Name:             optional(opts.Name),
		KeepAlive:        optional(opts.KeepAlive),
		RestartOnFailure: optional(opts.RestartOnFailure),
		MaxRestarts:      optional(opts.MaxRestarts),
		Stdin:            optional(opts.Stdin),
		WaitForPorts:     optionalSlice(opts.WaitForPorts),
	}
	if opts.Timeout > 0 {
		request.Timeout = optional(int((opts.Timeout + time.Second - 1) / time.Second))
	}
	return request
}

func processInfoFromAPI(process *sandboxapi.ProcessResponse) (ProcessInfo, error) {
	startedAt, err := parseTimestamp(process.StartedAt, "process start time")
	if err != nil {
		return ProcessInfo{}, err
	}
	completedAt, err := parseTimestamp(process.CompletedAt, "process completion time")
	if err != nil {
		return ProcessInfo{}, err
	}
	return ProcessInfo{
		PID:              process.Pid,
		Name:             process.Name,
		Command:          process.Command,
		Status:           ProcessStatus(process.Status),
		ExitCode:         process.ExitCode,
		Stdout:           process.Stdout,
		Stderr:           process.Stderr,
		Logs:             process.Logs,
		WorkingDir:       process.WorkingDir,
		StartedAt:        startedAt,
		CompletedAt:      completedAt,
		KeepAlive:        deref(process.KeepAlive),
		MaxRestarts:      deref(process.MaxRestarts),
		RestartCount:     deref(process.RestartCount),
		RestartOnFailure: deref(process.RestartOnFailure),
		Stdin:            deref(process.Stdin),
	}, nil
}
