package client_test

import (
	"context"
	"fmt"
	"log"

	"github.com/basetenlabs/baseten-go/client"
)

// A workload exchanges its identity provider's token (here a projected
// Kubernetes service account token) for short-lived Baseten access tokens
// under a trust rule, instead of using an API key.
func ExampleNewWorkloadIdentityCredentials() {
	creds, err := client.NewWorkloadIdentityCredentials(client.WorkloadIdentityCredentialsOptions{
		IdentityTokenFile: "/var/run/secrets/tokens/baseten",
		TrustRuleID:       "my-trust-rule-id",
	})
	if err != nil {
		log.Fatal(err)
	}
	cl, err := client.NewManagementClient(client.ManagementClientOptions{Credentials: creds})
	if err != nil {
		log.Fatal(err)
	}
	org, err := cl.API().GetOrganizationsMe(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(org.OrgId)
}
