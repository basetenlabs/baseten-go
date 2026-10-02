package sandbox

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/basetenlabs/baseten-go/internal/require"
)

// apiSandbox decodes a control plane sandbox record from its wire JSON.
func apiSandbox(t *testing.T, record string) *managementapi.Sandbox {
	t.Helper()
	var sandbox managementapi.Sandbox
	require.NoError(t, json.Unmarshal([]byte(record), &sandbox))
	return &sandbox
}

func TestInfoFromAPI(t *testing.T) {
	t.Run("FullRecord", func(t *testing.T) {
		info, err := infoFromAPI(apiSandbox(t, `{
			"name": "sb", "url": "https://sb.example", "status": "DEPLOYED",
			"image": "baseten/base-image:latest", "memory": 4096, "region": "us-was-1", "enabled": true,
			"envs": [
				{"name": "PLAIN", "value": "v", "secret": false},
				{"name": "SECRET", "value": "****", "secret": true},
				{"name": "DEFAULT", "value": "****"}
			],
			"labels": {"team": "a"}, "external_id": "ext-1",
			"lifecycle": {
				"terminated_retention": "24h",
				"expiration_policies": [
					{"type": "TTL_IDLE", "action": "DELETE", "value": "3600000ms"},
					{"type": "DATE", "action": "DELETE", "value": "2030-01-01T00:00:00Z"},
					{"type": "TTL_MAX_AGE", "action": "ARCHIVE", "value": "7d"}
				]
			},
			"ports": [{"target": 8080, "name": "api", "protocol": "HTTP"}],
			"network": {"subnet": "", "proxy": {"allowed_domains": ["example.com"], "routing": [{"destinations": ["api.example.com"], "headers": {"X": "y"}}]}},
			"created_at": "2026-10-01T00:00:00Z", "expires_in": 90
		}`))
		require.NoError(t, err)
		require.Equal(t, "sb", info.Name)
		require.Equal(t, StatusDeployed, info.Status)
		require.Equal(t, 4096, info.Memory)
		require.True(t, info.Envs["PLAIN"].NonSecret, "secret false is non-secret")
		require.False(t, info.Envs["SECRET"].NonSecret, "secret true is secret")
		require.False(t, info.Envs["DEFAULT"].NonSecret, "unset secret is secret")
		require.MapEqual(t, info.Labels, "team", "a")
		require.Equal(t, 24*time.Hour, info.Lifecycle.TerminatedRetention)
		policies := info.Lifecycle.ExpirationPolicies
		require.Len(t, policies, 3)
		require.Equal(t, ExpirationPolicyTypeTTLIdle, policies[0].Type)
		require.Equal(t, time.Hour, policies[0].After)
		require.True(t, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Equal(policies[1].At), "DATE policy time")
		// Unknown actions pass through.
		require.Equal(t, ExpirationAction("ARCHIVE"), policies[2].Action)
		require.Equal(t, 7*day, policies[2].After)
		require.Equal(t, Port{Target: 8080, Name: "api", Protocol: PortProtocolHTTP}, info.Ports[0])
		require.Equal(t, "example.com", info.Network.Proxy.AllowedDomains[0])
		require.MapEqual(t, info.Network.Proxy.Routing[0].Headers, "X", "y")
		require.Equal(t, 90*time.Second, info.ExpiresIn)
		require.True(t, info.UpdatedAt.IsZero(), "unset UpdatedAt is zero")
	})

	t.Run("MinimalRecord", func(t *testing.T) {
		info, err := infoFromAPI(apiSandbox(t, `{"name": "sb", "status": "DEPLOYING"}`))
		require.NoError(t, err)
		require.Equal(t, "", info.URL)
		require.Nil(t, info.Lifecycle)
		require.Nil(t, info.Network)
		// Collections are always set, so callers can range and index them.
		require.True(t, info.Envs != nil && info.Labels != nil && info.Ports != nil, "collections must be set")
	})

	t.Run("BadDuration", func(t *testing.T) {
		_, err := infoFromAPI(apiSandbox(t, `{"name": "sb", "status": "DEPLOYED", "lifecycle": {"terminated_retention": "soon"}}`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "terminated retention")
	})
}

func TestLifecycleToAPI(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	apiLifecycle, err := lifecycleToAPI(&Lifecycle{
		TerminatedRetention: 36 * time.Hour,
		ExpirationPolicies: []ExpirationPolicy{
			{Type: ExpirationPolicyTypeTTLIdle, After: 10 * time.Minute},
			{Type: ExpirationPolicyTypeDate, At: at, Action: "ARCHIVE"},
		},
	})
	require.NoError(t, err)
	raw, err := json.Marshal(apiLifecycle)
	require.NoError(t, err)
	require.Equal(t,
		`{"expiration_policies":[{"type":"TTL_IDLE","action":"DELETE","value":"600000ms"},`+
			`{"type":"DATE","action":"ARCHIVE","value":"2030-01-01T00:00:00Z"}],"terminated_retention":"129600000ms"}`,
		string(raw))

	t.Run("RoundTrips", func(t *testing.T) {
		lifecycle, err := lifecycleFromAPI(apiLifecycle)
		require.NoError(t, err)
		require.Equal(t, 36*time.Hour, lifecycle.TerminatedRetention)
		require.Equal(t, 10*time.Minute, lifecycle.ExpirationPolicies[0].After)
		require.Equal(t, ExpirationActionDelete, lifecycle.ExpirationPolicies[0].Action)
		require.True(t, at.Equal(lifecycle.ExpirationPolicies[1].At), "DATE policy time")
	})

	t.Run("NilOmitted", func(t *testing.T) {
		apiLifecycle, err := lifecycleToAPI(nil)
		require.NoError(t, err)
		require.Nil(t, apiLifecycle)
	})
}

func TestEnvsToAPI(t *testing.T) {
	require.Nil(t, envsToAPI(nil))
	// An empty but set map is sent, replacing the existing variables.
	require.Len(t, *envsToAPI(map[string]EnvValue{}), 0)
	envs := *envsToAPI(map[string]EnvValue{"A": {Value: "1"}, "B": {Value: "2", NonSecret: true}})
	secrets := map[string]bool{}
	for _, env := range envs {
		secrets[*env.Name] = *env.Secret
	}
	require.MapEqual(t, secrets, "A", true)
	require.MapEqual(t, secrets, "B", false)
}

func TestNetworkToAPI(t *testing.T) {
	require.Nil(t, networkToAPI(nil))
	raw, err := json.Marshal(networkToAPI(&Network{Proxy: &NetworkProxy{
		Bypass:  []string{},
		Routing: []NetworkProxyRoute{{Destinations: []string{"api.example.com"}, Secrets: map[string]string{"K": "v"}}},
	}}))
	require.NoError(t, err)
	// Unset fields are omitted, set but empty ones sent.
	require.Equal(t, `{"proxy":{"bypass":[],"routing":[{"destinations":["api.example.com"],"secrets":{"K":"v"}}]}}`, string(raw))
}
