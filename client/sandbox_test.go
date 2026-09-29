package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/client"
)

func TestSandboxClientValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts client.SandboxClientOptions
	}{
		{"missing token", client.SandboxClientOptions{BaseURL: "https://sandbox.invalid"}},
		{"conflicting auth", client.SandboxClientOptions{BaseURL: "https://sandbox.invalid", Token: "token", DeferAuth: true}},
		{"missing URL", client.SandboxClientOptions{Token: "token"}},
		{"relative URL", client.SandboxClientOptions{BaseURL: "/sandbox", Token: "token"}},
		{"URL credentials", client.SandboxClientOptions{BaseURL: "https://user:password@sandbox.invalid", Token: "token"}},
		{"URL query", client.SandboxClientOptions{BaseURL: "https://sandbox.invalid?token=secret", Token: "token"}},
		{"URL fragment", client.SandboxClientOptions{BaseURL: "https://sandbox.invalid#fragment", Token: "token"}},
		{"unsupported scheme", client.SandboxClientOptions{BaseURL: "ftp://sandbox.invalid", Token: "token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.NewSandboxClient(tc.opts); err == nil {
				t.Fatal("expected invalid options to be rejected")
			}
		})
	}
}

func TestSandboxClientSendsClonedHeaders(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	headers := http.Header{"X-Test": {"original"}}
	c, err := client.NewSandboxClient(client.SandboxClientOptions{
		Token: "test-token", BaseURL: server.URL + "/", Headers: headers, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	headers.Set("X-Test", "changed")
	if _, err := c.API().GetHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer test-token" || got.Get("X-Test") != "original" || !strings.HasPrefix(got.Get("User-Agent"), "baseten-go/") {
		t.Fatalf("unexpected request headers: auth present=%t, X-Test=%q, User-Agent=%q", got.Get("Authorization") != "", got.Get("X-Test"), got.Get("User-Agent"))
	}
	if headers.Get("Authorization") != "" {
		t.Fatal("constructor mutated caller headers")
	}
}

func TestSandboxClientDeferredAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	c, err := client.NewSandboxClient(client.SandboxClientOptions{BaseURL: server.URL, DeferAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.API().GetHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
}
