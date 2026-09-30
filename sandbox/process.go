package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"iter"

	"github.com/basetenlabs/baseten-go/client/sandboxapi"
)

// ExecOptions runs one command in a sandbox.
type ExecOptions struct {
	// Command is the shell command to run.
	Command string

	// WorkingDir is the directory to run the command in. Empty uses the
	// sandbox's default.
	WorkingDir string

	// Env holds environment variables for the process, on top of the
	// sandbox's own.
	Env map[string]string

	// Name refers to the process by name, instead of its pid.
	Name string

	// TimeoutSeconds is after which the process is killed. Zero means
	// never.
	TimeoutSeconds int

	// WaitForCompletion returns only once the process has exited. When
	// false, the process keeps running in the background.
	WaitForCompletion bool
}

// acceptEventStream is what the execution API requires to stream a command's
// output, even though it answers with newline-delimited JSON.
const acceptEventStream = "text/event-stream"

// SandboxProcess runs and inspects processes in a sandbox.
type SandboxProcess struct {
	api *sandboxapi.Client
}

// Exec starts a command and returns its state.
//
// Exec is never retried: a command may have side effects even when its
// response is lost.
func (p *SandboxProcess) Exec(ctx context.Context, opts *ExecOptions) (*ProcessInfo, error) {
	response, err := p.api.PostProcess(ctx, execRequest(opts, opts.WaitForCompletion))
	if err != nil {
		return nil, toSandboxAPIError(err, "exec")
	}
	info, err := processInfoFromAPI(response)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// ExecStream starts a command and yields its state as output arrives, ending
// with the final state of the exited process. An error yields as the pair's
// second value and ends the iteration.
func (p *SandboxProcess) ExecStream(ctx context.Context, opts *ExecOptions) iter.Seq2[ProcessInfo, error] {
	return func(yield func(ProcessInfo, error) bool) {
		response, err := p.api.PostProcessRaw(ctx, execRequest(opts, true), sandboxapi.RawRequestOptions{Accept: acceptEventStream})
		if err != nil {
			yield(ProcessInfo{}, toSandboxAPIError(err, "exec"))
			return
		}
		defer response.Body.Close()
		scanner := bufio.NewScanner(response.Body)
		// One event per line, and a line can carry arbitrarily much output.
		scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			var event sandboxapi.ProcessResponse
			if err := json.Unmarshal(line, &event); err != nil {
				yield(ProcessInfo{}, err)
				return
			}
			info, err := processInfoFromAPI(&event)
			if err != nil {
				yield(ProcessInfo{}, err)
				return
			}
			if !yield(info, nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			yield(ProcessInfo{}, err)
		}
	}
}

func execRequest(opts *ExecOptions, waitForCompletion bool) sandboxapi.ProcessRequest {
	request := sandboxapi.ProcessRequest{
		Command:           opts.Command,
		WaitForCompletion: &waitForCompletion,
	}
	if opts.WorkingDir != "" {
		request.WorkingDir = &opts.WorkingDir
	}
	if len(opts.Env) > 0 {
		env := opts.Env
		request.Env = &env
	}
	if opts.Name != "" {
		request.Name = &opts.Name
	}
	if opts.TimeoutSeconds > 0 {
		request.Timeout = &opts.TimeoutSeconds
	}
	return request
}
