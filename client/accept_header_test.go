package client_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/basetenlabs/baseten-go/client/inferenceapi"
	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/client/sandboxapi"
)

func TestClientsPreserveAcceptValues(t *testing.T) {
	calls := map[string]func(context.Context, string, http.Header) error{
		"management": func(ctx context.Context, base string, headers http.Header) error {
			c := managementapi.Client{BaseURL: base, HTTPClient: http.DefaultClient, Headers: headers}
			_, err := c.GetModels(ctx, managementapi.GetV1ModelsParams{})
			return err
		},
		"inference": func(ctx context.Context, base string, headers http.Header) error {
			c := inferenceapi.Client{BaseURL: base, HTTPClient: http.DefaultClient, Headers: headers}
			_, err := c.PredictProduction(ctx, map[string]any{})
			return err
		},
		"sandbox": func(ctx context.Context, base string, headers http.Header) error {
			c := sandboxapi.Client{BaseURL: base, HTTPClient: http.DefaultClient, Headers: headers}
			_, err := c.GetHealth(ctx)
			return err
		},
		"sandbox_raw": func(ctx context.Context, base string, headers http.Header) error {
			c := sandboxapi.Client{BaseURL: base, HTTPClient: http.DefaultClient, Headers: headers}
			resp, err := c.GetFilesystemRaw(ctx, "/file", sandboxapi.GetFilesystemPathParams{}, sandboxapi.RawRequestOptions{})
			if err == nil {
				resp.Body.Close()
			}
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			for _, key := range []string{"Accept", "accept"} {
				t.Run(key, func(t *testing.T) {
					want := []string{"application/octet-stream", "application/json;q=0.5"}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if got := r.Header.Values("Accept"); !reflect.DeepEqual(got, want) {
							t.Errorf("Accept = %q, want %q", got, want)
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{}`)
					}))
					defer server.Close()
					headers := http.Header{key: append([]string(nil), want...)}
					if err := call(t.Context(), server.URL, headers); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(headers[key], want) {
						t.Fatal("caller headers mutated")
					}
				})
			}
		})
	}
}
