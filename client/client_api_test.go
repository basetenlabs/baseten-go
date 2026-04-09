package client_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

	resp, err := api.GetModels(context.Background())
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.Models, 1)
	require.Equal(t, "my-model", resp.Models[0].Name)
	require.Equal(t, "GET", cap.Method)
	require.Equal(t, "/v1/models", cap.Path)
	require.Equal(t, "Api-Key test-key", cap.Header.Get("Authorization"))
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

func TestManagementResponseError(t *testing.T) {
	srv := newTestServer(t, 500, map[string]any{"detail": "boom"}, nil)
	api := newManagementClient(t, srv)

	_, err := api.GetModels(context.Background())
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
