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
