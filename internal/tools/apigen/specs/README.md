# API snapshots

Normal generation reads only checked-in snapshots.

`management.json` is a curated sandbox import onto the Go baseline at https://github.com/basetenlabs/baseten-go/commit/455b16fedfecfaa89f4165f9118794c9477ab9df. On 2026-09-29, `/v1/sandboxes/*`, `/v1/token`, and their transitive component dependencies were imported from https://api.staging.baseten.co/v1/spec. Unrelated routes and schemas remain at the baseline, avoiding unrelated routing API regressions in the staging snapshot. All literal `api.staging.baseten.co` occurrences are replaced with `api.baseten.co` before import or generation. The initial import found no differing shared component dependencies.

`sandbox.yml` is the exact execution snapshot from https://github.com/basetenlabs/baseten-js/blob/c06b325ef64bf86d9f90019dc8fd597b5b950ef5/packages/client/scripts/apigen/specs/sandbox.yml. Upstream origin: https://github.com/blaxel-ai/sandbox/blob/main/sandbox-api/docs/openapi.yml. The execution snapshot remains pinned until deliberately updated.

Refresh sandbox management support while retaining the baseline:

```sh
go run . -sandbox-management-only -management-spec-url https://api.staging.baseten.co/v1/spec
```

The scoped importer rejects conflicting shared components, external references, and missing dependencies. Review any conflict explicitly before changing the baseline.

To deliberately replace the entire management snapshot, omit `-sandbox-management-only`. Add `-update-specs` to also refresh inference and the model configuration schema; this flag requires an explicit management URL. Full refreshes may introduce unrelated API changes and must be reviewed accordingly.
