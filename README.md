# Baseten Go SDK

Go SDK for Baseten.

⚠️ Under active development. Nothing should be considered stable at this time.

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