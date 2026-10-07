package sandbox

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/basetenlabs/baseten-go/client/managementapi"
)

// Status is the status of a sandbox. Other values may be added, so do not
// treat the constants as exhaustive.
type Status string

// Values for Status.
const (
	StatusDeploying    Status = "DEPLOYING"
	StatusDeployed     Status = "DEPLOYED"
	StatusFailed       Status = "FAILED"
	StatusTerminated   Status = "TERMINATED"
	StatusDeleting     Status = "DELETING"
	StatusDeactivating Status = "DEACTIVATING"
	StatusDeactivated  Status = "DEACTIVATED"
	StatusBuilding     Status = "BUILDING"
	StatusUploading    Status = "UPLOADING"
	StatusArchiving    Status = "ARCHIVING"
	StatusArchived     Status = "ARCHIVED"
	StatusUnarchiving  Status = "UNARCHIVING"
)

// Info is a sandbox's record.
type Info struct {
	// Name is the sandbox's unique name.
	Name string

	// URL is the base URL of the sandbox's execution API. Empty until the
	// server assigns one.
	URL string

	// Status is the sandbox's status.
	Status Status

	// Image is the image reference, including its tag.
	Image string

	// Memory is the memory allocation in megabytes, which also sets the CPU
	// allocation.
	Memory int

	// Region is where the sandbox runs.
	Region string

	// Envs are the environment variables injected into the sandbox, by name.
	// Values come back masked unless revealed with
	// [GetInfoOptions.ShowSecrets], so sending them back in an update
	// overwrites the real values with the mask.
	Envs map[string]EnvValue

	// Labels are the key-value pairs for organizing and filtering.
	Labels map[string]string

	// ExternalID is the caller-owned identifier for external lookups.
	ExternalID string

	// Lifecycle is when the sandbox expires, nil when it has no lifecycle.
	Lifecycle *Lifecycle

	// Ports are the ports the sandbox exposes.
	Ports []Port

	// Network is the sandbox's network configuration, nil when it has none.
	Network *Network

	// CreatedAt is when the sandbox was created.
	CreatedAt time.Time

	// UpdatedAt is when the sandbox was last updated, zero when it never was.
	UpdatedAt time.Time

	// CreatedBy is who created the sandbox.
	CreatedBy string

	// UpdatedBy is who last updated the sandbox.
	UpdatedBy string

	// LastUsedAt is when the sandbox was last used, zero when it never was.
	LastUsedAt time.Time

	// ExpiresIn is how long until the sandbox expires, zero when it does not.
	ExpiresIn time.Duration
}

// EnvValue is the value of one environment variable.
type EnvValue struct {
	// Value of the variable.
	Value string

	// NonSecret marks the value as not secret, so it is returned unmasked.
	// False, the default, keeps it secret.
	NonSecret bool
}

// Lifecycle is when a sandbox expires and how long it is kept afterwards.
type Lifecycle struct {
	// ExpirationPolicies are the rules for when the sandbox expires.
	ExpirationPolicies []ExpirationPolicy

	// TerminatedRetention is how long a terminated sandbox is kept before it
	// is deleted. Zero uses the server's default.
	TerminatedRetention time.Duration
}

// ExpirationPolicyType is the kind of an expiration policy. Other values may
// be added, so do not treat the constants as exhaustive.
type ExpirationPolicyType string

// Values for ExpirationPolicyType.
const (
	// ExpirationPolicyTypeTTLIdle expires the sandbox after it has been idle
	// for [ExpirationPolicy.After].
	ExpirationPolicyTypeTTLIdle ExpirationPolicyType = "TTL_IDLE"

	// ExpirationPolicyTypeTTLMaxAge expires the sandbox
	// [ExpirationPolicy.After] its creation.
	ExpirationPolicyTypeTTLMaxAge ExpirationPolicyType = "TTL_MAX_AGE"

	// ExpirationPolicyTypeDate expires the sandbox at [ExpirationPolicy.At].
	ExpirationPolicyTypeDate ExpirationPolicyType = "DATE"
)

// ExpirationAction is what happens when a sandbox expires. Other values may
// be added, so do not treat the constants as exhaustive.
type ExpirationAction string

// Values for ExpirationAction.
const (
	// ExpirationActionDelete deletes the sandbox.
	ExpirationActionDelete ExpirationAction = "DELETE"
)

// ExpirationPolicy is one rule for when a sandbox expires.
type ExpirationPolicy struct {
	// Type is the kind of policy, which decides whether After or At applies.
	Type ExpirationPolicyType

	// After is the duration for [ExpirationPolicyTypeTTLIdle] and
	// [ExpirationPolicyTypeTTLMaxAge].
	After time.Duration

	// At is the time for [ExpirationPolicyTypeDate].
	At time.Time

	// Action is what happens on expiry. Empty sends [ExpirationActionDelete].
	Action ExpirationAction
}

// PortProtocol is the protocol of a port. Other values may be added, so do
// not treat the constants as exhaustive.
type PortProtocol string

// Values for PortProtocol.
const (
	PortProtocolHTTP PortProtocol = "HTTP"
	PortProtocolTCP  PortProtocol = "TCP"
	PortProtocolUDP  PortProtocol = "UDP"
	PortProtocolTLS  PortProtocol = "TLS"
)

// Port is a port a sandbox exposes.
type Port struct {
	// Target is the port number in the sandbox.
	Target int

	// Name is the port's name.
	Name string

	// Protocol is the port's protocol. Empty uses the server's default.
	Protocol PortProtocol
}

// Network is a sandbox's network configuration. It is set at creation and
// cannot be updated.
type Network struct {
	// Subnet is the subnet the sandbox joins. Empty uses the server's
	// default.
	Subnet string

	// Proxy routes the sandbox's outbound traffic, nil for none.
	Proxy *NetworkProxy
}

// NetworkProxy controls a sandbox's outbound traffic.
type NetworkProxy struct {
	// AllowedDomains are the only domains the sandbox may reach, when set.
	AllowedDomains []string

	// ForbiddenDomains are domains the sandbox may not reach.
	ForbiddenDomains []string

	// Bypass are domains whose traffic goes direct instead of through the
	// proxy.
	Bypass []string

	// Routing adds headers, body fields, and secrets to requests for
	// matching destinations.
	Routing []NetworkProxyRoute
}

// NetworkProxyRoute adds to outbound requests for matching destinations.
type NetworkProxyRoute struct {
	// Destinations are the domains this route applies to.
	Destinations []string

	// Headers are added to matching requests.
	Headers map[string]string

	// Body fields are added to matching requests.
	Body map[string]string

	// Secrets are added to matching requests. Write-only: never returned.
	Secrets map[string]string
}

func infoFromAPI(sandbox *managementapi.Sandbox) (Info, error) {
	info := Info{
		Name:       deref(sandbox.Name),
		URL:        deref(sandbox.Url),
		Status:     Status(sandbox.Status),
		Image:      deref(sandbox.Image),
		Memory:     deref(sandbox.Memory),
		Region:     deref(sandbox.Region),
		Envs:       envsFromAPI(deref(sandbox.Envs)),
		Labels:     map[string]string{},
		ExternalID: deref(sandbox.ExternalId),
		Ports:      portsFromAPI(deref(sandbox.Ports)),
		CreatedAt:  deref(sandbox.CreatedAt),
		UpdatedAt:  deref(sandbox.UpdatedAt),
		CreatedBy:  deref(sandbox.CreatedBy),
		UpdatedBy:  deref(sandbox.UpdatedBy),
		LastUsedAt: deref(sandbox.LastUsedAt),
		ExpiresIn:  time.Duration(deref(sandbox.ExpiresIn)) * time.Second,
	}
	for key, value := range deref(sandbox.Labels) {
		info.Labels[key] = value
	}
	if sandbox.Lifecycle != nil {
		lifecycle, err := lifecycleFromAPI(sandbox.Lifecycle)
		if err != nil {
			return Info{}, err
		}
		info.Lifecycle = &lifecycle
	}
	if sandbox.Network != nil {
		network := networkFromAPI(sandbox.Network)
		info.Network = &network
	}
	return info, nil
}

func envsToAPI(envs map[string]EnvValue) *[]managementapi.SandboxEnv {
	if envs == nil {
		return nil
	}
	result := make([]managementapi.SandboxEnv, 0, len(envs))
	for name, env := range envs {
		secret := !env.NonSecret
		result = append(result, managementapi.SandboxEnv{Name: &name, Value: &env.Value, Secret: &secret})
	}
	return &result
}

func envsFromAPI(envs []managementapi.SandboxEnv) map[string]EnvValue {
	result := make(map[string]EnvValue, len(envs))
	for _, env := range envs {
		if env.Name == nil {
			continue
		}
		// Unset means secret, the server's default.
		result[*env.Name] = EnvValue{Value: deref(env.Value), NonSecret: env.Secret != nil && !*env.Secret}
	}
	return result
}

func labelsToAPI(labels map[string]string) *managementapi.SandboxMetadataLabels {
	if labels == nil {
		return nil
	}
	result := managementapi.SandboxMetadataLabels(labels)
	return &result
}

// expirationPolicyWire is every expiration policy's shape on the wire. The
// generated union has one type per policy, each allowing only the known
// values, while unknown types and actions must pass through.
type expirationPolicyWire struct {
	Type   string `json:"type"`
	Action string `json:"action"`
	Value  string `json:"value"`
}

func lifecycleToAPI(lifecycle *Lifecycle) (*managementapi.SandboxLifecycle, error) {
	if lifecycle == nil {
		return nil, nil
	}
	result := &managementapi.SandboxLifecycle{}
	if lifecycle.TerminatedRetention != 0 {
		retention := formatDuration(lifecycle.TerminatedRetention)
		result.TerminatedRetention = &retention
	}
	if lifecycle.ExpirationPolicies != nil {
		policies := make([]managementapi.SandboxExpirationPolicy, 0, len(lifecycle.ExpirationPolicies))
		for _, policy := range lifecycle.ExpirationPolicies {
			wire := expirationPolicyWire{Type: string(policy.Type), Action: string(policy.Action)}
			if wire.Action == "" {
				wire.Action = string(ExpirationActionDelete)
			}
			if policy.Type == ExpirationPolicyTypeDate {
				wire.Value = policy.At.Format(time.RFC3339Nano)
			} else {
				wire.Value = formatDuration(policy.After)
			}
			raw, err := json.Marshal(wire)
			if err != nil {
				return nil, err
			}
			var apiPolicy managementapi.SandboxExpirationPolicy
			if err := apiPolicy.UnmarshalJSON(raw); err != nil {
				return nil, err
			}
			policies = append(policies, apiPolicy)
		}
		result.ExpirationPolicies = &policies
	}
	return result, nil
}

func lifecycleFromAPI(lifecycle *managementapi.SandboxLifecycle) (Lifecycle, error) {
	var result Lifecycle
	if retention := deref(lifecycle.TerminatedRetention); retention != "" {
		d, err := parseDuration(retention, "lifecycle terminated retention")
		if err != nil {
			return Lifecycle{}, err
		}
		result.TerminatedRetention = d
	}
	for _, apiPolicy := range deref(lifecycle.ExpirationPolicies) {
		raw, err := apiPolicy.MarshalJSON()
		if err != nil {
			return Lifecycle{}, err
		}
		var wire expirationPolicyWire
		if err := json.Unmarshal(raw, &wire); err != nil {
			return Lifecycle{}, err
		}
		policy := ExpirationPolicy{Type: ExpirationPolicyType(wire.Type), Action: ExpirationAction(wire.Action)}
		if policy.Type == ExpirationPolicyTypeDate {
			if policy.At, err = time.Parse(time.RFC3339Nano, wire.Value); err != nil {
				return Lifecycle{}, fmt.Errorf("DATE expiration policy is not a valid time: %q", wire.Value)
			}
		} else if policy.After, err = parseDuration(wire.Value, wire.Type+" expiration policy"); err != nil {
			return Lifecycle{}, err
		}
		result.ExpirationPolicies = append(result.ExpirationPolicies, policy)
	}
	return result, nil
}

func portsToAPI(ports []Port) *managementapi.SandboxPorts {
	if ports == nil {
		return nil
	}
	result := make(managementapi.SandboxPorts, 0, len(ports))
	for _, port := range ports {
		result = append(result, managementapi.SandboxPort{
			Target:   port.Target,
			Name:     optional(port.Name),
			Protocol: optional(managementapi.SandboxPortProtocol(port.Protocol)),
		})
	}
	return &result
}

func portsFromAPI(ports []managementapi.SandboxPort) []Port {
	result := make([]Port, 0, len(ports))
	for _, port := range ports {
		result = append(result, Port{
			Target:   port.Target,
			Name:     deref(port.Name),
			Protocol: PortProtocol(deref(port.Protocol)),
		})
	}
	return result
}

func networkToAPI(network *Network) *managementapi.SandboxNetwork {
	if network == nil {
		return nil
	}
	result := &managementapi.SandboxNetwork{Subnet: optional(network.Subnet)}
	if proxy := network.Proxy; proxy != nil {
		apiProxy := &managementapi.SandboxProxyConfig{
			AllowedDomains:   optionalSlice(proxy.AllowedDomains),
			ForbiddenDomains: optionalSlice(proxy.ForbiddenDomains),
			Bypass:           optionalSlice(proxy.Bypass),
		}
		if proxy.Routing != nil {
			routes := make([]managementapi.SandboxProxyTarget, 0, len(proxy.Routing))
			for _, route := range proxy.Routing {
				routes = append(routes, managementapi.SandboxProxyTarget{
					Destinations: optionalSlice(route.Destinations),
					Headers:      optionalMap(route.Headers),
					Body:         optionalMap(route.Body),
					Secrets:      optionalMap(route.Secrets),
				})
			}
			apiProxy.Routing = &routes
		}
		result.Proxy = apiProxy
	}
	return result
}

func networkFromAPI(network *managementapi.SandboxNetwork) Network {
	result := Network{Subnet: deref(network.Subnet)}
	if proxy := network.Proxy; proxy != nil {
		result.Proxy = &NetworkProxy{
			AllowedDomains:   deref(proxy.AllowedDomains),
			ForbiddenDomains: deref(proxy.ForbiddenDomains),
			Bypass:           deref(proxy.Bypass),
		}
		for _, route := range deref(proxy.Routing) {
			result.Proxy.Routing = append(result.Proxy.Routing, NetworkProxyRoute{
				Destinations: deref(route.Destinations),
				Headers:      deref(route.Headers),
				Body:         deref(route.Body),
				Secrets:      deref(route.Secrets),
			})
		}
	}
	return result
}
