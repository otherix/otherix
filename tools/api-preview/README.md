# API Preview

Local environment for browsing and validating OpenAPI specs.

## Usage

From repository root:

- `make api-preview` — start Swagger UI and Redoc
- `make api-preview-stop` — stop containers
- `make api-preview-logs` — follow logs
- `make api-validate` — lint every spec under `api/openapi/`
- `make api-bundle` — bundle Control Plane spec with all `$ref`s resolved into `api/openapi/control-plane.bundled.yaml` (gitignored)

## URLs (after start)

- Swagger UI: http://localhost:8081
- Redoc:      http://localhost:8082

Swagger UI shows multiple specs via the dropdown in the top right.
Redoc shows only Control Plane — it doesn't support multiple specs per
instance. When a second spec needs a Redoc view, add another service.

## Tooling

- UI containers: `swaggerapi/swagger-ui` and `redocly/redoc`, pinned in the Makefile.
- Linter / bundler: `@redocly/cli` invoked via `npx` (pinned via `REDOCLY_VERSION`
  in the Makefile). Configuration lives in `redocly.yaml` at the repo root.

## Adding a new spec

1. Place the YAML in `api/openapi/`.
2. Add an entry to the `URLS` env var in `tools/api-preview/docker-compose.yaml`.
3. (Optional) add a Redoc service for it if you want a Redoc view too.

`make api-validate` picks up new files automatically — no Makefile changes needed.
