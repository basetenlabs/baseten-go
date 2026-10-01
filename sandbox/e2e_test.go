package sandbox_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/sandbox"
)

// Live end-to-end test against a Baseten environment, skipped unless the same
// three environment variables as the other SDKs' suites are set:
// BASETEN_E2E_TEST_API_KEY, BASETEN_E2E_TEST_DOMAIN, and
// BASETEN_E2E_TEST_SANDBOXES_DOMAIN (the domain serving /v1/sandboxes, which
// during development differs from the management domain).
func e2eClient(t *testing.T) *sandbox.SandboxesClient {
	t.Helper()
	apiKey := os.Getenv("BASETEN_E2E_TEST_API_KEY")
	domain := os.Getenv("BASETEN_E2E_TEST_DOMAIN")
	sandboxesDomain := os.Getenv("BASETEN_E2E_TEST_SANDBOXES_DOMAIN")
	if apiKey == "" || domain == "" || sandboxesDomain == "" {
		t.Skip("set BASETEN_E2E_TEST_API_KEY, BASETEN_E2E_TEST_DOMAIN, and BASETEN_E2E_TEST_SANDBOXES_DOMAIN")
	}
	client, err := sandbox.NewSandboxesClient(sandbox.SandboxesClientOptions{
		APIKey:            apiKey,
		ManagementBaseURL: "https://api." + domain,
		SandboxesBaseURL:  "https://api." + sandboxesDomain,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestE2ESandboxLifecycleAndExec(t *testing.T) {
	client := e2eClient(t)
	ctx := context.Background()
	name := "baseten-go-e2e-" + time.Now().Format("20060102-150405")

	created, err := client.Create(ctx, &sandbox.CreateSandboxRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, deleteErr := client.Delete(ctx, name)
		if deleteErr != nil {
			t.Logf("cleanup delete failed: %v", deleteErr)
		}
	})
	if created.Name() != name {
		t.Fatalf("created %q, want %q", created.Name(), name)
	}

	// Creation returns before the sandbox reaches its URL and DEPLOYED
	// status, so poll until the record is ready to work in.
	var instance *sandbox.Sandbox
	deadline := time.Now().Add(5 * time.Minute)
	for {
		info, err := client.GetInfo(ctx, name, nil)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status == "DEPLOYED" && info.URL != "" {
			instance, err = client.SandboxFromInfo(*info)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if info.Status == "FAILED" {
			t.Fatalf("sandbox %s failed to deploy", name)
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox %s never deployed (status %s)", name, info.Status)
		}
		time.Sleep(5 * time.Second)
	}

	info, err := instance.Process().Exec(ctx, &sandbox.ExecOptions{
		Command:           "echo hello",
		WaitForCompletion: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.ExitCode != 0 || strings.TrimSpace(info.Stdout) != "hello" {
		t.Errorf("exec exit %d stdout %q", info.ExitCode, info.Stdout)
	}

	var listed bool
	for candidate, err := range client.List(ctx, &sandbox.ListSandboxesRequest{Query: name}) {
		if err != nil {
			t.Fatal(err)
		}
		if candidate.Name == name {
			listed = true
		}
	}
	if !listed {
		t.Errorf("created sandbox %s not listed", name)
	}
}
