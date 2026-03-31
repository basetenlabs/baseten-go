# Contributing

## Regenerating API Clients

Generated code lives in `client/managementapi/` and `client/inferenceapi/`. To regenerate from the checked-in OpenAPI specs:

```sh
cd internal/tools/apigen && go run .
```

To download the latest specs from Baseten before regenerating:

```sh
cd internal/tools/apigen && go run . -update-specs
```

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
