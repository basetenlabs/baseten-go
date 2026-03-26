// Package client provides Go clients for the Baseten management and inference
// APIs.
//
// Use [NewManagementClient] to interact with the management API (models,
// deployments, secrets, etc.) and [NewInferenceClient] to call deployed models
// and chains.
//
// Both clients expose the underlying generated API client via their API()
// method for direct access to all low-level endpoints.
package client
