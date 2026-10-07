package sandbox

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/client/sandboxapi"
	"github.com/basetenlabs/baseten-go/internal/require"
)

const processRecord = `{"pid":"12","name":"","command":"ls","status":"completed","exitCode":0,` +
	`"startedAt":"2026-10-01T00:00:00Z","completedAt":"2026-10-01T00:00:01Z",` +
	`"stdout":"out","stderr":"","logs":"out","workingDir":"/"}`

// execRequest is what the fake execution API saw of the last request.
type execRequest struct {
	path          string
	accept        string
	authorization string
	body          string
}

// execSandbox returns a Sandbox whose execution API is handler, authenticated
// through a fake control plane, and a func returning the last request
// handler got. Assertions belong in the test, not in handler, which runs on
// the server's goroutine.
func execSandbox(t *testing.T, handler http.HandlerFunc) (*Sandbox, func() execRequest) {
	t.Helper()
	var mu sync.Mutex
	var last execRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Left readable for handler.
		r.Body = io.NopCloser(bytes.NewReader(body))
		mu.Lock()
		last = execRequest{r.URL.Path, r.Header.Get("Accept"), r.Header.Get("Authorization"), string(body)}
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	sb, err := newControlPlane(t).client(t, ClientOptions{}).SandboxFromURL(SandboxFromURLOptions{URL: srv.URL})
	require.NoError(t, err)
	return sb, func() execRequest {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func respondJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(body))
}

func TestExec(t *testing.T) {
	t.Run("SendsRequest", func(t *testing.T) {
		sb, lastRequest := execSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusOK, processRecord)
		})
		info, err := sb.Process().Exec(t.Context(), ProcessExecOptions{
			Command:           "ls",
			Timeout:           1500 * time.Millisecond,
			WaitForPorts:      []int{8080},
			WaitForCompletion: true,
		})
		require.NoError(t, err)
		request := lastRequest()
		require.Equal(t, "Bearer token", request.authorization)
		require.Equal(t, `{"command":"ls","timeout":2,"waitForCompletion":true,"waitForPorts":[8080]}`, request.body)
		require.Equal(t, ProcessStatusCompleted, info.Status)
		require.Equal(t, "out", info.Stdout)
	})

	t.Run("GatewayErrorNotRetried", func(t *testing.T) {
		var calls atomic.Int32
		sb, _ := execSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		_, err := sb.Process().Exec(t.Context(), ProcessExecOptions{Command: "ls"})
		require.ErrorAs[*GatewayError](t, err)
		require.Equal(t, int32(1), calls.Load())
	})
}

func TestExecStream(t *testing.T) {
	// streamSandbox serves the given NDJSON records to a streaming exec.
	streamSandbox := func(t *testing.T, records ...string) (*Sandbox, func() execRequest) {
		return execSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			for _, record := range records {
				w.Write([]byte(record + "\n"))
			}
		})
	}
	result, err := json.Marshal(processRecord)
	require.NoError(t, err)
	resultRecord := `{"type":"result","data":` + string(result) + `}`

	t.Run("YieldsOutputThenExit", func(t *testing.T) {
		sb, lastRequest := streamSandbox(t,
			`{"type":"stdout","data":"Name? "}`,
			`{"type":"stdout","data":""}`,
			`{"type":"keepalive","data":""}`,
			``,
			`{"type":"stderr","data":"warn\n"}`,
			resultRecord,
		)
		var events []ProcessExecEvent
		for event, err := range sb.Process().ExecStream(t.Context(), ProcessExecStreamOptions{Command: "ls"}) {
			require.NoError(t, err)
			events = append(events, event)
		}
		// The empty chunk is kept, the keepalive and blank line are not.
		require.Len(t, events, 4)
		require.Equal(t, ProcessExecEvent{TextStream: ProcessStreamStdout, Text: "Name? "}, events[0])
		require.Equal(t, ProcessExecEvent{TextStream: ProcessStreamStdout}, events[1])
		require.Equal(t, ProcessExecEvent{TextStream: ProcessStreamStderr, Text: "warn\n"}, events[2])
		require.Equal(t, "12", events[3].Exit.PID)
		request := lastRequest()
		require.Equal(t, "application/x-ndjson", request.accept)
		require.Equal(t, `{"command":"ls","waitForCompletion":true}`, request.body)
	})

	t.Run("MissingExitErrors", func(t *testing.T) {
		sb, _ := streamSandbox(t, `{"type":"stdout","data":"x"}`)
		var lastErr error
		for _, err := range sb.Process().ExecStream(t.Context(), ProcessExecStreamOptions{Command: "ls"}) {
			lastErr = err
		}
		require.Error(t, lastErr)
		require.Contains(t, lastErr.Error(), "before reporting the process's exit")
	})

	t.Run("ErrorRecordErrors", func(t *testing.T) {
		sb, _ := streamSandbox(t, `{"type":"stdout","data":"x"}`, `{"type":"error","data":"process vanished"}`, resultRecord)
		var events []ProcessExecEvent
		var lastErr error
		for event, err := range sb.Process().ExecStream(t.Context(), ProcessExecStreamOptions{Command: "ls"}) {
			if err != nil {
				lastErr = err
				continue
			}
			events = append(events, event)
		}
		// The error ends the stream, so the result after it is never read.
		require.Len(t, events, 1)
		require.Error(t, lastErr)
		require.Contains(t, lastErr.Error(), "process vanished")
	})

	t.Run("UnreadableRecordErrors", func(t *testing.T) {
		sb, _ := streamSandbox(t, `not json`)
		var lastErr error
		for _, err := range sb.Process().ExecStream(t.Context(), ProcessExecStreamOptions{Command: "ls"}) {
			lastErr = err
		}
		require.Error(t, lastErr)
	})

	t.Run("BreakStopsQuietly", func(t *testing.T) {
		sb, _ := streamSandbox(t, `{"type":"stdout","data":"a"}`, `{"type":"stdout","data":"b"}`, resultRecord)
		count := 0
		for _, err := range sb.Process().ExecStream(t.Context(), ProcessExecStreamOptions{Command: "ls"}) {
			require.NoError(t, err)
			count++
			break
		}
		require.Equal(t, 1, count)
	})

	t.Run("APIError", func(t *testing.T) {
		sb, _ := execSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusUnprocessableEntity, `{"error":"bad command"}`)
		})
		count := 0
		for _, err := range sb.Process().ExecStream(t.Context(), ProcessExecStreamOptions{Command: "bad"}) {
			count++
			apiErr := require.ErrorAs[*APIError](t, err)
			require.Equal(t, 422, apiErr.Status)
		}
		require.Equal(t, 1, count)
	})
}

func TestProcessList(t *testing.T) {
	shortBackoff(t)
	var calls atomic.Int32
	sb, _ := execSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
		// The first attempt meets a gateway error, which a read rides out.
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		respondJSON(w, http.StatusOK, `[`+processRecord+`]`)
	})
	processes, err := sb.Process().List(t.Context(), ProcessListOptions{})
	require.NoError(t, err)
	require.Len(t, processes, 1)
	require.Equal(t, "12", processes[0].PID)
	require.Equal(t, int32(2), calls.Load())
}

func TestProcessLogs(t *testing.T) {
	sb, lastRequest := execSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(w, http.StatusOK, `{"stdout":"o","stderr":"e","logs":"oe"}`)
	})
	logs, err := sb.Process().Logs(t.Context(), ProcessLogsOptions{Identifier: "bg"})
	require.NoError(t, err)
	require.Equal(t, ProcessLogs{Stdout: "o", Stderr: "e", Logs: "oe"}, *logs)
	require.Equal(t, "/process/bg/logs", lastRequest().path)
}

func TestProcessCalls(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	sb, lastRequest := execSandbox(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/process/p" {
			respondJSON(w, http.StatusOK, processRecord)
			return
		}
		respondJSON(w, http.StatusOK, `{"message":"ok"}`)
	})
	ctx := t.Context()
	_, err := sb.Process().Get(ctx, ProcessGetOptions{Identifier: "p"})
	require.NoError(t, err)
	require.NoError(t, sb.Process().Stop(ctx, ProcessStopOptions{Identifier: "p"}))
	require.NoError(t, sb.Process().Kill(ctx, ProcessKillOptions{Identifier: "p"}))
	require.NoError(t, sb.Process().CloseStdin(ctx, ProcessCloseStdinOptions{Identifier: "p"}))
	require.NoError(t, sb.Process().WriteStdin(ctx, ProcessWriteStdinOptions{Identifier: "p", Data: []byte("a\x00b\n")}))
	require.Equal(t, "a\x00b\n", lastRequest().body)
	require.Equal(t, "GET /process/p,DELETE /process/p,DELETE /process/p/kill,DELETE /process/p/stdin,POST /process/p/stdin",
		strings.Join(seen, ","))
}

func TestProcessWait(t *testing.T) {
	running := strings.Replace(processRecord, `"completed"`, `"running"`, 1)

	t.Run("RidesOutErrorsUntilExit", func(t *testing.T) {
		var calls atomic.Int32
		sb, _ := execSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			switch calls.Add(1) {
			case 1:
				respondJSON(w, http.StatusOK, running)
			case 2:
				respondJSON(w, http.StatusInternalServerError, `{"error":"busy"}`)
			default:
				respondJSON(w, http.StatusOK, strings.Replace(processRecord, `"completed"`, `"failed"`, 1))
			}
		})
		info, err := sb.Process().Wait(t.Context(), ProcessWaitOptions{Identifier: "p", PollInterval: time.Millisecond})
		require.NoError(t, err)
		// A failed process is a result, not an error.
		require.Equal(t, ProcessStatusFailed, info.Status)
		require.Equal(t, int32(3), calls.Load())
	})

	t.Run("TimesOut", func(t *testing.T) {
		sb, _ := execSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusServiceUnavailable, `{"error":"waking"}`)
		})
		_, err := sb.Process().Wait(t.Context(), ProcessWaitOptions{
			Identifier:   "p",
			Timeout:      50 * time.Millisecond,
			PollInterval: time.Millisecond,
		})
		timeoutErr := require.ErrorAs[*ProcessWaitTimeoutError](t, err)
		require.Equal(t, "p", timeoutErr.Identifier)
		// The last poll's error is kept.
		require.ErrorAs[*GatewayError](t, err)
	})

	t.Run("UnknownStatusFails", func(t *testing.T) {
		sb, _ := execSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusOK, strings.Replace(processRecord, `"completed"`, `"paused"`, 1))
		})
		_, err := sb.Process().Wait(t.Context(), ProcessWaitOptions{Identifier: "p"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown status paused")
	})

	t.Run("NotFoundFails", func(t *testing.T) {
		sb, _ := execSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(w, http.StatusNotFound, `{"error":"process not found"}`)
		})
		_, err := sb.Process().Wait(t.Context(), ProcessWaitOptions{Identifier: "p"})
		apiErr := require.ErrorAs[*APIError](t, err)
		require.Equal(t, http.StatusNotFound, apiErr.Status)
	})
}

func TestProcessStreamLogs(t *testing.T) {
	sb, lastRequest := execSandbox(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("stdout:a\n[keepalive]\nstderr:b\nstdout:\nunprefixed\r\nstdout:last"))
	})
	var lines []ProcessLogLine
	for line, err := range sb.Process().StreamLogs(t.Context(), ProcessStreamLogsOptions{Identifier: "p"}) {
		require.NoError(t, err)
		lines = append(lines, line)
	}
	require.Len(t, lines, 5)
	require.Equal(t, ProcessLogLine{TextStream: ProcessStreamStdout, Text: "a"}, lines[0])
	require.Equal(t, ProcessLogLine{TextStream: ProcessStreamStderr, Text: "b"}, lines[1])
	// An empty line is real output.
	require.Equal(t, ProcessLogLine{TextStream: ProcessStreamStdout}, lines[2])
	require.Equal(t, ProcessLogLine{Text: "unprefixed"}, lines[3])
	// A last line without its newline is still yielded.
	require.Equal(t, ProcessLogLine{TextStream: ProcessStreamStdout, Text: "last"}, lines[4])
	require.Equal(t, "/process/p/logs/stream", lastRequest().path)
}

func TestProcessInfoFromAPI(t *testing.T) {
	t.Run("Running", func(t *testing.T) {
		var process sandboxapi.ProcessResponse
		require.NoError(t, json.Unmarshal([]byte(`{
			"pid": "12", "name": "bg", "command": "sleep 9", "status": "running", "exitCode": 0,
			"startedAt": "Wed, 01 Jan 2025 12:00:00 GMT", "completedAt": null,
			"stdout": "", "stderr": "", "logs": "", "workingDir": "", "keepAlive": true
		}`), &process))
		info, err := processInfoFromAPI(&process)
		require.NoError(t, err)
		require.Equal(t, "12", info.PID)
		require.Equal(t, ProcessStatusRunning, info.Status)
		require.True(t, time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC).Equal(info.StartedAt), "start time")
		require.True(t, info.CompletedAt.IsZero(), "running process has no completion time")
		require.True(t, info.KeepAlive, "keepAlive")
	})

	t.Run("BadTimestamp", func(t *testing.T) {
		_, err := processInfoFromAPI(&sandboxapi.ProcessResponse{StartedAt: "later"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "process start time")
	})
}
