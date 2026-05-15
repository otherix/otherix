# agentmock

`internal/agentmock` is the in-process server-side fixture for the
Otherix Agent API contract (`api/openapi/agent.yaml`). It implements
the codegen-derived `agentapi.ServerInterface` in two TLS modes,
drives an opt-in heartbeat goroutine to a Control Plane endpoint,
and exposes a small Test API for state preload, mutation,
inspection, and fault injection.

Six endpoints are functional (`health.check`, `info.get`,
`storagePools.list`, `storagePools.get`, `storageImages.list`,
`tasks.get` as always-404); the remaining 38 of the 44
`ServerInterface` methods are mounted as 501 stubs so the contract
is compile-time-covered without runtime reach.

## What this is not

Not a binary, not a substitute for the real agent, not for production
use. The fixture only ships under `_test.go` consumers' wing — there
is no `cmd/mockagent/`. A stand-alone process re-using this package
verbatim is tracked as future work.

## Usage

```go
import (
    "net/http"
    "testing"

    "github.com/otherix/otherix/internal/agentmock"
)

func TestSomething(t *testing.T) {
    mock := agentmock.Start(t, agentmock.Options{})
    // mock.Stop is registered via t.Cleanup automatically.

    resp, _ := http.Get(mock.URL() + "/v1/health")
    defer resp.Body.Close()
    // resp.StatusCode == 200, body { "status": "ok" }.
}
```

`Options` exposes `NodeID`, `NodeName`, `Architecture`, `TLS`
(`TLSDisabled` default; `TLSEnabled` for client-cert handshake
testing), and the heartbeat-push wiring (`HeartbeatInterval` +
`ControlPlaneURL`). The combinations:

- Both unset — no heartbeat machinery, mock is pull-side only.
- `ControlPlaneURL` set, `HeartbeatInterval` unset — no goroutine,
  but `SendHeartbeatNow` is available for tests that want to drive
  the push synchronously (the heartbeat HTTP client is built so
  the call has somewhere to send).
- Both set — goroutine pushes on each tick AND `SendHeartbeatNow`
  works.
- `HeartbeatInterval` set, `ControlPlaneURL` unset — hard error
  (`*testing.T.Fatalf`): the goroutine has nowhere to push.

## Test API

The Test API exposes state mutation (`AddFirmware`, `AddStoragePool`,
`SetPoolCapacity`, `AddImage`, `EvictImage`, `SetMigrationCapability`,
`SetCapability`), inspection (`URL`, `NodeID`, `ReceivedRequests`),
heartbeat control (`SendHeartbeatNow`, `SuppressHeartbeats`,
`ResumeHeartbeats`), and fault injection (`InjectError`,
`InjectErrorPersistent`, `OnRequest`, `ClearInjections`).

## mTLS

`TLSEnabled` mode loads pre-baked ECDSA P-256 certs from
`testdata/certs/` and serves with client-cert verification. The
test-side client config is exposed via the same package for symmetric
use in tests that drive the mock through TLS.

For tests that stand up a CP-side TLS listener in front of the mock
(e.g. the heartbeat receiver integration suite), the package exports:

- `agentmock.CACertPEM()` — the cluster CA certificate in PEM form,
  for trust-pool construction.
- `agentmock.ControlPlaneServerTLSConfig()` — a `*tls.Config` that
  presents `controlplane.crt` + key and requires + verifies client
  certs against the same CA. Used to wrap an `httptest.NewUnstartedServer`
  with `StartTLS` for symmetric mTLS testing.
- `agentmock.NodeCertFingerprint()` — the SHA-256 fingerprint (32
  raw bytes) of the cert mock-agent presents to the CP. Insert this
  into `agent_certs.fingerprint_sha256` to make the agentMTLS
  middleware accept the mock's identity.
- `agentmock.NodeCertPEM()` — the same cert in PEM form, for tests
  that need to build a `*x509.Certificate` directly (e.g. seeding a
  `tls.ConnectionState` with peer certs).

## Heartbeat

When both `HeartbeatInterval` and `ControlPlaneURL` are set, a
goroutine pushes `POST /v1/nodes/{id}/heartbeat` to the configured
CP on each tick; the optional `migration` field is included only
when it differs from the previous heartbeat. `SuppressHeartbeats`
/ `ResumeHeartbeats` pause and unpause the loop without restart.

`SendHeartbeatNow` is independent of the goroutine schedule and
works whenever `ControlPlaneURL` is set — including the
URL-without-interval configuration above, which is the right shape
for tests that need to drive the push at deterministic moments
(e.g. assert state after a single heartbeat without racing a
ticker).

## Regenerating the codegen interface

`internal/agentapi/agent.gen.go` is `oapi-codegen` v2 output, derived
from `api/openapi/agent.yaml` after a 3.1→3.0 nullable normalisation
pass via `tools/openapi-normalize/`. Both steps run inside `make
agent-api-generate`; CI gates drift with `make agent-api-verify`.
The mock implements every method of the regenerated
`agentapi.ServerInterface`, so a contract drift surfaces as a build
failure here.
