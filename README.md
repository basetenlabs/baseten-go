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

The SDK is a thin client over the Baseten management and inference APIs, plus a few operations that take more than one
call, such as pushing a model. It has no dependencies of its own.

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