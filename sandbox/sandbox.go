package sandbox

import (
	"strings"

	"github.com/basetenlabs/baseten-go/client/sandboxapi"
)

// Sandbox works in one sandbox: running processes and reading and writing its
// filesystem.
//
// Get one from [Client.Create] or [Client.Get], or, without a call, from
// [Client.SandboxFromURL] when the sandbox's URL is already known. A Sandbox
// uses its client's credentials, HTTP client, headers, and retries.
type Sandbox struct {
	name    string
	url     string
	api     *sandboxapi.Client
	retries RetryOptions
	process *ProcessService
	fs      *FileSystemService
}

func (c *Client) newSandbox(name, url string) *Sandbox {
	url = strings.TrimRight(url, "/")
	sandbox := &Sandbox{
		name: name,
		url:  url,
		api: &sandboxapi.Client{
			BaseURL:    url,
			HTTPClient: &authenticatedClient{inner: c.httpClient, tokens: c.tokens},
			Headers:    c.headers,
		},
		retries: c.retries,
	}
	sandbox.process = newProcessService(sandbox)
	sandbox.fs = newFileSystemService(sandbox)
	return sandbox
}

// Name is the sandbox's name, empty for a Sandbox from
// [Client.SandboxFromURL].
func (s *Sandbox) Name() string {
	return s.name
}

// URL is the base URL of the sandbox's execution API.
func (s *Sandbox) URL() string {
	return s.url
}

// Process runs and inspects processes in the sandbox.
func (s *Sandbox) Process() *ProcessService {
	return s.process
}

// FS reads and writes files and directories in the sandbox.
func (s *Sandbox) FS() *FileSystemService {
	return s.fs
}

// API returns the underlying generated execution API client for this
// sandbox, for operations this package does not wrap. The generated API
// surface is not covered by Go compatibility guarantees and may change between
// versions. The caller must close the body of a response returned by a Raw
// method.
func (s *Sandbox) API() *sandboxapi.Client {
	return s.api
}
