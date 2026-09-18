package client_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/internal/require"
	"reflect"
	"testing"
)

// All three generated unions must preserve the same wire discriminator.
type routeTargetSetter interface {
	FromRouteTargetBasetenModelAPI(managementapi.RouteTargetBasetenModelAPI) error
	FromRouteTargetAnthropic(managementapi.RouteTargetAnthropic) error
	FromRouteTargetOpenAI(managementapi.RouteTargetOpenAI) error
	FromRouteTargetXAI(managementapi.RouteTargetXAI) error
	FromRouteTargetVertex(managementapi.RouteTargetVertex) error
	FromRouteTargetOpenAICompatible(managementapi.RouteTargetOpenAICompatible) error
}

func TestManagementRouteTargets(t *testing.T) {
	tests := []struct {
		name, wire string
		set        func(routeTargetSetter) error
		want       any
	}{
		{name: "BASETEN_MODEL_API", wire: `{"type":"BASETEN_MODEL_API","model_api":"zai-org/GLM-5.3"}`,
			set: func(u routeTargetSetter) error {
				return u.FromRouteTargetBasetenModelAPI(managementapi.RouteTargetBasetenModelAPI{ModelApi: "zai-org/GLM-5.3"})
			},
			want: managementapi.RouteTargetBasetenModelAPI{Type: "BASETEN_MODEL_API", ModelApi: "zai-org/GLM-5.3"}},
		{name: "ANTHROPIC", wire: `{"type":"ANTHROPIC","model":"claude","secret_name":"provider-key"}`,
			set: func(u routeTargetSetter) error {
				return u.FromRouteTargetAnthropic(managementapi.RouteTargetAnthropic{Model: "claude", SecretName: "provider-key"})
			},
			want: managementapi.RouteTargetAnthropic{Type: "ANTHROPIC", Model: "claude", SecretName: "provider-key"}},
		{name: "OPENAI", wire: `{"type":"OPENAI","model":"gpt","secret_name":"provider-key"}`,
			set: func(u routeTargetSetter) error {
				return u.FromRouteTargetOpenAI(managementapi.RouteTargetOpenAI{Model: "gpt", SecretName: "provider-key"})
			},
			want: managementapi.RouteTargetOpenAI{Type: "OPENAI", Model: "gpt", SecretName: "provider-key"}},
		{name: "XAI", wire: `{"type":"XAI","model":"grok","secret_name":"provider-key"}`,
			set: func(u routeTargetSetter) error {
				return u.FromRouteTargetXAI(managementapi.RouteTargetXAI{Model: "grok", SecretName: "provider-key"})
			},
			want: managementapi.RouteTargetXAI{Type: "XAI", Model: "grok", SecretName: "provider-key"}},
		{name: "VERTEX", wire: `{"type":"VERTEX","model":"gemini","secret_name":"provider-key","vertex_config":{"project_id":"project","location":"us-central1"}}`,
			set: func(u routeTargetSetter) error {
				return u.FromRouteTargetVertex(managementapi.RouteTargetVertex{Model: "gemini", SecretName: "provider-key", VertexConfig: managementapi.VertexTargetConfig{ProjectId: "project", Location: "us-central1"}})
			},
			want: managementapi.RouteTargetVertex{Type: "VERTEX", Model: "gemini", SecretName: "provider-key", VertexConfig: managementapi.VertexTargetConfig{ProjectId: "project", Location: "us-central1"}}},
		{name: "OPENAI_COMPATIBLE", wire: `{"type":"OPENAI_COMPATIBLE","model":"custom","secret_name":"provider-key","base_url":"https://example.com/v1"}`,
			set: func(u routeTargetSetter) error {
				return u.FromRouteTargetOpenAICompatible(managementapi.RouteTargetOpenAICompatible{Model: "custom", SecretName: "provider-key", BaseUrl: "https://example.com/v1"})
			},
			want: managementapi.RouteTargetOpenAICompatible{Type: "OPENAI_COMPATIBLE", Model: "custom", SecretName: "provider-key", BaseUrl: "https://example.com/v1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var create managementapi.CreateRouteRequest_Target
			var update managementapi.UpdateRouteRequest_Target
			var response managementapi.Route_Target
			for _, u := range []routeTargetSetter{&create, &update, &response} {
				require.NoError(t, tc.set(u))
				encoded, err := json.Marshal(u)
				require.NoError(t, err)
				assertRouteJSON(t, tc.wire, string(encoded))
			}
			fixture := json.RawMessage(fmt.Sprintf(`{"id":"route123","name":"acme/assistant","team_id":"team123","display_name":"Assistant","description":"","invoke_url":"https://inference.baseten.co/v1","created_at":"2026-09-18T00:00:00Z","target":%s}`, tc.wire))
			var cap requestCapture
			api := newManagementClient(t, newTestServer(t, 200, fixture, &cap))
			created, err := api.PostRoutes(context.Background(), managementapi.CreateRouteRequest{Name: "acme/assistant", TeamId: "team123", Target: create})
			require.NoError(t, err)
			require.Equal(t, "POST", cap.Method)
			require.Equal(t, "/v1/routes", cap.Path)
			require.Equal(t, "Bearer test-key", cap.Header.Get("Authorization"))
			assertRouteJSON(t, `{"name":"acme/assistant","team_id":"team123","target":`+tc.wire+`}`, cap.Body)
			updated, err := api.PatchRoutes(context.Background(), "route123", managementapi.UpdateRouteRequest{Target: &update})
			require.NoError(t, err)
			require.Equal(t, "PATCH", cap.Method)
			require.Equal(t, "/v1/routes/route123", cap.Path)
			assertRouteJSON(t, `{"target":`+tc.wire+`}`, cap.Body)
			fetched, err := api.GetRoutesRouteId(context.Background(), "route123")
			require.NoError(t, err)
			require.Equal(t, "GET", cap.Method)
			require.Equal(t, "/v1/routes/route123", cap.Path)
			for _, route := range []*managementapi.Route{created, updated, fetched} {
				value, err := route.Target.ValueByDiscriminator()
				require.NoError(t, err)
				require.True(t, reflect.DeepEqual(tc.want, value), "want %#v, got %#v", tc.want, value)
				require.Equal(t, "route123", route.Id)
			}
		})
	}
}
func assertRouteJSON(t *testing.T, want, got string) {
	t.Helper()
	var expected, actual any
	require.NoError(t, json.Unmarshal([]byte(want), &expected))
	require.NoError(t, json.Unmarshal([]byte(got), &actual))
	require.True(t, reflect.DeepEqual(expected, actual), "want %s, got %s", want, got)
}
func TestManagementRouteMetadata(t *testing.T) {
	empty, label := "", "Assistant"
	for _, tc := range []struct {
		name, want string
		body       managementapi.UpdateRouteRequest
	}{
		{"omitted", `{}`, managementapi.UpdateRouteRequest{}},
		{"clear description", `{"description":""}`, managementapi.UpdateRouteRequest{Description: &empty}},
		{"label only", `{"display_name":"Assistant"}`, managementapi.UpdateRouteRequest{DisplayName: &label}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Test serialization only: the backend rejects an entirely empty update.
			encoded, err := json.Marshal(tc.body)
			require.NoError(t, err)
			assertRouteJSON(t, tc.want, string(encoded))
		})
	}
}
func TestManagementRoutePaginationAndDelete(t *testing.T) {
	var cap requestCapture
	next, team, name, limit := "opaque+/=cursor", "team123", "acme/assistant", 2
	api := newManagementClient(t, newTestServer(t, 200, json.RawMessage(`{"items":[],"pagination":{"cursor":"opaque+/=cursor","has_more":true}}`), &cap))
	page, err := api.GetRoutes(context.Background(), managementapi.GetV1RoutesParams{TeamId: &team, Name: &name, Limit: &limit})
	require.NoError(t, err)
	require.Equal(t, "GET", cap.Method)
	require.Equal(t, "/v1/routes", cap.Path)
	require.Equal(t, team, cap.Query.Get("team_id"))
	require.Equal(t, name, cap.Query.Get("name"))
	require.Equal(t, "2", cap.Query.Get("limit"))
	require.Equal(t, next, *page.Pagination.Cursor)
	_, err = api.GetRoutes(context.Background(), managementapi.GetV1RoutesParams{Cursor: page.Pagination.Cursor})
	require.NoError(t, err)
	require.Equal(t, next, cap.Query.Get("cursor"))
	require.Equal(t, 1, len(cap.Query))
	api = newManagementClient(t, newTestServer(t, 200, json.RawMessage(`{"id":"route123","name":"acme/assistant"}`), &cap))
	deleted, err := api.DeleteRoutes(context.Background(), "route/123")
	require.NoError(t, err)
	require.Equal(t, "DELETE", cap.Method)
	require.Equal(t, "/v1/routes/route%2F123", cap.RawPath)
	require.Equal(t, "route123", deleted.Id)
	require.Equal(t, name, deleted.Name)
}
func TestManagementRouteErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 422, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			api := newManagementClient(t, newTestServer(t, status, map[string]any{"error": "route failure"}, nil))
			calls := []func() error{
				func() error { _, e := api.GetRoutes(context.Background(), managementapi.GetV1RoutesParams{}); return e },
				func() error { _, e := api.GetRoutesRouteId(context.Background(), "route123"); return e },
				func() error {
					_, e := api.PostRoutes(context.Background(), managementapi.CreateRouteRequest{})
					return e
				},
				func() error {
					_, e := api.PatchRoutes(context.Background(), "route123", managementapi.UpdateRouteRequest{})
					return e
				},
				func() error { _, e := api.DeleteRoutes(context.Background(), "route123"); return e },
			}
			for _, call := range calls {
				response := require.ErrorAs[*managementapi.ResponseError](t, call())
				require.Equal(t, status, response.StatusCode)
				require.Contains(t, response.Body, "route failure")
			}
		})
	}
}
