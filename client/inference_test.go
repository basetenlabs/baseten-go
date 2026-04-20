package client_test

import (
	"testing"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/internal/require"
)

func TestNewInferenceClient(t *testing.T) {
	t.Run("RequiresAPIKey", func(t *testing.T) {
		_, err := client.NewInferenceClient(client.InferenceClientOptions{ModelID: "abc"})
		require.Error(t, err)
	})

	t.Run("RequiresModelOrChainOrBaseURL", func(t *testing.T) {
		_, err := client.NewInferenceClient(client.InferenceClientOptions{APIKey: "test-key"})
		require.Error(t, err)
	})

	t.Run("BaseURLMutuallyExclusiveWithModelID", func(t *testing.T) {
		_, err := client.NewInferenceClient(client.InferenceClientOptions{
			APIKey:  "test-key",
			BaseURL: "https://custom.example.com",
			ModelID: "abc",
		})
		require.Error(t, err)
	})

	t.Run("BaseURL", func(t *testing.T) {
		c, err := client.NewInferenceClient(client.InferenceClientOptions{
			APIKey:  "test-key",
			BaseURL: "https://custom.example.com",
		})
		require.NoError(t, err)
		require.NotNil(t, c.API())
	})

	t.Run("MutuallyExclusive", func(t *testing.T) {
		_, err := client.NewInferenceClient(client.InferenceClientOptions{
			APIKey:  "test-key",
			ModelID: "abc",
			ChainID: "def",
		})
		require.Error(t, err)
	})

	t.Run("ModelWithoutEnvironment", func(t *testing.T) {
		c, err := client.NewInferenceClient(client.InferenceClientOptions{
			APIKey:  "test-key",
			ModelID: "abc123",
		})
		require.NoError(t, err)
		require.NotNil(t, c.API())
	})

	t.Run("ModelWithEnvironment", func(t *testing.T) {
		c, err := client.NewInferenceClient(client.InferenceClientOptions{
			APIKey:      "test-key",
			ModelID:     "abc123",
			Environment: "prod-us",
		})
		require.NoError(t, err)
		require.NotNil(t, c.API())
	})

	t.Run("ChainWithoutEnvironment", func(t *testing.T) {
		c, err := client.NewInferenceClient(client.InferenceClientOptions{
			APIKey:  "test-key",
			ChainID: "def456",
		})
		require.NoError(t, err)
		require.NotNil(t, c.API())
	})

	t.Run("DeferAuthSucceedsWithoutAPIKey", func(t *testing.T) {
		c, err := client.NewInferenceClient(client.InferenceClientOptions{
			DeferAuth: true,
			ModelID:   "abc123",
		})
		require.NoError(t, err)
		require.NotNil(t, c.API())
	})

	t.Run("DeferAuthRejectsAPIKey", func(t *testing.T) {
		_, err := client.NewInferenceClient(client.InferenceClientOptions{
			APIKey:    "test-key",
			DeferAuth: true,
			ModelID:   "abc123",
		})
		require.Error(t, err)
	})
}
