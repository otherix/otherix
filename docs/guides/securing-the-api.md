# Securing the API

The user-facing control-plane `/v1` API serves HTTPS by default and
authenticates callers with Bearer tokens - JWTs and `otx_*` API tokens.
This is the surface your operators, the `otherix` CLI, and any web UI
talk to. This page covers how that TLS is provisioned and how the CLI
establishes trust when it connects.

The separate per-node agent API is a different surface: it uses mutual
TLS (client certificates) to authenticate cluster nodes, not human
users. Users and the CLI **never** present a client certificate to the
`/v1` API - they authenticate with a token over a server-authenticated
TLS connection. For the node-side certificate flow, see
[Join a node](join-a-node.md) and [Certificates](../operations/certificates.md).

## Transport model

| Surface | Transport | Authenticates with |
|---|---|---|
| User-facing `/v1` API | HTTPS (server cert only) | Bearer token (JWT or `otx_*` API token) |
| Per-node agent API | Mutual TLS | Node client certificate |

TLS on the `/v1` listener is controlled by a single flag,
`server.tls.enabled`, which defaults to `true`:

```yaml
server:
  listen: "0.0.0.0:8080"
  tls:
    enabled: true   # default; serve HTTPS on the /v1 listener
```

Set it to `false` only to serve plaintext HTTP - appropriate on a
loopback bind for local development, or when a terminating proxy in
front of the control plane handles TLS. See
[Local development without TLS](#local-development-without-tls) below.

## Certificate sources

The `/v1` listener does not have its own certificate block. It reuses
the per-replica control-plane certificate (`cp_cert`), the same leaf
each replica presents to agents. There are two ways that cert is
sourced.

### Operator-supplied certificate

Point `cp_cert.cert_file` and `cp_cert.key_file` at an externally-issued
certificate and its key. Both must be set; they are a pair.

```yaml
cp_cert:
  cert_file: /etc/otherix/cp.crt
  key_file: /etc/otherix/cp.key
```

Use this when you want a publicly-trusted certificate - one from your
own CA or a public CA - so that clients (and browsers) trust the control
plane without any extra step.

### Auto-generated certificate (default)

When no operator certificate is configured, each replica generates a
leaf certificate signed by the cluster CA on boot. Its Subject
Alternative Name (SAN) set covers `localhost`, `127.0.0.1`, the host's
hostname, and the listen address.

To reach the control plane by an external hostname (a DNS name or a
load-balancer VIP that auto-detection does not see), add that name to
`cp_cert.additional_sans` so the generated certificate is valid for it:

```yaml
cp_cert:
  additional_sans:
    - cp.example.com
    - 203.0.113.10
```

!!! note "A generated cert is not publicly trusted"
    The auto-generated certificate chains to the cluster CA, which no
    system trust store knows about by default. Clients connecting over
    HTTPS need to trust that CA - the CLI does this for you (see below),
    and you can distribute the CA bundle to other clients out of band.
    For a turnkey, browser-trusted endpoint, supply your own certificate
    instead.

## Connecting the CLI

`otherix config add cluster` logs in to a control plane and persists a
named cluster. Against an HTTPS endpoint it establishes trust
automatically, in this order:

1. **System trust.** If the control-plane certificate is already trusted
   by the system trust store (for example a publicly-trusted operator
   certificate), nothing extra is needed.
2. **Fetch and pin the cluster CA.** Otherwise the CLI fetches the
   cluster CA from the control plane and pins it. Pass
   `--ca-fingerprint <sha256-hex>` (obtained out of band) to verify the
   fetched CA non-interactively, or confirm the displayed fingerprint at
   the interactive prompt.

```bash
otherix config add cluster \
  --name prod \
  --server https://cp.example.com:8080 \
  --login admin@example.com \
  --ca-fingerprint sha256:1a2b3c...
```

You can also provide the CA directly instead of fetching it:

| Flag | Effect |
|---|---|
| `--ca-fingerprint <hex>` | Verify the fetched cluster CA against an expected sha256 fingerprint, non-interactively. |
| `--ca-file <path>` | Trust the CA bundle in a local PEM file. |
| `--ca-data <base64>` | Trust a base64-encoded PEM bundle inline (alternative to `--ca-file`). |
| `--insecure-skip-tls-verify` | Disable certificate verification entirely. Development only. |

Once trust is established, the CA is stored **inline** in the CLI config
as `certificate-authority-data` (base64 PEM). The config entry is
therefore self-contained: it carries everything needed to verify the
endpoint, with no reference to an external CA file.

!!! warning "Never use `--insecure-skip-tls-verify` against a real control plane"
    `--insecure-skip-tls-verify` turns off certificate verification, so
    the connection cannot detect an impostor or a man-in-the-middle. It
    exists for local experimentation only. Against any control plane that
    matters, pin the CA with `--ca-fingerprint`, supply it with
    `--ca-file` / `--ca-data`, or use a publicly-trusted certificate.

## Local development without TLS

For local development you can drop TLS entirely. Set
`server.tls.enabled: false` and bind the listener to loopback:

```yaml
server:
  listen: "127.0.0.1:8080"
  tls:
    enabled: false
```

The CLI then talks plain HTTP, and no CA is needed:

```bash
otherix config add cluster \
  --name dev \
  --server http://localhost:8080 \
  --login admin@example.com
```

!!! warning "Plaintext is for loopback or a terminating proxy only"
    A plaintext `/v1` listener carries Bearer tokens in the clear. Only
    disable TLS on a loopback bind, or where a proxy in front of the
    control plane terminates TLS. Never expose a plaintext listener on a
    routable address.

## See also

- [Certificates](../operations/certificates.md)
- [Users and RBAC](users-and-rbac.md)
- [Join a node](join-a-node.md)
- [High availability](../operations/high-availability.md)
