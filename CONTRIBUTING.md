# Contributing

## Regenerating API Clients

Generated code lives in `client/managementapi/`, `client/inferenceapi/`, and `client/sandboxapi/`. To regenerate from the checked-in OpenAPI specs:

```sh
cd internal/tools/apigen && go run .
```

To import the sandbox management contract from staging while preserving unrelated APIs:

```sh
cd internal/tools/apigen && go run . -management-spec-url https://api.staging.baseten.co/v1/spec -sandbox-management-only
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


The explicit management URL prevents accidentally replacing unreleased sandbox
routes with a production snapshot. `-sandbox-management-only` imports sandbox
paths, token exchange and their referenced components into the existing snapshot.
Omit it for an intentional full management refresh; add `-update-specs` to also
refresh inference and the model configuration schema. Staging domain occurrences are normalized to
`api.baseten.co` before saving or generating. The execution snapshot remains
pinned; see [snapshot provenance](internal/tools/apigen/specs/README.md).

## Generator and sandbox tests

Run all three Go modules; root tests do not include the generator's nested module:

```sh
go test ./...
(cd internal/tools/apigen && go test ./...)
(cd internal/separate-module-tests && go test ./...)
go test -race ./client
```

Sandbox protocol tests use local HTTP servers for multipart and binary requests,
raw streaming responses, cancellation, error handling, and management headers.
They do not verify live sandbox behavior. High-level lifecycle helpers and their
live sandbox test are deferred to a separate change.
