package client_test

import (
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/internal/require"
)

var (
	e2eAPIKey  = os.Getenv("BASETEN_E2E_TEST_API_KEY")
	e2eDomain  = os.Getenv("BASETEN_E2E_TEST_DOMAIN")
	e2eModelID = os.Getenv("BASETEN_E2E_TEST_MODEL_ID")
)

func skipOrFailE2E(t *testing.T) {
	t.Helper()
	if e2eAPIKey == "" {
		t.Skip("BASETEN_E2E_TEST_API_KEY not set")
	}
	if e2eDomain == "" {
		require.Fail(t, "BASETEN_E2E_TEST_API_KEY is set but BASETEN_E2E_TEST_DOMAIN is missing")
	}
	if e2eModelID == "" {
		require.Fail(t, "BASETEN_E2E_TEST_API_KEY is set but BASETEN_E2E_TEST_MODEL_ID is missing")
	}
}

func newE2EManagementClient(t *testing.T) *client.ManagementClient {
	t.Helper()
	c, err := client.NewManagementClient(client.ManagementClientOptions{
		APIKey:  e2eAPIKey,
		BaseURL: fmt.Sprintf("https://api.%s", e2eDomain),
	})
	require.NoError(t, err)
	return c
}

func newE2EInferenceClient(t *testing.T) *client.InferenceClient {
	t.Helper()
	c, err := client.NewInferenceClient(client.InferenceClientOptions{
		APIKey:  e2eAPIKey,
		BaseURL: fmt.Sprintf("https://model-%s.api.%s", e2eModelID, e2eDomain),
	})
	require.NoError(t, err)
	return c
}

func TestE2EListModels(t *testing.T) {
	skipOrFailE2E(t)
	c := newE2EManagementClient(t)

	models, err := c.API().GetModels(t.Context())
	require.NoError(t, err)

	found := false
	for _, m := range models.Models {
		if m.Id == e2eModelID {
			found = true
			break
		}
	}
	require.True(t, found, "model %s not found in list", e2eModelID)
}

func TestE2EGetModel(t *testing.T) {
	skipOrFailE2E(t)
	c := newE2EManagementClient(t)

	model, err := c.API().GetModelsModelId(t.Context(), e2eModelID)
	require.NoError(t, err)
	require.Equal(t, e2eModelID, model.Id)
}

func TestE2EGetModelNotFound(t *testing.T) {
	skipOrFailE2E(t)
	c := newE2EManagementClient(t)

	_, err := c.API().GetModelsModelId(t.Context(), "nonexistent-model-id")
	require.Error(t, err)
	respErr := require.ErrorAs[*managementapi.ResponseError](t, err)
	require.Equal(t, 404, respErr.StatusCode)
}

func TestE2EListDeployments(t *testing.T) {
	skipOrFailE2E(t)
	c := newE2EManagementClient(t)

	deployments, err := c.API().GetModelsDeployments(t.Context(), e2eModelID)
	require.NoError(t, err)
	require.True(t, len(deployments.Deployments) > 0, "expected at least one deployment")
}

func TestE2EInference(t *testing.T) {
	skipOrFailE2E(t)
	c := newE2EInferenceClient(t)

	result, err := c.API().PredictProduction(t.Context(), map[string]any{"prompt": "hello"})
	require.NoError(t, err)
	require.NotNil(t, result)
}

func TestE2EAPIKeyCRUD(t *testing.T) {
	skipOrFailE2E(t)
	c := newE2EManagementClient(t)

	var randBytes [16]byte
	_, err := rand.Read(randBytes[:])
	require.NoError(t, err)
	keyName := fmt.Sprintf("e2e-test-%x", randBytes)
	var createdPrefix string

	defer func() {
		if createdPrefix != "" {
			_, _ = c.API().DeleteApiKeys(t.Context(), createdPrefix)
		}
	}()

	created, err := c.API().PostApiKeys(t.Context(), managementapi.CreateAPIKeyRequest{
		Name: &keyName,
		Type: managementapi.PERSONAL,
	})
	require.NoError(t, err)
	createdPrefix = strings.SplitN(created.ApiKey, ".", 2)[0]

	keys, err := c.API().GetApiKeys(t.Context())
	require.NoError(t, err)
	found := false
	for _, k := range keys.Keys {
		if k.Name != nil && *k.Name == keyName {
			found = true
			break
		}
	}
	require.True(t, found, "created API key not found in list")

	tombstone, err := c.API().DeleteApiKeys(t.Context(), createdPrefix)
	require.NoError(t, err)
	require.Equal(t, createdPrefix, tombstone.Prefix)

	keys, err = c.API().GetApiKeys(t.Context())
	require.NoError(t, err)
	for _, k := range keys.Keys {
		require.NotEqual(t, createdPrefix, k.Prefix)
	}
	createdPrefix = ""
}
