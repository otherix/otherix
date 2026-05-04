# Contributing to Otherix

Thanks for your interest in contributing.

## How this project is developed

Architecture, technical decisions, code review, and roadmap are
human-authored. AI coding assistants are used to draft implementation
and tests within the boundaries set by those decisions, and every
change is reviewed by a human before merging. See
[CLAUDE.md](CLAUDE.md) for the conventions both humans and AI follow.

## Project conventions

The canonical reference for project conventions, architecture, code
style, API design rules, and development practices is
[CLAUDE.md](CLAUDE.md). It originally targeted Claude Code agent
sessions but is the source of truth for any contributor — human or
AI. Read it at the start of any non-trivial work.

Companion documents:

- [docs/rbac.md](docs/rbac.md) — RBAC role/permission/scope matrix.

## Development setup

See the **Quick start (local development)** section of
[README.md](README.md) for prerequisites, dependency setup, and a
basic local-run flow.

Key make targets:

- `make build` — build daemons + CLI
- `make test` / `make test-migrations` — unit / integration tests
- `make lint` / `make fmt` — code quality
- `make api-validate` / `make api-preview` — OpenAPI checks
- `make help` — full grouped target list

## Pull requests

- One logical change per PR. The project commits in atomic units;
  PRs are expected to follow the same shape.
- Conventional Commits style for messages
  (`feat(area): summary`, `fix(area): summary`, `refactor: ...`,
  `docs: ...`, `test: ...`, `chore: ...`).
- All tests pass (`make test && make test-migrations`).
- Lint clean (`make lint`).
- OpenAPI specs valid (`make api-validate`) when API surface is
  touched.
- New `.go` files include the SPDX header (see [CLAUDE.md](CLAUDE.md)).
- New conventions or scope changes go into `CLAUDE.md` as part of the
  same change.

## Issues and discussions

Open an issue first to discuss scope and approach before
implementing non-trivial work.

## Code of Conduct

Be respectful and constructive. Disagreements about technical
direction are welcome; personal attacks are not.

## License

By contributing to Otherix, you agree that your contributions will be
licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
