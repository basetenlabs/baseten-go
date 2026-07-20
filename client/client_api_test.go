package client_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/inferenceapi"
	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/internal/require"
)

type requestCapture struct {
	Method  string
	Path    string
	RawPath string
	Query   url.Values
	Header  http.Header
	Body    string
}

func newTestServer(t *testing.T, statusCode int, response any, capture *requestCapture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			body, _ := io.ReadAll(r.Body)
			rawPath := r.URL.RawPath
			if rawPath == "" {
				rawPath = r.URL.Path
			}
			*capture = requestCapture{
				Method:  r.Method,
				Path:    r.URL.Path,
				RawPath: rawPath,
				Query:   r.URL.Query(),
				Header:  r.Header,
				Body:    string(body),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if response != nil {
			json.NewEncoder(w).Encode(response)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newManagementClient(t *testing.T, srv *httptest.Server) *managementapi.Client {
	t.Helper()
	cl, err := client.NewManagementClient(client.ManagementClientOptions{
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)
	return cl.API()
}

func newInferenceClient(t *testing.T, srv *httptest.Server) *inferenceapi.Client {
	t.Helper()
	cl, err := client.NewInferenceClient(client.InferenceClientOptions{
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)
	return cl.API()
}

func TestManagementGetModels(t *testing.T) {
	var cap requestCapture
	srv := newTestServer(t, 200, map[string]any{
		"models": []map[string]any{
			{"name": "my-model", "created_at": "2024-01-01T00:00:00Z", "deployments_count": 2},
		},
	}, &cap)
	api := newManagementClient(t, srv)

	resp, err := api.GetModels(context.Background(), managementapi.GetV1ModelsParams{})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.Models, 1)
	require.Equal(t, "my-model", resp.Models[0].Name)
	require.Equal(t, "GET", cap.Method)
	require.Equal(t, "/v1/models", cap.Path)
	require.Equal(t, "Bearer test-key", cap.Header.Get("Authorization"))
}

func TestManagementPathParams(t *testing.T) {
	var cap requestCapture
	srv := newTestServer(t, 200, map[string]any{
		"name": "my-model", "created_at": "2024-01-01T00:00:00Z", "deployments_count": 0,
	}, &cap)
	api := newManagementClient(t, srv)

	_, err := api.GetModelsModelId(context.Background(), "abc/def")
	require.NoError(t, err)
	require.Equal(t, "/v1/models/abc%2Fdef", cap.RawPath)
}

func TestManagementPostBody(t *testing.T) {
	var cap requestCapture
	srv := newTestServer(t, 200, map[string]any{
		"name": "MY_SECRET", "created_at": "2024-01-01T00:00:00Z",
	}, &cap)
	api := newManagementClient(t, srv)

	resp, err := api.PostSecrets(context.Background(), managementapi.UpsertSecretRequest{
		Name:  "MY_SECRET",
		Value: "s3cret",
	})
	require.NoError(t, err)
	require.Equal(t, "MY_SECRET", resp.Name)
	require.Equal(t, "POST", cap.Method)
	require.Equal(t, "application/json", cap.Header.Get("Content-Type"))
	var body map[string]string
	require.NoError(t, json.Unmarshal([]byte(cap.Body), &body))
	require.MapEqual(t, body, "name", "MY_SECRET")
	require.MapEqual(t, body, "value", "s3cret")
}

func TestManagementGetAuditLogs(t *testing.T) {
	var cap requestCapture
	srv := newTestServer(t, 200, map[string]any{
		"items": []map[string]any{
			{
				"id":         "log-1",
				"created":    "2024-01-01T00:00:00Z",
				"actor":      map[string]any{"type": "USER", "email": "user@example.com"},
				"source":     "API",
				"event_type": "MODEL_DEPLOYED",
				"event_data": map[string]any{
					"event_type":             "MODEL_DEPLOYED",
					"model_id":               "model-123",
					"model_name":             "my-model",
					"deployment_id":          "deploy-456",
					"deployment_name":        "my-deployment",
					"environment_name":       "production",
					"scale_previous_to_zero": true,
					"trusted":                false,
					"publish":                true,
					// A field the client doesn't know about yet; must survive a
					// marshal round-trip since event_data is stored as raw JSON.
					"future_field": "future_value",
				},
			},
			{
				"id":         "log-2",
				"created":    "2024-01-02T00:00:00Z",
				"actor":      map[string]any{"type": "USER", "email": "user@example.com"},
				"source":     "API",
				"event_type": "MODEL_DELETED",
				"event_data": map[string]any{
					"event_type": "MODEL_DELETED",
					"model_id":   "model-123",
					"model_name": "my-model",
				},
			},
			{
				// An event type the client was generated before it existed.
				"id":         "log-3",
				"created":    "2024-01-03T00:00:00Z",
				"actor":      map[string]any{"type": "USER", "email": "user@example.com"},
				"source":     "API",
				"event_type": "QUANTUM_TELEPORTED",
				"event_data": map[string]any{
					"event_type":   "QUANTUM_TELEPORTED",
					"qubit_id":     "q-99",
					"entanglement": true,
				},
			},
		},
		"pagination": map[string]any{"has_more": false},
	}, &cap)
	api := newManagementClient(t, srv)

	limit := 50
	direction := managementapi.AuditLogSortDirection_DESC
	resp, err := api.GetAuditLogs(context.Background(), managementapi.GetV1AuditLogsParams{
		Limit:     &limit,
		Direction: &direction,
		EventTypeGroups: &[]managementapi.AuditLogEventTypeGroup{
			managementapi.AuditLogEventTypeGroup_DEPLOYED,
			managementapi.AuditLogEventTypeGroup_PROMOTED,
		},
		Sources: &[]managementapi.AuditLogSource{managementapi.AuditLogSource_UI},
		UserIds: &[]string{"u1", "u2"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.Items, 3)

	// Scalar query params render as a single value: a plain int (limit) and a
	// named-string enum (direction).
	require.Equal(t, "50", cap.Query.Get("limit"))
	require.Equal(t, "DESC", cap.Query.Get("direction"))

	// Slice query params explode into one repeated parameter per element. This
	// covers both named-element slices (event_type_groups, sources) and plain
	// []string (user_ids); the named-element case is the one that regressed to
	// a single "[A B]"-style value before encodeQuery handled slices generically.
	require.Equal(t, "DEPLOYED,PROMOTED", strings.Join(cap.Query["event_type_groups"], ","))
	require.Equal(t, "UI", strings.Join(cap.Query["sources"], ","))
	require.Equal(t, "u1,u2", strings.Join(cap.Query["user_ids"], ","))

	// A known event deserializes to its concrete type via the discriminator,
	// ignoring the unknown field.
	deployedEntry := resp.Items[0]
	require.Equal(t, managementapi.AuditLogEventType_MODEL_DEPLOYED, deployedEntry.EventType)
	value, err := deployedEntry.EventData.ValueByDiscriminator()
	require.NoError(t, err)
	deployed, ok := value.(managementapi.AuditLogEventModelDeployed)
	require.True(t, ok, "expected AuditLogEventModelDeployed, got %T", value)
	require.Equal(t, "model-123", deployed.ModelId)
	require.Equal(t, "production", *deployed.EnvironmentName)
	require.True(t, deployed.Publish, "expected publish to be true")

	// The unknown field survives a marshal round-trip: event_data is retained
	// as raw JSON rather than re-encoded from the typed struct.
	remarshaled, err := json.Marshal(deployedEntry.EventData)
	require.NoError(t, err)
	require.Contains(t, string(remarshaled), `"future_field":"future_value"`)

	// A second known event type resolves to its own concrete type.
	deletedEntry := resp.Items[1]
	require.Equal(t, managementapi.AuditLogEventType_MODEL_DELETED, deletedEntry.EventType)
	deletedValue, err := deletedEntry.EventData.ValueByDiscriminator()
	require.NoError(t, err)
	deleted, ok := deletedValue.(managementapi.AuditLogEventModelDeleted)
	require.True(t, ok, "expected AuditLogEventModelDeleted, got %T", deletedValue)
	require.Equal(t, "my-model", deleted.ModelName)

	// An unknown event type still deserializes at the response level; only
	// ValueByDiscriminator reports it can't resolve a concrete type. The raw
	// discriminator and the whole event_data payload remain accessible.
	unknownEntry := resp.Items[2]
	require.Equal(t, managementapi.AuditLogEventType("QUANTUM_TELEPORTED"), unknownEntry.EventType)
	_, err = unknownEntry.EventData.ValueByDiscriminator()
	require.Error(t, err)
	discriminator, err := unknownEntry.EventData.Discriminator()
	require.NoError(t, err)
	require.Equal(t, "QUANTUM_TELEPORTED", discriminator)
	unknownJSON, err := json.Marshal(unknownEntry.EventData)
	require.NoError(t, err)
	require.Contains(t, string(unknownJSON), `"qubit_id":"q-99"`)

	// The union round-trips the other direction too: From* stamps the
	// discriminator so ValueByDiscriminator resolves the concrete type again.
	var union managementapi.AuditLogEntry_EventData
	require.NoError(t, union.FromAuditLogEventModelDeployed(managementapi.AuditLogEventModelDeployed{
		ModelId:      "model-789",
		DeploymentId: "deploy-789",
	}))
	marshaled, err := json.Marshal(union)
	require.NoError(t, err)
	require.Contains(t, string(marshaled), `"event_type":"MODEL_DEPLOYED"`)
	roundTripped, err := union.ValueByDiscriminator()
	require.NoError(t, err)
	_, ok = roundTripped.(managementapi.AuditLogEventModelDeployed)
	require.True(t, ok, "expected AuditLogEventModelDeployed after round-trip, got %T", roundTripped)
}

func TestManagementResponseError(t *testing.T) {
	srv := newTestServer(t, 500, map[string]any{"detail": "boom"}, nil)
	api := newManagementClient(t, srv)

	_, err := api.GetModels(context.Background(), managementapi.GetV1ModelsParams{})
	respErr := require.ErrorAs[*managementapi.ResponseError](t, err)
	require.Equal(t, 500, respErr.StatusCode)
	require.Contains(t, respErr.Body, "boom")
}

func TestInferencePredictProduction(t *testing.T) {
	var cap requestCapture
	srv := newTestServer(t, 200, map[string]any{"result": 42}, &cap)
	api := newInferenceClient(t, srv)

	resp, err := api.PredictProduction(context.Background(), map[string]any{"input": "hello"})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, float64(42), (*resp)["result"].(float64))
	require.Equal(t, "POST", cap.Method)
	require.Equal(t, "/production/predict", cap.Path)
}

func TestInferenceAsyncPredict201(t *testing.T) {
	srv := newTestServer(t, 201, map[string]any{"request_id": "req-123"}, nil)
	api := newInferenceClient(t, srv)

	resp, err := api.AsyncPredictProduction(context.Background(), inferenceapi.AsyncPredictRequest{
		ModelInput: map[string]any{"prompt": "test"},
	})
	require.NoError(t, err)
	require.Equal(t, "req-123", resp.RequestId)
}

func TestInferenceTypedError(t *testing.T) {
	errMsg := "rate limited"
	srv := newTestServer(t, 429, map[string]any{
		"error": errMsg, "error_code": "rate_limited",
	}, nil)
	api := newInferenceClient(t, srv)

	_, err := api.PredictProduction(context.Background(), nil)
	typedErr := require.ErrorAs[*inferenceapi.ResponseErrorResponse](t, err)
	require.Equal(t, 429, typedErr.StatusCode)
	require.NotNil(t, typedErr.ErrorResponse.Error)
	require.Equal(t, errMsg, *typedErr.ErrorResponse.Error)
}

func TestInferenceUnknownErrorCodeFallback(t *testing.T) {
	// Status 418 is not in any errorCodes map, should fall back to ResponseError.
	srv := newTestServer(t, 418, map[string]any{"error": "teapot"}, nil)
	api := newInferenceClient(t, srv)

	_, err := api.PredictProduction(context.Background(), nil)
	respErr := require.ErrorAs[*inferenceapi.ResponseError](t, err)
	require.Equal(t, 418, respErr.StatusCode)
	require.Contains(t, respErr.Body, "teapot")
}

func TestInferenceWakeNoResponse(t *testing.T) {
	var cap requestCapture
	srv := newTestServer(t, 202, nil, &cap)
	api := newInferenceClient(t, srv)

	err := api.WakeProduction(context.Background())
	require.NoError(t, err)
	require.Equal(t, "POST", cap.Method)
	require.Equal(t, "/production/wake", cap.Path)
}

func TestInferenceWakeError(t *testing.T) {
	srv := newTestServer(t, 401, map[string]any{
		"error": "unauthorized", "error_code": "unauthorized",
	}, nil)
	api := newInferenceClient(t, srv)

	err := api.WakeProduction(context.Background())
	typedErr := require.ErrorAs[*inferenceapi.ResponseErrorResponse](t, err)
	require.Equal(t, 401, typedErr.StatusCode)
}
