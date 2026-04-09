package client_test

import (
	"testing"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/internal/require"
)

func TestNewManagementClient(t *testing.T) {
	t.Run("RequiresAPIKey", func(t *testing.T) {
		_, err := client.NewManagementClient(client.ManagementClientOptions{})
		require.Error(t, err)
	})

	t.Run("Success", func(t *testing.T) {
		c, err := client.NewManagementClient(client.ManagementClientOptions{APIKey: "test-key"})
		require.NoError(t, err)
		require.NotNil(t, c.API())
	})
}
