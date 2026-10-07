// Package client provides Go clients for the Baseten management, inference,
// and sandbox APIs.
//
// Use [NewManagementClient] to interact with the management API (models,
// deployments, secrets, etc.), [NewInferenceClient] to call deployed models
// and chains, and [NewSandboxClient] to call a single sandbox.
//
// All clients expose the underlying generated API client via their API()
// method for direct access to all low-level endpoints.
package client
