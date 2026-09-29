// Package client provides Go clients for the Baseten management, inference,
// and sandbox execution APIs.
//
// Use [NewManagementClient] to interact with the management API (models,
// deployments, sandboxes, etc.) and [NewInferenceClient] to call deployed models
// and chains. [NewSandboxClient] connects directly to an execution URL using an
// existing sandbox token. The caller manages token expiry and sandbox readiness.
//
// Each low-level client exposes the generated API via its API method. Raw
// response bodies must be closed by the caller, including streams stopped early.
package client
