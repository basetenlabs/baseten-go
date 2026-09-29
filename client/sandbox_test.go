package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/client"
)

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
		Token: "test-token", BaseURL: server.URL, Headers: headers, HTTPClient: server.Client(),
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

func TestSandboxClientEmptyToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	c, err := client.NewSandboxClient(client.SandboxClientOptions{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.API().GetHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxClientOptions(t *testing.T) {
	// Like the JavaScript constructor, construction passes the base URL through
	// without validating it or performing network requests.
	for _, baseURL := range []string{"", "/sandbox", "https://sandbox.invalid/"} {
		opts := client.SandboxClientOptions{BaseURL: baseURL, HTTPClient: http.DefaultClient, Headers: http.Header{"X-Test": {"original"}}}
		c, err := client.NewSandboxClient(opts)
		if err != nil {
			t.Fatal(err)
		}
		if c.API().BaseURL != baseURL || c.Options().BaseURL != baseURL || c.Options().HTTPClient != opts.HTTPClient {
			t.Fatal("constructor did not preserve supplied options")
		}
		// Options are a shallow copy, but the live client's headers are cloned.
		opts.Headers.Set("X-Test", "changed")
		if c.Options().Headers.Get("X-Test") != "changed" || c.API().Headers.Get("X-Test") != "original" {
			t.Fatal("unexpected options or live client header ownership")
		}
	}
}

func TestSandboxClientCustomAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, token, expected string
	}{
		{"empty token preserves custom authorization", "", "Custom credential"},
		{"token overrides custom authorization", "test-token", "Bearer test-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != tc.expected {
					t.Errorf("Authorization = %q, want %q", got, tc.expected)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"ok"}`))
			}))
			defer server.Close()
			c, err := client.NewSandboxClient(client.SandboxClientOptions{
				BaseURL: server.URL, Token: tc.token, HTTPClient: server.Client(),
				Headers: http.Header{"Authorization": {"Custom credential"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.API().GetHealth(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
