package sandbox_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
	"github.com/basetenlabs/baseten-go/sandbox"
)

var (
	e2eAPIKey = os.Getenv("BASETEN_E2E_TEST_API_KEY")
	e2eDomain = os.Getenv("BASETEN_E2E_TEST_DOMAIN")
	// Set to dump every request and response, for spotting server-side
	// anomalies. Credentials are masked.
	e2eDebug = os.Getenv("BASETEN_E2E_TEST_DEBUG") == "1"
)

// Allowed clock difference between this machine and the server when checking
// that a timestamp falls within the run.
const e2eClockSkew = time.Minute

// e2eLabels are on every sandbox the suite creates.
var e2eLabels = map[string]string{"created_by": "e2e"}

// e2eSharedEnvs are the shared sandbox's env, by name.
var e2eSharedEnvs = map[string]sandbox.EnvValue{
	"E2E_PLAIN":  {Value: "plain-value", NonSecret: true},
	"E2E_SECRET": {Value: "secret-value"},
}

// The sandbox every e2e test shares, created on first use so runs without
// the e2e env never create one, and deleted by TestMain.
var shared struct {
	once    sync.Once
	started time.Time
	name    string
	info    *sandbox.Info
	err     error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if shared.name != "" {
		if err := deleteSharedSandbox(); err != nil {
			log.Print(err)
			code = 1
		}
	}
	os.Exit(code)
}

func deleteSharedSandbox() error {
	if os.Getenv("BASETEN_E2E_TEST_KEEP_SANDBOX") != "" {
		log.Printf("BASETEN_E2E_TEST_KEEP_SANDBOX set; leaving sandbox %s in place", shared.name)
		return nil
	}
	client, err := newE2EClient()
	if err != nil {
		return err
	}
	deleted, err := client.Delete(context.Background(), sandbox.DeleteOptions{Name: shared.name})
	if isNotFound(err) && shared.err != nil {
		// A create that never got through leaves nothing to delete.
		return nil
	} else if err != nil {
		return fmt.Errorf("deleting sandbox %s: %w", shared.name, err)
	}
	if deleted.Name != shared.name || deleted.Status != sandbox.StatusDeleting {
		return fmt.Errorf("deleting sandbox %s returned %s as %s", shared.name, deleted.Name, deleted.Status)
	}
	return nil
}

func skipOrFailE2E(t *testing.T) {
	t.Helper()
	if e2eAPIKey == "" {
		t.Skip("BASETEN_E2E_TEST_API_KEY not set")
	}
	if e2eDomain == "" {
		require.Fail(t, "BASETEN_E2E_TEST_API_KEY is set but BASETEN_E2E_TEST_DOMAIN is missing")
	}
}

func newE2EClient() (*sandbox.Client, error) {
	opts := sandbox.ClientOptions{APIKey: e2eAPIKey, BaseURL: "https://api." + e2eDomain}
	if e2eDebug {
		opts.HTTPClient = debugClient{}
	}
	return sandbox.NewClient(opts)
}

func e2eClient(t *testing.T) *sandbox.Client {
	t.Helper()
	skipOrFailE2E(t)
	client, err := newE2EClient()
	require.NoError(t, err)
	return client
}

// sharedSandbox returns the shared sandbox's name and record as of creation,
// creating it on first use.
func sharedSandbox(t *testing.T) (string, *sandbox.Info) {
	t.Helper()
	client := e2eClient(t)
	shared.once.Do(func() {
		shared.started = time.Now()
		// The name is known before creating, so even a partial create is
		// removed by TestMain.
		shared.name = uniqueName()
		_, shared.err = client.Create(context.Background(), sandbox.CreateOptions{
			Name: shared.name,
			// TODO: Temporary, while other regions' exec planes reject our
			// tokens.
			Region: "us-was-1",
			Labels: e2eLabels,
			// Left unset, E2E_DEFAULT keeps the server's default, which is
			// secret.
			Envs: mergeEnvs(e2eSharedEnvs, map[string]sandbox.EnvValue{"E2E_DEFAULT": {Value: "default-value"}}),
		})
		if shared.err == nil {
			shared.info, shared.err = waitDeployed(context.Background(), client, shared.name)
		}
	})
	if shared.err != nil {
		t.Fatalf("creating the shared sandbox failed: %v", shared.err)
	}
	return shared.name, shared.info
}

func sharedSandboxHandle(t *testing.T) *sandbox.Sandbox {
	t.Helper()
	name, _ := sharedSandbox(t)
	sb, err := e2eClient(t).Get(t.Context(), sandbox.GetOptions{Name: name})
	require.NoError(t, err)
	return sb
}

func mergeEnvs(maps ...map[string]sandbox.EnvValue) map[string]sandbox.EnvValue {
	merged := map[string]sandbox.EnvValue{}
	for _, m := range maps {
		for k, v := range m {
			merged[k] = v
		}
	}
	return merged
}

// uniqueName is a fresh name, so concurrent runs never clash.
func uniqueName() string {
	suffix := make([]byte, 4)
	rand.Read(suffix)
	return "go-e2e-" + hex.EncodeToString(suffix)
}

// waitDeployed polls until a sandbox is deployed, failing fast if it cannot
// be.
func waitDeployed(ctx context.Context, client *sandbox.Client, name string) (*sandbox.Info, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		info, err := client.GetInfo(ctx, sandbox.GetInfoOptions{Name: name})
		if err != nil {
			return nil, err
		}
		switch {
		case info.Status == sandbox.StatusDeployed:
			return info, nil
		case info.Status != sandbox.StatusDeploying:
			return nil, fmt.Errorf("sandbox %s is %s, not deploying", name, info.Status)
		case time.Now().After(deadline):
			return nil, fmt.Errorf("sandbox %s is still deploying", name)
		}
		time.Sleep(time.Second)
	}
}

// requireWithinRun checks that a server timestamp falls between since and
// now, allowing for clock skew, which catches format and zone mistakes.
func requireWithinRun(t *testing.T, what string, since, ts time.Time) {
	t.Helper()
	if ts.Before(since.Add(-e2eClockSkew)) || ts.After(time.Now().Add(e2eClockSkew)) {
		t.Fatalf("%s %v is not between %v and now", what, ts, since)
	}
}

func TestE2EClient(t *testing.T) {
	client := e2eClient(t)
	sharedName, _ := sharedSandbox(t)

	t.Run("GetInfo", func(t *testing.T) {
		info, err := client.GetInfo(t.Context(), sandbox.GetInfoOptions{Name: sharedName})
		require.NoError(t, err)
		require.Equal(t, sharedName, info.Name)
		require.Equal(t, sandbox.StatusDeployed, info.Status)
		require.True(t, strings.HasPrefix(info.URL, "https://"), "url %q", info.URL)
		require.MapEqual(t, info.Labels, "created_by", "e2e")
		requireWithinRun(t, "created_at", shared.started, info.CreatedAt)
		// Only an env set as not secret comes back unmasked, and one left
		// unset is secret.
		require.Equal(t, sandbox.EnvValue{Value: "plain-value", NonSecret: true}, info.Envs["E2E_PLAIN"])
		require.Equal(t, sandbox.EnvValue{Value: "****"}, info.Envs["E2E_SECRET"])
		require.Equal(t, sandbox.EnvValue{Value: "****"}, info.Envs["E2E_DEFAULT"])
	})

	t.Run("GetInfoShowSecrets", func(t *testing.T) {
		// The e2e key is a workspace admin, so secrets are revealed.
		info, err := client.GetInfo(t.Context(), sandbox.GetInfoOptions{Name: sharedName, ShowSecrets: true})
		require.NoError(t, err)
		require.Equal(t, sandbox.EnvValue{Value: "plain-value", NonSecret: true}, info.Envs["E2E_PLAIN"])
		require.Equal(t, sandbox.EnvValue{Value: "secret-value"}, info.Envs["E2E_SECRET"])
		require.Equal(t, sandbox.EnvValue{Value: "default-value"}, info.Envs["E2E_DEFAULT"])
	})

	t.Run("GetByNameAndFromURL", func(t *testing.T) {
		info, err := client.GetInfo(t.Context(), sandbox.GetInfoOptions{Name: sharedName})
		require.NoError(t, err)
		byName, err := client.Get(t.Context(), sandbox.GetOptions{Name: sharedName})
		require.NoError(t, err)
		require.Equal(t, sharedName, byName.Name())
		require.Equal(t, strings.TrimRight(info.URL, "/"), byName.URL())
		fromURL, err := client.SandboxFromURL(sandbox.SandboxFromURLOptions{URL: info.URL})
		require.NoError(t, err)
		process, err := fromURL.Process().Exec(t.Context(), sandbox.ProcessExecOptions{Command: "true", WaitForCompletion: true})
		require.NoError(t, err)
		require.Equal(t, 0, process.ExitCode)
	})

	t.Run("List", func(t *testing.T) {
		var names []string
		for info, err := range client.List(t.Context(), sandbox.ListOptions{Query: sharedName}) {
			require.NoError(t, err)
			names = append(names, info.Name)
		}
		require.True(t, contains(names, sharedName), "%s not in %v", sharedName, names)
	})

	t.Run("CreateIfNotExists", func(t *testing.T) {
		before, err := client.GetInfo(t.Context(), sandbox.GetInfoOptions{Name: sharedName})
		require.NoError(t, err)
		created, err := client.Create(t.Context(), sandbox.CreateOptions{
			Name:              sharedName,
			CreateIfNotExists: true,
			Labels:            map[string]string{"other": "1"},
		})
		require.NoError(t, err)
		require.Equal(t, sharedName, created.Name())
		require.Equal(t, strings.TrimRight(before.URL, "/"), created.URL())
		require.True(t, before.CreatedAt.Equal(created.Info.CreatedAt), "created_at changed")
		// The existing configuration is kept, not replaced by the request's.
		require.Len(t, labelKeys(created.Info.Labels), len(before.Labels))
		require.MapEqual(t, created.Info.Labels, "created_by", "e2e")
	})

	// Changes only fields no other test on the shared sandbox depends on.
	t.Run("UpdateReplacesLabels", func(t *testing.T) {
		labels := map[string]string{"created_by": "e2e", "e2e_extra": "1"}
		updated, err := client.Update(t.Context(), sandbox.UpdateOptions{
			Name: sharedName,
			Lifecycle: &sandbox.Lifecycle{
				ExpirationPolicies:  []sandbox.ExpirationPolicy{{Type: sandbox.ExpirationPolicyTypeTTLIdle, After: time.Hour}},
				TerminatedRetention: 10 * time.Minute,
			},
			Labels: labels,
		})
		require.NoError(t, err)
		fetched, err := client.GetInfo(t.Context(), sandbox.GetInfoOptions{Name: sharedName})
		require.NoError(t, err)
		for _, info := range []*sandbox.Info{updated, fetched} {
			require.Len(t, info.Lifecycle.ExpirationPolicies, 1)
			require.Equal(t, sandbox.ExpirationPolicy{
				Type:   sandbox.ExpirationPolicyTypeTTLIdle,
				After:  time.Hour,
				Action: sandbox.ExpirationActionDelete,
			}, info.Lifecycle.ExpirationPolicies[0])
			require.Equal(t, 10*time.Minute, info.Lifecycle.TerminatedRetention)
			require.Len(t, labelKeys(info.Labels), 2)
			require.MapEqual(t, info.Labels, "e2e_extra", "1")
		}
		replaced, err := client.Update(t.Context(), sandbox.UpdateOptions{Name: sharedName, Labels: e2eLabels})
		require.NoError(t, err)
		require.Len(t, labelKeys(replaced.Labels), 1)
	})

	t.Run("UpdateWithNothingFails", func(t *testing.T) {
		_, err := client.Update(t.Context(), sandbox.UpdateOptions{Name: sharedName})
		apiErr := require.ErrorAs[*sandbox.APIError](t, err)
		require.Equal(t, http.StatusBadRequest, apiErr.Status)
	})

	t.Run("GetMissingFails", func(t *testing.T) {
		_, err := client.GetInfo(t.Context(), sandbox.GetInfoOptions{Name: sharedName + "-missing"})
		apiErr := require.ErrorAs[*sandbox.APIError](t, err)
		require.Equal(t, http.StatusNotFound, apiErr.Status)
		require.NotEqual(t, "", apiErr.Code)
	})

}

func TestE2EImageListLibrary(t *testing.T) {
	client := e2eClient(t)
	images, err := client.Images().ListLibrary(t.Context(), sandbox.ImageListLibraryOptions{})
	require.NoError(t, err)
	var base *sandbox.ImageLibraryInfo
	for i, image := range images {
		require.NotEqual(t, "", image.Image)
		if image.Name == "base-image" {
			base = &images[i]
		}
	}
	require.True(t, base != nil, "no base-image in the library")
	require.Equal(t, "baseten/base-image:latest", base.Image)
}

func TestE2EProcess(t *testing.T) {
	sb := sharedSandboxHandle(t)

	t.Run("RunsToCompletion", func(t *testing.T) {
		started := time.Now()
		process, err := sb.Process().Exec(t.Context(), sandbox.ProcessExecOptions{
			Command:           "sh -c 'echo out; echo err >&2; exit 3'",
			WaitForCompletion: true,
		})
		require.NoError(t, err)
		require.Equal(t, 3, process.ExitCode)
		require.Equal(t, "out", strings.TrimSpace(process.Stdout))
		require.Equal(t, "err", strings.TrimSpace(process.Stderr))
		requireWithinRun(t, "startedAt", started, process.StartedAt)
		requireWithinRun(t, "completedAt", started, process.CompletedAt)
	})

	t.Run("WorkingDir", func(t *testing.T) {
		process, err := sb.Process().Exec(t.Context(), sandbox.ProcessExecOptions{
			Command:           "pwd",
			WorkingDir:        "/tmp",
			WaitForCompletion: true,
		})
		require.NoError(t, err)
		require.Equal(t, "/tmp", strings.TrimSpace(process.Stdout))
	})

	t.Run("SandboxEnv", func(t *testing.T) {
		process, err := sb.Process().Exec(t.Context(), sandbox.ProcessExecOptions{
			Command:           "sh -c 'echo $E2E_PLAIN; echo $E2E_SECRET'",
			WaitForCompletion: true,
		})
		require.NoError(t, err)
		require.Equal(t, "plain-value\nsecret-value", strings.TrimSpace(process.Stdout))
	})

	t.Run("CommandEnv", func(t *testing.T) {
		process, err := sb.Process().Exec(t.Context(), sandbox.ProcessExecOptions{
			Command:           "sh -c 'echo $E2E_EXEC'",
			Env:               map[string]string{"E2E_EXEC": "exec-value"},
			WaitForCompletion: true,
		})
		require.NoError(t, err)
		require.Equal(t, "exec-value", strings.TrimSpace(process.Stdout))
	})

	t.Run("GetsListsAndWaitsForBackgroundProcess", func(t *testing.T) {
		name := startProcess(t, sb, sandbox.ProcessExecOptions{Command: "sh -c 'sleep 1; echo done'"})
		got, err := sb.Process().Get(t.Context(), sandbox.ProcessGetOptions{Identifier: name})
		require.NoError(t, err)
		require.Equal(t, name, got.Name)
		require.Equal(t, sandbox.ProcessStatusRunning, got.Status)
		require.True(t, got.CompletedAt.IsZero(), "completedAt %v while running", got.CompletedAt)
		processes, err := sb.Process().List(t.Context(), sandbox.ProcessListOptions{})
		require.NoError(t, err)
		var names []string
		for _, process := range processes {
			names = append(names, process.Name)
		}
		require.True(t, contains(names, name), "%s not in %v", name, names)

		finished, err := sb.Process().Wait(t.Context(), sandbox.ProcessWaitOptions{Identifier: name})
		require.NoError(t, err)
		require.Equal(t, sandbox.ProcessStatusCompleted, finished.Status)
		require.Equal(t, 0, finished.ExitCode)
		logs, err := sb.Process().Logs(t.Context(), sandbox.ProcessLogsOptions{Identifier: got.PID})
		require.NoError(t, err)
		require.Equal(t, "done", strings.TrimSpace(logs.Stdout))
	})

	t.Run("StopsAndKills", func(t *testing.T) {
		stopped := startProcess(t, sb, sandbox.ProcessExecOptions{Command: "sleep 300"})
		killed := startProcess(t, sb, sandbox.ProcessExecOptions{Command: "sleep 300"})
		require.NoError(t, sb.Process().Stop(t.Context(), sandbox.ProcessStopOptions{Identifier: stopped}))
		require.NoError(t, sb.Process().Kill(t.Context(), sandbox.ProcessKillOptions{Identifier: killed}))
		for identifier, status := range map[string]sandbox.ProcessStatus{
			stopped: sandbox.ProcessStatusStopped,
			killed:  sandbox.ProcessStatusKilled,
		} {
			process, err := sb.Process().Wait(t.Context(), sandbox.ProcessWaitOptions{Identifier: identifier})
			require.NoError(t, err)
			require.Equal(t, status, process.Status)
		}
	})

	t.Run("WritesAndClosesStdin", func(t *testing.T) {
		name := startProcess(t, sb, sandbox.ProcessExecOptions{Command: "cat", Stdin: true})
		require.NoError(t, sb.Process().WriteStdin(t.Context(), sandbox.ProcessWriteStdinOptions{Identifier: name, Data: []byte("hello\n")}))
		require.NoError(t, sb.Process().WriteStdin(t.Context(), sandbox.ProcessWriteStdinOptions{Identifier: name, Data: []byte("more\n")}))
		require.NoError(t, sb.Process().CloseStdin(t.Context(), sandbox.ProcessCloseStdinOptions{Identifier: name}))
		finished, err := sb.Process().Wait(t.Context(), sandbox.ProcessWaitOptions{Identifier: name})
		require.NoError(t, err)
		require.True(t, finished.Stdin, "stdin not recorded")
		require.Equal(t, sandbox.ProcessStatusCompleted, finished.Status)
		require.Equal(t, "hello\nmore\n", finished.Stdout)
	})

	t.Run("WriteStdinWithoutStdinFails", func(t *testing.T) {
		name := startProcess(t, sb, sandbox.ProcessExecOptions{Command: "sleep 300"})
		err := sb.Process().WriteStdin(t.Context(), sandbox.ProcessWriteStdinOptions{Identifier: name, Data: []byte("hello\n")})
		apiErr := require.ErrorAs[*sandbox.APIError](t, err)
		require.Equal(t, http.StatusConflict, apiErr.Status)
	})

	t.Run("StreamLogsOfRunningProcess", func(t *testing.T) {
		name := startProcess(t, sb, sandbox.ProcessExecOptions{Command: "sh -c 'sleep 1; echo a; echo b >&2; echo c'"})
		lines := streamLogLines(t, sb, name)
		// Order holds within each stream, not between them.
		require.Equal(t, "stdout:a,stdout:c", filterLogLines(lines, sandbox.ProcessStreamStdout))
		require.Equal(t, "stderr:b", filterLogLines(lines, sandbox.ProcessStreamStderr))
		require.Len(t, lines, 3)
	})

	t.Run("StreamLogsOfFinishedProcessFromStart", func(t *testing.T) {
		name := startProcess(t, sb, sandbox.ProcessExecOptions{Command: "sh -c 'echo a; echo b'"})
		_, err := sb.Process().Wait(t.Context(), sandbox.ProcessWaitOptions{Identifier: name})
		require.NoError(t, err)
		require.Equal(t, "stdout:a,stdout:b", filterLogLines(streamLogLines(t, sb, name), ""))
	})

	t.Run("WaitTimeoutLeavesProcessRunning", func(t *testing.T) {
		name := startProcess(t, sb, sandbox.ProcessExecOptions{Command: "sleep 300"})
		_, err := sb.Process().Wait(t.Context(), sandbox.ProcessWaitOptions{Identifier: name, Timeout: time.Second})
		timeoutErr := require.ErrorAs[*sandbox.ProcessWaitTimeoutError](t, err)
		require.Equal(t, name, timeoutErr.Identifier)
		process, err := sb.Process().Get(t.Context(), sandbox.ProcessGetOptions{Identifier: name})
		require.NoError(t, err)
		require.Equal(t, sandbox.ProcessStatusRunning, process.Status)
	})

	t.Run("ExecStreamOutputThenExit", func(t *testing.T) {
		var stdout, stderr strings.Builder
		var exit *sandbox.ProcessInfo
		for event, err := range sb.Process().ExecStream(t.Context(), sandbox.ProcessExecStreamOptions{
			Command: `sh -c 'printf "a\n\nb\n"; echo e >&2; exit 2'`,
		}) {
			require.NoError(t, err)
			require.True(t, exit == nil, "event after exit")
			switch event.TextStream {
			case sandbox.ProcessStreamStdout:
				stdout.WriteString(event.Text)
			case sandbox.ProcessStreamStderr:
				stderr.WriteString(event.Text)
			}
			exit = event.Exit
		}
		require.True(t, exit != nil, "no exit event")
		require.Equal(t, 2, exit.ExitCode)
		// Chunks follow the process's writes, so only what they join to is
		// fixed.
		require.Equal(t, "a\n\nb\n", stdout.String())
		require.Equal(t, "e\n", stderr.String())
	})

	t.Run("ExecStreamPromptBeforeNewline", func(t *testing.T) {
		name := uniqueName()
		t.Cleanup(func() { killQuietly(sb, name) })
		var stdout string
		for event, err := range sb.Process().ExecStream(t.Context(), sandbox.ProcessExecStreamOptions{
			Command: `sh -c 'printf "Continue? "; read answer; echo "got $answer"'`,
			Name:    name,
			Stdin:   true,
		}) {
			require.NoError(t, err)
			if event.Exit != nil {
				require.Equal(t, 0, event.Exit.ExitCode)
				break
			}
			if event.TextStream != sandbox.ProcessStreamStdout {
				continue
			}
			stdout += event.Text
			// The process blocks on the read, so this only happens if the
			// prompt arrived without its line being finished.
			if stdout == "Continue? " {
				require.NoError(t, sb.Process().WriteStdin(t.Context(), sandbox.ProcessWriteStdinOptions{Identifier: name, Data: []byte("yes\n")}))
			}
		}
		require.Equal(t, "Continue? got yes\n", stdout)
	})

	// The sandbox sends output as JSON strings, replacing invalid UTF-8 with
	// U+FFFD before it reaches the SDK.
	t.Run("NonUTF8OutputReplaced", func(t *testing.T) {
		process, err := sb.Process().Exec(t.Context(), sandbox.ProcessExecOptions{
			Command:           `printf 'a\377b'`,
			WaitForCompletion: true,
		})
		require.NoError(t, err)
		require.Equal(t, "a�b", process.Stdout)
		logs, err := sb.Process().Logs(t.Context(), sandbox.ProcessLogsOptions{Identifier: process.PID})
		require.NoError(t, err)
		require.Equal(t, "a�b", logs.Stdout)
		var streamed strings.Builder
		for event, err := range sb.Process().ExecStream(t.Context(), sandbox.ProcessExecStreamOptions{Command: `printf 'a\377b'`}) {
			require.NoError(t, err)
			streamed.WriteString(event.Text)
		}
		require.Equal(t, "a�b", streamed.String())
	})

	t.Run("UnknownProcessFails", func(t *testing.T) {
		_, err := sb.Process().Logs(t.Context(), sandbox.ProcessLogsOptions{Identifier: uniqueName()})
		apiErr := require.ErrorAs[*sandbox.APIError](t, err)
		require.Equal(t, http.StatusNotFound, apiErr.Status)
		_, err = sb.Process().Get(t.Context(), sandbox.ProcessGetOptions{Identifier: uniqueName()})
		apiErr = require.ErrorAs[*sandbox.APIError](t, err)
		require.Equal(t, http.StatusNotFound, apiErr.Status)
	})
}

// startProcess starts a background process under a fresh name, killed when the
// test ends, and returns the name.
func startProcess(t *testing.T, sb *sandbox.Sandbox, opts sandbox.ProcessExecOptions) string {
	t.Helper()
	opts.Name = uniqueName()
	t.Cleanup(func() { killQuietly(sb, opts.Name) })
	started, err := sb.Process().Exec(t.Context(), opts)
	require.NoError(t, err)
	require.Equal(t, opts.Name, started.Name)
	return opts.Name
}

// killQuietly kills a process the test may already have ended, so cleanup
// never masks the test's own failure.
func killQuietly(sb *sandbox.Sandbox, identifier string) {
	_ = sb.Process().Kill(context.Background(), sandbox.ProcessKillOptions{Identifier: identifier})
}

func streamLogLines(t *testing.T, sb *sandbox.Sandbox, identifier string) []sandbox.ProcessLogLine {
	t.Helper()
	var lines []sandbox.ProcessLogLine
	for line, err := range sb.Process().StreamLogs(t.Context(), sandbox.ProcessStreamLogsOptions{Identifier: identifier}) {
		require.NoError(t, err)
		lines = append(lines, line)
	}
	return lines
}

// filterLogLines joins the lines from one stream, or all when stream is empty,
// as "stream:text".
func filterLogLines(lines []sandbox.ProcessLogLine, stream sandbox.ProcessStream) string {
	var parts []string
	for _, line := range lines {
		if stream == "" || line.TextStream == stream {
			parts = append(parts, string(line.TextStream)+":"+line.Text)
		}
	}
	return strings.Join(parts, ",")
}

func TestE2EFileSystem(t *testing.T) {
	sb := sharedSandboxHandle(t)
	fs := sb.FS()

	// A directory of the test's own, so tests on the shared sandbox never see
	// each other's files.
	testDir := func(t *testing.T) string {
		t.Helper()
		dir := "/tmp/e2e-fs/" + uniqueName()
		require.NoError(t, fs.Mkdir(t.Context(), sandbox.FileSystemMkdirOptions{Path: dir}))
		return dir
	}
	read := func(t *testing.T, path string) string {
		t.Helper()
		content, err := fs.Read(t.Context(), sandbox.FileSystemReadOptions{Path: path})
		require.NoError(t, err)
		return content
	}
	write := func(t *testing.T, path, content string) {
		t.Helper()
		require.NoError(t, fs.Write(t.Context(), sandbox.FileSystemWriteOptions{Path: path, Content: content}))
	}
	mode := func(t *testing.T, path string) string {
		t.Helper()
		process, err := sb.Process().Exec(t.Context(), sandbox.ProcessExecOptions{
			Command:           "stat -c %a '" + path + "'",
			WaitForCompletion: true,
		})
		require.NoError(t, err)
		return strings.TrimSpace(process.Stdout)
	}

	t.Run("WritesAndReads", func(t *testing.T) {
		dir := testDir(t)
		write(t, dir+"/hello.txt", "hello")
		require.Equal(t, "hello", read(t, dir+"/hello.txt"))
	})

	t.Run("WritesIntoMissingDirectories", func(t *testing.T) {
		dir := testDir(t)
		write(t, dir+"/a/b/c.txt", "nested")
		require.Equal(t, "nested", read(t, dir+"/a/b/c.txt"))
	})

	t.Run("WritesAndReadsRelativePath", func(t *testing.T) {
		path := uniqueName() + ".txt"
		write(t, path, "relative")
		t.Cleanup(func() {
			require.NoError(t, fs.Remove(context.Background(), sandbox.FileSystemRemoveOptions{Path: path}))
		})
		require.Equal(t, "relative", read(t, path))
	})

	t.Run("ReadMissingFails", func(t *testing.T) {
		dir := testDir(t)
		_, err := fs.Read(t.Context(), sandbox.FileSystemReadOptions{Path: dir + "/missing.txt"})
		apiErr := require.ErrorAs[*sandbox.APIError](t, err)
		require.Equal(t, http.StatusNotFound, apiErr.Status)
	})

	t.Run("NamesNeedingEscaping", func(t *testing.T) {
		dir := testDir(t)
		path := dir + "/a b%#?.txt"
		write(t, path, "escaped")
		require.Equal(t, "escaped", read(t, path))
		require.NoError(t, fs.WriteBytes(t.Context(), sandbox.FileSystemWriteBytesOptions{Path: path, Content: []byte{1, 2, 3}}))
		content, err := fs.ReadBytes(t.Context(), sandbox.FileSystemReadBytesOptions{Path: path})
		require.NoError(t, err)
		require.True(t, bytes.Equal([]byte{1, 2, 3}, content), "content %v", content)
		listing, err := fs.List(t.Context(), sandbox.FileSystemListOptions{Path: dir})
		require.NoError(t, err)
		require.Len(t, listing.Files, 1)
		require.Equal(t, "a b%#?.txt", listing.Files[0].Name)
	})

	t.Run("RoundTripsEveryByteValue", func(t *testing.T) {
		dir := testDir(t)
		all := make([]byte, 256)
		for i := range all {
			all[i] = byte(i)
		}
		require.NoError(t, fs.WriteBytes(t.Context(), sandbox.FileSystemWriteBytesOptions{Path: dir + "/all.bin", Content: all}))
		content, err := fs.ReadBytes(t.Context(), sandbox.FileSystemReadBytesOptions{Path: dir + "/all.bin"})
		require.NoError(t, err)
		require.True(t, bytes.Equal(all, content), "content differs")
	})

	t.Run("WriteBytesWithPermissions", func(t *testing.T) {
		dir := testDir(t)
		require.NoError(t, fs.WriteBytes(t.Context(), sandbox.FileSystemWriteBytesOptions{
			Path:        dir + "/run.sh",
			Content:     []byte("#!/bin/sh\necho ran\n"),
			Permissions: "0755",
		}))
		require.Equal(t, "755", mode(t, dir+"/run.sh"))
		ran, err := sb.Process().Exec(t.Context(), sandbox.ProcessExecOptions{Command: dir + "/run.sh", WaitForCompletion: true})
		require.NoError(t, err)
		require.Equal(t, "ran", strings.TrimSpace(ran.Stdout))
		// An existing file keeps its mode.
		require.NoError(t, fs.WriteBytes(t.Context(), sandbox.FileSystemWriteBytesOptions{
			Path:        dir + "/run.sh",
			Content:     []byte("#!/bin/sh\necho again\n"),
			Permissions: "0700",
		}))
		require.Equal(t, "755", mode(t, dir+"/run.sh"))
	})

	t.Run("WriteBytesOverThresholdInParts", func(t *testing.T) {
		dir := testDir(t)
		// Three parts, the last one partial, in a pattern that does not line
		// up with the part size, so a part off by any number of bytes shows.
		content := make([]byte, 12<<20+17)
		for i := range content {
			content[i] = byte(i % 251)
		}
		require.NoError(t, fs.WriteBytes(t.Context(), sandbox.FileSystemWriteBytesOptions{
			Path:        dir + "/big.bin",
			Content:     content,
			Permissions: "0755",
		}))
		got, err := fs.ReadBytes(t.Context(), sandbox.FileSystemReadBytesOptions{Path: dir + "/big.bin"})
		require.NoError(t, err)
		require.True(t, bytes.Equal(content, got), "read %d bytes, want %d exactly as written", len(got), len(content))
		require.Equal(t, "755", mode(t, dir+"/big.bin"))
	})

	t.Run("WriteOverThresholdInParts", func(t *testing.T) {
		dir := testDir(t)
		// Two bytes each in UTF-8, with a line number so parts out of order
		// show.
		var content strings.Builder
		for i := range 1_000_000 {
			fmt.Fprintf(&content, "%d é\n", i)
		}
		write(t, dir+"/big.txt", content.String())
		require.True(t, read(t, dir+"/big.txt") == content.String(), "content differs")
	})

	t.Run("MkdirWithPermissions", func(t *testing.T) {
		dir := testDir(t)
		require.NoError(t, fs.Mkdir(t.Context(), sandbox.FileSystemMkdirOptions{Path: dir + "/private", Permissions: "0700"}))
		require.Equal(t, "700", mode(t, dir+"/private"))
	})

	t.Run("ReadDirectoryAndListFileFail", func(t *testing.T) {
		dir := testDir(t)
		write(t, dir+"/f.txt", "f")
		_, err := fs.Read(t.Context(), sandbox.FileSystemReadOptions{Path: dir})
		require.Error(t, err)
		require.Contains(t, err.Error(), "is a directory")
		_, err = fs.ReadBytes(t.Context(), sandbox.FileSystemReadBytesOptions{Path: dir})
		require.Error(t, err)
		require.Contains(t, err.Error(), "is a directory")
		_, err = fs.List(t.Context(), sandbox.FileSystemListOptions{Path: dir + "/f.txt"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "is a file")
	})

	t.Run("MkdirMakesParentsAndRepeats", func(t *testing.T) {
		dir := testDir(t)
		require.NoError(t, fs.Mkdir(t.Context(), sandbox.FileSystemMkdirOptions{Path: dir + "/x/y/z"}))
		require.NoError(t, fs.Mkdir(t.Context(), sandbox.FileSystemMkdirOptions{Path: dir + "/x/y/z"}))
		listing, err := fs.List(t.Context(), sandbox.FileSystemListOptions{Path: dir + "/x/y"})
		require.NoError(t, err)
		require.Len(t, listing.Subdirectories, 1)
		require.Equal(t, "z", listing.Subdirectories[0].Name)
	})

	t.Run("Lists", func(t *testing.T) {
		dir := testDir(t)
		before := time.Now()
		write(t, dir+"/a.txt", "12345")
		require.NoError(t, fs.Mkdir(t.Context(), sandbox.FileSystemMkdirOptions{Path: dir + "/sub"}))
		listing, err := fs.List(t.Context(), sandbox.FileSystemListOptions{Path: dir})
		require.NoError(t, err)
		require.Equal(t, dir, listing.Path)
		require.Len(t, listing.Subdirectories, 1)
		require.Equal(t, "sub", listing.Subdirectories[0].Name)
		require.Len(t, listing.Files, 1)
		file := listing.Files[0]
		require.Equal(t, "a.txt", file.Name)
		require.Equal(t, dir+"/a.txt", file.Path)
		require.Equal(t, int64(5), file.Size)
		requireWithinRun(t, "lastModified", before, file.LastModified)
	})

	t.Run("RemovesNonEmptyDirectoryOnlyRecursively", func(t *testing.T) {
		dir := testDir(t)
		write(t, dir+"/f.txt", "f")
		require.NoError(t, fs.Remove(t.Context(), sandbox.FileSystemRemoveOptions{Path: dir + "/f.txt"}))
		_, err := fs.Read(t.Context(), sandbox.FileSystemReadOptions{Path: dir + "/f.txt"})
		require.True(t, isNotFound(err), "read after remove: %v", err)

		write(t, dir+"/d/g.txt", "g")
		err = fs.Remove(t.Context(), sandbox.FileSystemRemoveOptions{Path: dir + "/d"})
		require.ErrorAs[*sandbox.APIError](t, err)
		require.NoError(t, fs.Remove(t.Context(), sandbox.FileSystemRemoveOptions{Path: dir + "/d", Recursive: true}))
		_, err = fs.List(t.Context(), sandbox.FileSystemListOptions{Path: dir + "/d"})
		require.True(t, isNotFound(err), "list after remove: %v", err)
	})

	t.Run("FindsByPatternAndType", func(t *testing.T) {
		dir := testDir(t)
		require.NoError(t, fs.WriteTree(t.Context(), sandbox.FileSystemWriteTreeOptions{
			Path:  dir,
			Files: map[string]string{"a.ts": "a", "b.js": "b", "sub/c.ts": "c"},
		}))
		ts, err := fs.Find(t.Context(), sandbox.FileSystemFindOptions{Path: dir, Patterns: []string{"*.ts"}})
		require.NoError(t, err)
		var paths []string
		for _, match := range ts.Matches {
			paths = append(paths, match.Path)
		}
		slices.Sort(paths)
		// Relative to the searched path, as observed from the server.
		require.Equal(t, "a.ts,sub/c.ts", strings.Join(paths, ","))
		require.Equal(t, 2, ts.Total)
		dirs, err := fs.Find(t.Context(), sandbox.FileSystemFindOptions{Path: dir, Type: sandbox.FileSystemEntryTypeDirectory})
		require.NoError(t, err)
		paths = nil
		for _, match := range dirs.Matches {
			require.Equal(t, sandbox.FileSystemEntryTypeDirectory, match.Type)
			paths = append(paths, match.Path)
		}
		require.True(t, contains(paths, "sub"), "sub not in %v", paths)
	})

	t.Run("Greps", func(t *testing.T) {
		dir := testDir(t)
		require.NoError(t, fs.WriteTree(t.Context(), sandbox.FileSystemWriteTreeOptions{
			Path:  dir,
			Files: map[string]string{"a.txt": "one\nfind Needle here\nthree\n", "b.txt": "nothing\n"},
		}))
		found, err := fs.Grep(t.Context(), sandbox.FileSystemGrepOptions{Path: dir, Query: "needle"})
		require.NoError(t, err)
		require.Equal(t, 1, found.Total)
		require.Len(t, found.Matches, 1)
		// Relative to the searched path, as observed from the server.
		require.Equal(t, "a.txt", found.Matches[0].Path)
		require.Equal(t, 2, found.Matches[0].Line)
		require.Equal(t, "find Needle here", found.Matches[0].Text)
		require.Equal(t, "", found.Matches[0].Context)
		withContext, err := fs.Grep(t.Context(), sandbox.FileSystemGrepOptions{Path: dir, Query: "needle", ContextLines: 1})
		require.NoError(t, err)
		require.Len(t, withContext.Matches, 1)
		require.Equal(t, "one\nfind Needle here\nthree", withContext.Matches[0].Context)
		exact, err := fs.Grep(t.Context(), sandbox.FileSystemGrepOptions{Path: dir, Query: "needle", CaseSensitive: true})
		require.NoError(t, err)
		require.Equal(t, 0, exact.Total)
	})

	t.Run("CopiesFilesAndDirectories", func(t *testing.T) {
		dir := testDir(t)
		write(t, dir+"/it's.txt", "quoted")
		require.NoError(t, fs.Copy(t.Context(), sandbox.FileSystemCopyOptions{Source: dir + "/it's.txt", Destination: dir + "/copy.txt"}))
		require.Equal(t, "quoted", read(t, dir+"/copy.txt"))

		require.NoError(t, fs.WriteTree(t.Context(), sandbox.FileSystemWriteTreeOptions{
			Path:  dir + "/src",
			Files: map[string]string{"a.txt": "a", "d/b.txt": "b"},
		}))
		require.NoError(t, fs.Copy(t.Context(), sandbox.FileSystemCopyOptions{Source: dir + "/src", Destination: dir + "/dst"}))
		require.Equal(t, "b", read(t, dir+"/dst/d/b.txt"))
	})

	t.Run("CopyMissingSourceFails", func(t *testing.T) {
		dir := testDir(t)
		err := fs.Copy(t.Context(), sandbox.FileSystemCopyOptions{Source: dir + "/missing", Destination: dir + "/x"})
		copyErr := require.ErrorAs[*sandbox.FileSystemCopyError](t, err)
		require.Contains(t, copyErr.Process.Stderr, "missing")
	})

	t.Run("WriteTreeLeavesOtherFiles", func(t *testing.T) {
		dir := testDir(t)
		write(t, dir+"/keep.txt", "keep")
		require.NoError(t, fs.WriteTree(t.Context(), sandbox.FileSystemWriteTreeOptions{
			Path:  dir,
			Files: map[string]string{"a.txt": "a", "d/e/b.txt": "b"},
		}))
		require.Equal(t, "a", read(t, dir+"/a.txt"))
		require.Equal(t, "b", read(t, dir+"/d/e/b.txt"))
		require.Equal(t, "keep", read(t, dir+"/keep.txt"))
	})

	// Watches with opts and writes path until the watch reports an event for
	// it.
	watchUntilWritten := func(t *testing.T, opts sandbox.FileSystemWatchOptions, path string) {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		events := make(chan sandbox.FileSystemWatchEvent, 100)
		watchErr := make(chan error, 1)
		go func() {
			defer close(events)
			for event, err := range fs.Watch(ctx, opts) {
				if err != nil {
					watchErr <- err
					return
				}
				select {
				case events <- event:
				case <-ctx.Done():
					return
				}
			}
			watchErr <- nil
		}()
		// The server's watch starts at some unknown point after the request,
		// so a change made too early is missed. Rather than sleeping, this
		// repeats the change, each attempt a full round trip, until its event
		// arrives.
		var found *sandbox.FileSystemWatchEvent
		var unmatched []sandbox.FileSystemWatchEvent
		for attempt := 0; attempt < 20 && found == nil; attempt++ {
			write(t, path, fmt.Sprint(attempt))
		drain:
			for found == nil {
				select {
				case event, ok := <-events:
					if !ok {
						require.NoError(t, <-watchErr)
						t.Fatal("the watch ended before an event for the file")
					}
					if event.Path == path {
						found = &event
					} else {
						unmatched = append(unmatched, event)
					}
				default:
					break drain
				}
			}
		}
		require.True(t, found != nil, "no event for %s, only %v", path, unmatched)
		require.True(t, slices.Contains(found.Ops, sandbox.FileSystemWatchOpCreate) || slices.Contains(found.Ops, sandbox.FileSystemWatchOpWrite),
			"ops %v", found.Ops)
	}

	// The watch covers only the directory itself: in an earlier run, 20
	// writes to a file in a subdirectory produced no event for it.
	t.Run("WatchesDirectory", func(t *testing.T) {
		dir := testDir(t)
		watchUntilWritten(t, sandbox.FileSystemWatchOptions{Path: dir}, dir+"/top.txt")
	})

	t.Run("WatchesRecursively", func(t *testing.T) {
		dir := testDir(t)
		// The subdirectory exists before the watch, so the event shows the
		// watch covers it.
		write(t, dir+"/sub/existing.txt", "existing")
		watchUntilWritten(t, sandbox.FileSystemWatchOptions{Path: dir, Recursive: true}, dir+"/sub/nested.txt")
	})
}

// Every push starts a build on the server, and this suite runs on every CI
// push, so the suite pushes exactly one image and keeps its build small.
func TestE2EImagePushAndCreate(t *testing.T) {
	client := e2eClient(t)
	ctx := t.Context()
	imageName := uniqueName()
	sandboxName := uniqueName()
	sandboxCreated := false
	t.Cleanup(func() {
		// The image cannot be deleted while a sandbox uses it. Cleanup errors
		// are logged rather than failing, so they never hide the test's own
		// failure.
		ctx := context.Background()
		if sandboxCreated {
			if err := deleteSandboxAndWait(ctx, client, sandboxName); err != nil {
				t.Logf("cleanup of sandbox %s failed: %v", sandboxName, err)
			}
		}
		if _, err := client.Images().Delete(ctx, sandbox.ImageDeleteOptions{Name: imageName}); err != nil && !isNotFound(err) {
			t.Errorf("cleanup of image %s failed: %v", imageName, err)
		}
	})

	// A pullable base, since the default sandbox image name only resolves on
	// sandbox create, not in a build. The label makes each run's build
	// context unique: identical pushes share one build on the server, which
	// links their images so none can be deleted before the others, and CI
	// runs this concurrently.
	// The sandbox API binary and entrypoint come from the builder's
	// injection.
	builder := sandbox.NewImageBuilder("debian:bookworm-slim").
		Label(map[string]string{"e2e-image": imageName}).
		AddFile("/hello.txt", []byte("hello from the image")).
		RunCommands("echo built by RUN > /run.txt")
	dockerfile, err := builder.Dockerfile()
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(dockerfile,
		"COPY --from=ghcr.io/blaxel-ai/sandbox:latest /sandbox-api /usr/local/bin/sandbox-api\n"+
			`ENTRYPOINT ["/usr/local/bin/sandbox-api"]`+"\n"), "no injected lines in %s", dockerfile)
	pushed, err := client.Images().Push(ctx, sandbox.ImagePushOptions{Name: imageName, SourceBuilder: builder})
	require.NoError(t, err)
	require.Equal(t, imageName, pushed.Name)
	require.Equal(t, sandbox.ImageStatusBuilt, pushed.Status)

	info, err := client.Images().GetInfo(ctx, sandbox.ImageGetInfoOptions{Name: imageName})
	require.NoError(t, err)
	require.Equal(t, sandbox.ImageStatusBuilt, info.Status)
	var listed []string
	for image, err := range client.Images().List(ctx, sandbox.ImageListOptions{NamePrefix: imageName}) {
		require.NoError(t, err)
		listed = append(listed, image.Name)
	}
	require.Equal(t, imageName, strings.Join(listed, ","))
	tags := 0
	for _, err := range client.Images().ListTags(ctx, sandbox.ImageListTagsOptions{Name: imageName}) {
		require.NoError(t, err)
		tags++
	}
	require.True(t, tags > 0, "no tags")

	sandboxCreated = true
	_, err = client.Create(ctx, sandbox.CreateOptions{
		Name:  sandboxName,
		Image: imageName + ":latest",
		// TODO: Same temporary region pin as the shared sandbox.
		Region: "us-was-1",
		Labels: e2eLabels,
	})
	require.NoError(t, err)
	_, err = waitDeployed(ctx, client, sandboxName)
	require.NoError(t, err)
	sb, err := client.Get(ctx, sandbox.GetOptions{Name: sandboxName})
	require.NoError(t, err)
	exec := func(command string) *sandbox.ProcessInfo {
		t.Helper()
		process, err := sb.Process().Exec(ctx, sandbox.ProcessExecOptions{Command: command, WaitForCompletion: true})
		require.NoError(t, err)
		return process
	}
	for path, want := range map[string]string{"/hello.txt": "hello from the image", "/run.txt": "built by RUN\n"} {
		content, err := sb.FS().Read(ctx, sandbox.FileSystemReadOptions{Path: path})
		require.NoError(t, err)
		require.Equal(t, want, content)
	}
	// The copied binary is where the copy put it, and runs as the entrypoint,
	// which is what answers these calls at all.
	require.Equal(t, 0, exec("test -x /usr/local/bin/sandbox-api").ExitCode)
	require.Regexp(t, `^/usr/local/bin/sandbox-api\b`, exec(`tr '\0' ' ' < /proc/1/cmdline`).Stdout)

	// Not checked for content: some builds have no logs on the server, even
	// days later, so only the order is asserted.
	logs, err := client.Images().Logs(ctx, sandbox.ImageLogsOptions{Name: imageName})
	require.NoError(t, err)
	for i := 1; i < len(logs); i++ {
		require.False(t, logs[i].Timestamp.Before(logs[i-1].Timestamp), "log lines out of order at %d", i)
	}
}

func deleteSandboxAndWait(ctx context.Context, client *sandbox.Client, name string) error {
	if _, err := client.Delete(ctx, sandbox.DeleteOptions{Name: name}); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		info, err := client.GetInfo(ctx, sandbox.GetInfoOptions{Name: name})
		if isNotFound(err) {
			return nil
		} else if err != nil {
			return err
		}
		// A deleted sandbox stays listed as TERMINATED rather than
		// disappearing.
		if info.Status == sandbox.StatusTerminated {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sandbox %s is still %s", name, info.Status)
		}
		time.Sleep(2 * time.Second)
	}
}

func isNotFound(err error) bool {
	var apiErr *sandbox.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func labelKeys(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	return keys
}

// debugClient logs each request and its response as one entry, once the
// response body is done. The body is not held back from the caller, so
// streams still stream.
type debugClient struct{}

func (debugClient) Do(req *http.Request) (*http.Response, error) {
	var requestBody []byte
	if req.Body != nil {
		requestBody, _ = io.ReadAll(req.Body)
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(requestBody))
	}
	headers := req.Header.Clone()
	if headers.Get("Authorization") != "" {
		headers.Set("Authorization", "<masked>")
	}
	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("%s %s request=%s %s failed after %v: %v", req.Method, req.URL, headers, requestBody, time.Since(started), err)
		return nil, err
	}
	// The token exchange's response body is the minted token.
	masked := req.URL.Path == "/v1/token"
	resp.Body = &loggedBody{
		ReadCloser: resp.Body,
		masked:     masked,
		log: func(body string) {
			log.Printf("%s %s request=%s %s response=%d %v %v %s",
				req.Method, req.URL, headers, requestBody, resp.StatusCode, time.Since(started), resp.Header, body)
		},
	}
	return resp, nil
}

// loggedBody passes a response body through, keeping a copy to log on close.
type loggedBody struct {
	io.ReadCloser
	masked bool
	log    func(string)
	copy   bytes.Buffer
	once   sync.Once
}

func (b *loggedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.copy.Write(p[:n])
	return n, err
}

func (b *loggedBody) Close() error {
	b.once.Do(func() {
		if b.masked {
			b.log("<masked>")
		} else {
			b.log(b.copy.String())
		}
	})
	return b.ReadCloser.Close()
}
