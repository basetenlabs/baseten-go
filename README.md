# Baseten Go SDK

[![Release](https://img.shields.io/github/v/release/basetenlabs/baseten-go)](https://github.com/basetenlabs/baseten-go/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/basetenlabs/baseten-go.svg)](https://pkg.go.dev/github.com/basetenlabs/baseten-go)

Go SDK for Baseten. See the [API documentation](https://pkg.go.dev/github.com/basetenlabs/baseten-go) and [usage](#usage) below.

⚠️ SDK may change in incompatible ways between releases until the SDK reaches 1.0.

## Install

```bash
go get github.com/basetenlabs/baseten-go
```

## Usage

The SDK provides clients for the Baseten management, inference, and sandbox execution APIs, plus
helpers for model uploads. It has no runtime dependencies.

### Calling the API

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/managementapi"
)

func main() {
	// Create a management client
	cl, err := client.NewManagementClient(client.ManagementClientOptions{
		APIKey: "my-api-key",
	})
	if err != nil {
		log.Fatal(err)
	}

	// List all models
	resp, err := cl.API().GetModels(context.Background(), managementapi.GetV1ModelsParams{})
	if err != nil {
		log.Fatal(err)
	}

	// Print each model name
	for _, m := range resp.Models {
		fmt.Println(m.Name)
	}
}
```

### Pushing a model

`PushModel` validates the push, builds a tar archive of the model directory, uploads it, and creates the model or adds a
deployment to an existing one. Since the SDK has no dependencies, the caller supplies the upload itself:

```go
// The parsed config.yaml is what the server builds from; the raw text is kept
// verbatim on the deployment for display.
raw, err := os.ReadFile("./my-model/config.yaml")
if err != nil {
	log.Fatal(err)
}
var config map[string]any
if err := yaml.Unmarshal(raw, &config); err != nil {
	log.Fatal(err)
}

result, err := cl.PushModel(context.Background(), client.PushModelOptions{
	Config:          config,
	RawConfig:       string(raw),
	Archive:         modelarchive.BuildModelArchiveOptions{Dir: "./my-model"},
	EnvironmentName: "production",
	ModelUploader: func(ctx context.Context, upload client.ModelUpload) error {
		s3Client := s3.NewFromConfig(aws.Config{
			Region: upload.Region,
			Credentials: credentials.NewStaticCredentialsProvider(
				upload.AccessKeyID, upload.SecretAccessKey, upload.SessionToken),
		})
		_, err := transfermanager.New(s3Client).UploadObject(ctx, &transfermanager.UploadObjectInput{
			Bucket: &upload.Bucket,
			Key:    &upload.Key,
			Body:   upload.Body,
		})
		return err
	},
})
if err != nil {
	log.Fatal(err)
}

// The deployment is not live yet; poll it to wait for a terminal status.
fmt.Println(result.Model.Id, result.Deployment.Id, result.Deployment.Status)
```
### Sandboxes

Sandbox management and execution are separate APIs. Use the generated management
methods through `cl.API()` to manage sandboxes and obtain a short-lived token.
Connect to the execution URL returned by the management API with that token:

```go
sandbox, err := client.NewSandboxClient(client.SandboxClientOptions{
    BaseURL: sandboxURL,
    Token:   token,
})
if err != nil {
    log.Fatal(err)
}
result, err := sandbox.API().PostProcess(ctx, sandboxapi.ProcessRequest{Command: "echo hello"})
if err != nil {
    log.Fatal(err)
}
fmt.Println(result.Stdout, result.ExitCode)
```

Import `github.com/basetenlabs/baseten-go/client/sandboxapi` for execution types.
The caller manages token expiry, sandbox readiness, and deletion.

The execution client performs no token exchange or retries. Binary uploads accept `io.Reader` with the content type selected from the API spec.
For multipart, supply the content type and boundary returned by
`multipart.Writer.FormDataContentType()`. Generated `...Raw` methods accept
`RawRequestOptions` for content negotiation and return an unread `*http.Response`;
the caller must close its body. Typed methods negotiate JSON. The archive export
response retains `StatusCode` and separate `JSON200`/`JSON202` payloads.
