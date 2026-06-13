# Quick start: single-node control plane

Bring up a working Otherix control plane on one Debian/Ubuntu host, with
no repo clone and no Go toolchain.

## 1. Install the control plane

```bash
curl -fsSL https://raw.githubusercontent.com/otherix/otherix/main/deploy/install/install.sh \
  | sudo OTHERIX_COMPONENT=api sh
```

The installer downloads the `otherix-api` `.deb`, installs the systemd
unit, generates an auth secret, and prints a one-time admin password.
The server boots in `single` mode and advertises this host's routable
IPv4 (`peer_url: auto`), so it is already HA-ready.

Check it:

```bash
systemctl status otherix-api
curl http://localhost:8080/healthz   # or the configured listener
```

## 2. Install the CLI

macOS:

```bash
brew install otherix/tap/otherix
```

Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/otherix/otherix/main/deploy/install/install.sh \
  | sudo OTHERIX_COMPONENT=cli sh
```

Then authenticate with the printed admin credentials. The CLI logs in,
mints a long-lived API token, and stores it as the current cluster:

```bash
otherix config add cluster \
  --name local \
  --server https://<cp-host>:8080 \
  --login <admin-email> \
  --password <admin-password>
```

## 3. Add a hypervisor node

On each KVM host:

```bash
curl -fsSL https://raw.githubusercontent.com/otherix/otherix/main/deploy/install/install.sh \
  | sudo OTHERIX_COMPONENT=agent sh
sudo otherix-agent bootstrap \
  --token <join-token> \
  --cp-url https://<cp-host>:8443 \
  --ca-fingerprint <sha256> \
  --node-name <name> \
  --advertised-endpoint <agent-host>:9443 \
  --migration-host <agent-host>
```

Issue the join token from the CLI (`otherix node join-token create`),
which prints the token plaintext and the CA fingerprint exactly once.
The agent boots in polling mode and becomes ready once bootstrap writes
its cert material.

## Growing to HA

A single node is a one-member cluster with a routable peer URL, so you
can add members later with no reconfiguration of this node. See the HA
guide (Phase 2).

## Filesystem layout

- `/etc/otherix/` - operator config (`api.yaml`, `api.env`).
- `/var/lib/otherix/` - all runtime state (etcd data, certs, pools, VM
  state). Package removal never deletes this tree; `apt purge` keeps it.
