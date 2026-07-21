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

Current SDK only has barebones client. Here is usage example of the barebones underlying client:

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