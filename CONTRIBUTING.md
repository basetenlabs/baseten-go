# Contributing

## Regenerating API Clients

Generated code lives in `client/managementapi/`, `client/inferenceapi/`, and `client/sandboxapi/`. To regenerate from the checked-in OpenAPI specs:

```sh
cd internal/tools/apigen && go run .
```

To download the latest specs from Baseten before regenerating:

```sh
cd internal/tools/apigen && go run . -update-specs
```

A spec sync PR commits everything `-update-specs` produces, so the generated clients match what Baseten serves. Don't pick parts of a spec or hand-edit generated code; if an endpoint isn't in the published spec yet, wait for it to ship. State the command you ran in the PR description.

## End-to-End Tests

E2e tests in `client/e2e_test.go` run against a live Baseten environment. They are skipped automatically when `BASETEN_E2E_TEST_API_KEY` is not set.

To bootstrap the test model, see [baseten-python](https://github.com/basetenlabs/baseten-python)'s contributing guide.

### Running

```bash
BASETEN_E2E_TEST_API_KEY=... \
BASETEN_E2E_TEST_DOMAIN=... \
BASETEN_E2E_TEST_MODEL_ID=... \
    go test ./...
```
