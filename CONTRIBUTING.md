# Contributing to Otherix

Thanks for your interest in contributing.

## How this project is developed

Architecture, technical decisions, code review, and roadmap are
human-authored. AI coding assistants are used to draft
implementation and tests within the boundaries set by those
decisions, and every change is reviewed by a human before merging.

## Project conventions

Architecture and design records live in [docs/](docs/):

- [docs/architecture.md](docs/architecture.md) — high-level
  orientation, data flow, schema design.
- [docs/rbac.md](docs/rbac.md) — RBAC role / permission / scope
  matrix.
- [docs/scheduler-configuration.md](docs/scheduler-configuration.md)
  — placement scoring and configuration.
- [docs/macos-development.md](docs/macos-development.md) — macOS
  dev workflow (Lima).

Code style follows the
[Google Go Style Guide](https://google.github.io/styleguide/go).
Tooling is wired through Make targets:

- `make fmt` — `gofumpt` + `goimports -local github.com/otherix/otherix`
- `make vet` — `go vet ./...`
- `make lint` — `golangci-lint run --timeout 5m`
- `make vuln` — `govulncheck`

Tests use the standard library `testing` package and
[google/go-cmp](https://github.com/google/go-cmp) for structural
diffs. **No assertion libraries** (no testify). Mock outbound HTTP
with `net/http/httptest`. Test doubles end in `Stub` / `Fake` /
`Spy` / `Mock`.

All hand-written `.go` files carry the SPDX short-form Apache 2.0
header on the first two lines:

```
// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik
```

Generated files (oapi-codegen output in
`internal/agentapi/agent.gen.go`) keep their own `// Code generated …`
preamble, do not receive the SPDX header, and must not be edited by
hand. Regenerate via `make agent-api-generate`.

## Development setup

See the **Quick start (local development)** section of
[README.md](README.md) for prerequisites, dependency setup, and a
basic local-run flow.

Key make targets:

- `make build` — build daemons + CLI
- `make test` / `make test-etcd` — unit / etcd-backed integration tests (no Docker)
- `make lint` / `make fmt` — code quality
- `make api-validate` / `make api-preview` — OpenAPI checks
- `make help` — full grouped target list

## Pull requests

- One logical change per PR. The project commits in atomic units;
  PRs are expected to follow the same shape.
- Conventional Commits style for messages
  (`feat(area): summary`, `fix(area): summary`, `refactor: ...`,
  `docs: ...`, `test: ...`, `chore: ...`).
- All tests pass (`make test && make test-etcd`).
- Lint clean (`make lint`).
- OpenAPI specs valid (`make api-validate`) when API surface is
  touched.
- New `.go` files include the SPDX Apache 2.0 header (see the
  "Project conventions" section above).

## Issues and discussions

Open an issue first to discuss scope and approach before
implementing non-trivial work.

## Code of Conduct

Be respectful and constructive. Disagreements about technical
direction are welcome; personal attacks are not.

## License

By contributing to Otherix, you agree that your contributions will be
licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
