<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/brand/banner-dark-strip-transparent.svg">
  <img alt="otherix" src="assets/brand/banner-light-strip-transparent.svg">
</picture>

# Otherix

Open-source self-hosted control plane for managing virtual machines on
KVM/QEMU clusters.

Otherix runs VMs on a fleet of bare-metal nodes, each controlled by an
Otherix agent that talks to QEMU directly (no libvirt). The control
plane (`otherix-api`) — REST API with in-process scheduler and
reconciliation loops — keeps the cluster's desired state in PostgreSQL;
agents report observed state through heartbeat. The split mirrors the
Kubernetes pattern: declarative API, generation/observed-generation
bookkeeping, reconciliation loops.

The differences come from the workload. VMs are long-lived stateful
entities, not cattle. Disks persist. Identities are stable.
Live migration is a first-class operation that runs peer-to-peer
between agents, with the control plane out of the data path. Storage
pools, networks, and firmwares are explicit cluster resources rather
than abstractions over a cloud provider. Snapshots and templates are
managed primitives, not afterthoughts.

Otherix is built to be deployed in your own datacentre or homelab.
Two control-plane deployment shapes are supported: a Helm chart for
Kubernetes (the production target), and standalone binaries that run
directly on a host. The latter works on a dedicated control-plane
host, or on the same host that runs an agent for single-node
installations. Agents install on each KVM/QEMU host as a single
binary alongside qemu-system-*. No external dependencies beyond
PostgreSQL.

## Status

Early development.

## Development

Otherix is developed with a clear separation of responsibilities
between human authorship and AI assistance:

- **Human-authored:** architecture, technical decisions, API and
  schema design, code review, roadmap, and overall project
  direction.
- **AI-assisted:** implementation of code and tests within the
  boundaries set by those decisions, drafting of routine
  documentation, and refactoring under review.

Every commit is reviewed by a human before merging. AI assistants
operate against the conventions documented in
[CLAUDE.md](CLAUDE.md), which is itself a human-authored reference.

## Architecture

Otherix ships two daemons and an operator CLI.

**Daemons (cluster components):**

- **`otherix-api`** — REST API for users, the CLI, the web UI, and agents.
  Hosts in-process VM placement scheduling, reconciliation loops, and the
  [river](https://riverqueue.com) worker pool for background jobs. Designed
  for HA — multiple replicas share work via Postgres advisory locks.
- **`otherix-agent`** — runs on each KVM/QEMU host; manages local
  virtualization, communicates with the control plane via mTLS.

**Operator CLI:**

- **`otherix`** — command-line client for operators, scripts, and dev
  workflows. Not a cluster component; installed wherever an operator
  runs commands.

The control plane is designed to run in Kubernetes via Helm. Agents run on
bare-metal hosts. Authoritative architecture records live in
[`docs/`](docs/).

## Quick start (local development)

```bash
# 1. Start dev dependencies (Postgres)
make dev-up

# 2. Apply migrations
make migrate-up

# 3. Run components locally (one terminal each)
make run-api
make run-agent       # limited usefulness without QEMU on the host

# 4. Verify the api-server is up
curl http://localhost:8080/healthz
# {"status":"ok","version":"dev"}
curl http://localhost:8080/readyz
# {"status":"ok","version":"dev","checks":{"database":{"status":"ok"}}}

# 5. Bootstrap an admin user (no admin-creation endpoint yet)
HASH=$(./bin/otherix-api --hash-password 'pick-a-strong-password')
psql "$DATABASE_URL" -c \
  "INSERT INTO users (email, password_hash, role)
   VALUES ('admin@local', '$HASH', 'admin');"

# 6. Login and exercise an authenticated endpoint
TOKEN=$(curl -s -X POST http://localhost:8080/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@local","password":"pick-a-strong-password"}' \
  | jq -r .access_token)
curl http://localhost:8080/v1/users/me -H "Authorization: Bearer $TOKEN"

# 7. Browse API docs in the browser
make api-preview
# Swagger UI: http://localhost:8081
# Redoc:      http://localhost:8082
```

For local development on macOS, install [Lima](https://lima-vm.io)
and run the agent inside a Lima VM; see
[docs/macos-development.md](docs/macos-development.md) for the
workflow and rationale.

## Linux dev environment

The agent runs natively on Linux as a per-user systemd unit. The
control plane runs from the same host (PostgreSQL via `make dev-up`).
Cert material reaches the agent through the join-token bootstrap
protocol, not manual cert generation — `make seed-mvp`
orchestrates the full flow end-to-end.

```bash
# 1. Start dev dependencies + apply migrations
make dev-up
make migrate-up

# 2. Stage the agent: build, install user systemd unit at
#    ~/.config/systemd/user/otherix-agent.service. The agent is NOT
#    started — the bootstrap protocol provisions cert material first.
make bootstrap-dev

# 3. (separate terminal) run the control plane against the dev config.
#    First-time only: seed the bootstrap admin via env vars before start.
export OTHERIX_BOOTSTRAP_ADMIN_EMAIL=admin@otherix.local
export OTHERIX_BOOTSTRAP_ADMIN_PASSWORD='correct-horse-battery-staple'
make run-api-dev

# 4. Bootstrap the agent end-to-end — mints join token, provisions
#    bootstrap material, starts the agent, waits for the node row to
#    appear, seeds a pool + default template.
make seed-mvp

# 5. Verify the node is reachable (heartbeat is the canonical proof).
./bin/otherix node list

# 6. Daily redeploy after agent code changes
make deploy-dev

# 7. Tail agent logs
journalctl --user -u otherix-agent -f

# 8. Tear down
make clean-dev
```

Cluster CA + per-replica CP server cert auto-generate inside the api
binary on first boot. The agent's cert material
is installed to `~/.config/otherix/certs/`. KVM is required for real
VM workloads — verify with `ls /dev/kvm` before relying on the dev
environment.

## macOS dev environment

See [docs/macos-development.md](docs/macos-development.md). The same
`make bootstrap-dev` / `deploy-dev` / `clean-dev` targets dispatch to
a Lima-based pipeline (Ubuntu 24.04 VM, system systemd unit, agent
reachable from the host via the 127.0.0.1:9443 port forward).

## Build

```bash
make build                  # daemons + CLI for the current platform
make build-api              # single daemon binary
make build-cli              # operator CLI binary
make build-linux-amd64      # cross-compile daemons for linux/amd64
make build-linux-arm64      # cross-compile daemons for linux/arm64
```

## Database

Migrations live at `internal/store/migrate/migrations/` and are embedded
into the `otherix-api` binary at build time. Apply them via the binary
(or via Make, which wraps the binary):

    make migrate-up        # Apply all pending migrations
    make migrate-status    # Show what is and isn't applied
    make migrate-down      # Roll back (DROPS public schema — see migration Down)

To regenerate sqlc-generated Go code after editing
`internal/store/queries/*.sql`:

    make sqlc-generate     # Pinned to sqlc 1.31.1 via Docker

Integration tests for the data access layer require Docker (testcontainers
brings up Postgres):

    make test-migrations

## Layout

```
cmd/{api,agent,cli}/                     # binary entry points
internal/                                # private packages
  api/ agent/                            # per-daemon packages
  scheduler/ reconciler/                 # in-process control-plane logic
  auth/ config/ logger/ version/         # shared base packages
api/openapi/                             # Control Plane + Agent API specs
internal/store/                          # data access layer (sqlc-generated + queries)
internal/store/migrate/migrations/       # goose SQL migrations (embedded into api binary)
deploy/                                  # Dockerfiles, compose, example configs
docs/                                    # architecture, plans
```

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for project conventions and
development practices. The canonical technical reference is
[`CLAUDE.md`](CLAUDE.md).

## License

Otherix is licensed under the Apache License, Version 2.0.
See [LICENSE](LICENSE) for the full text.

Copyright 2026 Andrei Taranik.
