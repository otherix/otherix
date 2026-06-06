# Quickstart

The shortest path from zero to a running VM on a single Linux host. By
the end you will have a control plane, one enrolled node, and a booted
Ubuntu VM you can attach a console to.

This guide assumes a Linux host with KVM and QEMU. If you are on a Mac,
the agent runs inside a Lima VM - see
[macOS development](../macos-development.md) for that flow, then return
here for the CLI steps. For deeper config detail see
[Installation](install.md).

!!! note "Prerequisites"
    - `otherix-api`, `otherix-agent`, and `otherix` built or installed
      (`make build` drops them in `./bin/`).
    - `/dev/kvm` present and `qemu-system-<arch>` installed for the
      agent.
    - Network reachability from the agent host to the control plane on
      the user API port (8080) and the agent/bootstrap TLS port (8443).

## 1. Start the control plane

Seed the bootstrap admin, then start the api-server. The admin env vars
are read on first boot only.

```bash
export OTHERIX_BOOTSTRAP_ADMIN_EMAIL=admin@otherix.local
export OTHERIX_BOOTSTRAP_ADMIN_PASSWORD='correct-horse-battery-staple'

otherix-api --config /etc/otherix/api.yaml
```

The minimal `api.yaml` is in [Installation](install.md#minimal-apiyaml):
it enables `agent_server`, `agent_client`, and `workers` so the cluster
can dispatch to the node. On first boot the api-server generates the
cluster CA and its own server cert, seeds the admin, and seeds the
`default` storage pool name.

Confirm it is up:

```bash
curl http://localhost:8080/healthz
# {"status":"ok","version":"..."}
```

!!! tip "Local dev shortcut"
    Working from the repo? `make local-dev-start` brings up the whole
    stack (api-server with embedded etcd, agent, and a configured CLI)
    in one command, then `make local-dev-stop` tears it all down. The
    steps below are the manual equivalent.

## 2. Point the CLI at the cluster

`config add cluster` logs in with the admin credentials, mints a
long-lived API token, and stores it in `~/.otherix/config`. Subsequent
commands need neither `--token` nor `--endpoint`.

```bash
otherix config add cluster \
  --name local \
  --server http://localhost:8080 \
  --login admin@otherix.local \
  --password 'correct-horse-battery-staple'
# cluster added: name=local server=http://localhost:8080 current=true
```

## 3. Enrol the local node

Enrolment is a join-token bootstrap. Mint a token on the control plane,
then run `otherix-agent bootstrap` on the node host. The bootstrap call
fetches the CA (pinned by fingerprint), generates a keypair, submits a
CSR, and writes the issued cert material plus an `agent.yaml`.

Mint the token (admin only):

```bash
otherix node join-token create --node-name node-1 --ttl 10m
```

The output prints the token plaintext and the cluster CA fingerprint
exactly once - save both:

```
Token created (id: ...):

  otx_join_...

CA fingerprint:

  sha256:...
```

Run bootstrap on the node host, passing both values. `--cp-url` is the
control plane's TLS agent/bootstrap listener (port 8443, not the plain
user API). `--advertised-endpoint` is the HTTPS URL the control plane
will use to reach this agent (the agent listens on `0.0.0.0:9443` by
default):

```bash
otherix-agent bootstrap \
  --token 'otx_join_...' \
  --ca-fingerprint 'sha256:...' \
  --cp-url https://localhost:8443 \
  --node-name node-1 \
  --advertised-endpoint https://127.0.0.1:9443 \
  --migration-host 127.0.0.1
```

Architecture is auto-detected from the host. Bootstrap is idempotent: a
re-run without `--force` on an already-provisioned host exits cleanly.
It writes the `agent.yaml` only when absent, so operator-tuned settings
survive a re-bootstrap.

!!! note "Token source alternatives"
    Instead of `--token`, use `--token-path <file>` or
    `--token-env <VARNAME>` (exactly one of the three).

Now run the agent. The bare binary defaults to `serve`:

```bash
otherix-agent serve --config /etc/otherix/agent.yaml
```

`serve` starts in a polling loop and transitions to full runtime once
the cert material and config are present (which bootstrap just wrote). In
production you would run this under systemd; the repo dev flow installs a
user unit and `make seed-dev` drives bootstrap end to end.

Verify the node reaches `ready`. It arrives `pending` and flips to
`ready` after its first heartbeat:

```bash
otherix node list
# NAME    ARCHITECTURE  STATUS  CORDONED  AGE
# node-1  arm64         ready   no        20s
```

## 4. Create a VM

A VM is created directly from an image URL - there is no template
entity. `--image-url` and `--arch` are required; `--pool` is optional and
resolves to the cluster default (`default`, auto-provisioned on every
ready node). `--wait` blocks until the async task reaches a terminal
state.

```bash
otherix vm create demo-vm \
  --image-url https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-arm64.img \
  --arch arm64 \
  --vcpus 2 \
  --memory-mb 2048 \
  --wait
# created task=<task-id> status=pending
# .....
# vm running task=<task-id>
```

!!! tip "Match the image to the host architecture"
    Use the `amd64` cloud image
    (`...-minimal-cloudimg-amd64.img`) with `--arch amd64` on an x86_64
    host. The agent downloads the image into a per-pool cache on first
    use, so the first create pays a one-time download.

## 5. Inspect, console, delete

List and inspect:

```bash
otherix vm list
# NAME     STATUS   POOL     IMAGE
# demo-vm  running  default  ubuntu-24.04-minimal-cloudimg-arm64.img

otherix vm get demo-vm
# name: demo-vm
# status: running
# node: node-1
# vcpus: 2
# memory_mb: 2048
# ...
```

Attach to the serial console. The terminal goes raw; press `Ctrl+]` to
detach. The Ubuntu cloud image boots to a login prompt - press Enter:

```bash
otherix vm console demo-vm
# connected to demo-vm (serial console). Press Ctrl+] to detach.
```

Tear it down (interactive confirmation without `--force`):

```bash
otherix vm delete demo-vm --wait --force
# deleted task=<task-id> status=pending
# .....
# vm deleted task=<task-id>
```

## Next steps

- [Join a node](../guides/join-a-node.md) - the enrolment protocol in
  full, including fleet tokens and recovery.
- [Create and manage VMs](../guides/create-and-manage-vms.md) -
  placement, lifecycle, resize, and snapshots.
- [Declarative manifests](../guides/declarative-manifests.md) -
  `otherix create -f` for YAML-defined resources.
- [Storage pools](../guides/storage-pools.md) and
  [Networks](../guides/networks.md) - cluster infrastructure resources.
- [Console and logs](../guides/console-and-logs.md) - console access
  modes and log streaming.
- [Architecture](../architecture.md) - the desired/observed state model
  and how reconciliation works.
