# Load balancers

A load balancer fronts a **pool of VMs** selected by label and brokers each
connection to one healthy backend. It gives you a single stable name for a set
of interchangeable VMs - a stateless web tier, a group of read replicas - so a
client connects to the load balancer and never has to know which VM it lands on.

Balancing is **per-connection**: the broker picks a healthy backend at the moment
a connection is established (broker-time), so a long-lived listener spreads its
connections across the pool. Load balancers are managed through the `otherix lb`
command group (aliased `loadbalancer`) and are addressed by name.

For how a client reaches a VM through the control plane with no public IP per VM,
see the [Ingress concept](../concepts/ingress.md).

## Label your backends

VMs join a load balancer's pool by their **labels**. Set them when you create the
VM:

```bash
otherix vm create web01 \
  --image-url https://example.com/noble.img \
  --arch arm64 \
  --network net-app \
  --label app=web \
  --label tier=frontend
```

Repeat `--label k=v` for each label. Create every VM that should serve the pool
with the same selectable label (here `app=web`).

!!! note "Labels are set at create time"
    A VM's labels are fixed when the VM is created. A VM must be created with a
    label to be selectable by a load balancer - there is no way to add the label
    afterwards, so plan the labels before you launch the pool.

## Create a load balancer

The minimum a load balancer needs is a traffic port and a selector:

```bash
otherix lb create web-lb --port 80 --selector app=web
```

- **`--port`** (required, 1..65535) is the guest TCP port that traffic is sent
  to - the port your service listens on inside each VM.
- **`--selector`** (required, one or more `k=v` terms) is the label match. Every
  VM whose labels satisfy every term joins the pool. Comma-separate multiple
  terms: `--selector app=web,tier=frontend`.

### Adding health checks

Health-check flags turn on active probing so a broken backend is taken out of
rotation. All are optional:

```bash
otherix lb create web-lb \
  --port 80 \
  --selector app=web \
  --health-port 8080 \
  --health-interval 5 \
  --health-healthy-threshold 2 \
  --health-unhealthy-threshold 2
```

| Flag | Meaning | Default |
| --- | --- | --- |
| `--health-port` | TCP port to probe. Lets you health-check a `/health` port distinct from the traffic port. | follows `--port` |
| `--health-interval` | Seconds between probes (1..300). | `10` |
| `--health-timeout` | Per-probe timeout in seconds (1..60). | `2` |
| `--health-healthy-threshold` | Consecutive successful probes before an unhealthy backend rejoins (1..10). | `2` |
| `--health-unhealthy-threshold` | Consecutive failed probes before a backend is dropped (1..10). | `3` |

`--health-port` follows `--port` unless you set it, so a service that serves both
traffic and its health check on the same port needs no health-port flag.

## How health checks work

The owning agent actively **TCP-probes** each backend's health port on the
configured interval. The pool served to connections is filtered by the verdicts:

- **A confirmed-unhealthy backend is subtracted from the pool.** After a backend
  fails `--health-unhealthy-threshold` probes in a row it is removed from
  rotation and no new connections are brokered to it.
- **A recovering backend rejoins.** Once a dropped backend passes
  `--health-healthy-threshold` probes in a row it is put back into rotation.
- **A still-warming backend is served.** A backend that has not yet been probed
  (no verdict) is kept in the pool. Load balancing fails **toward serving**: a
  problem in the health pipeline never darks a load balancer that is otherwise
  serving traffic.
- **When every backend is confirmed unhealthy, a connect is refused.** With no
  eligible backend the broker returns an error rather than sending traffic to a
  known-bad VM.

The health port can differ from the traffic port, which is the point of
`--health-port` - probe a lightweight `/health` endpoint on port 8080 while
serving traffic on port 80.

## See pool status

`otherix lb list` shows each load balancer's rolled-up health and backend count:

```bash
otherix lb list
```

```
NAME     PORT   SELECTOR              STATUS     TARGETS
web-lb   80     app=web               healthy    3/3
api-lb   8443   app=api,tier=edge     degraded   2/3
db-ro    5432   role=replica          unhealthy  0/2
cache    6379   app=cache             no_backends 0/0
```

The `STATUS` column reports the rolled-up state of the pool:

- **`healthy`** - every backend is passing its health check.
- **`degraded`** - the pool is serving but not fully healthy: some backends are
  down, or some are still warming up. A partly-healthy or still-warming pool
  reads `degraded`, **not** `unhealthy`.
- **`unhealthy`** - there are backends, but every one of them is confirmed
  unhealthy. A connect is refused in this state.
- **`no_backends`** - the selector currently matches no VMs.

`TARGETS` is `<healthy>/<total>` - how many backends are in rotation out of how
many the selector matches.

For per-backend detail, use `get`:

```bash
otherix lb get web-lb
```

This lists each backend VM with its health (`true`, `false`, or `unknown` for a
warming backend that has no verdict yet) and the time it was last probed, so you
can see exactly which VM is dragging the pool down.

## Connect through it

`otherix lb connect` opens a **local listener** that balances each connection
over the healthy pool:

```bash
otherix lb connect web-lb -L 127.0.0.1:8080
```

Then point any client at the local address:

```bash
curl http://127.0.0.1:8080/
```

`-L, --listen host:port` sets the local listener address. Because each accepted
connection is brokered independently to a healthy backend, a long-lived listener
load-balances on its own - every new connection re-brokers and may land on a
different VM.

## Update and delete

`otherix lb update` changes a load balancer in place. `--selector` **replaces the
whole selector** (it is not additive), and the `--port` / `--health-*` flags work
as on create:

```bash
otherix lb update web-lb --selector app=web,tier=frontend   # replace the selector
otherix lb update web-lb --port 8080                        # change the traffic port
otherix lb update web-lb --health-port 9000 --health-interval 3
```

Delete removes the load balancer (the backing VMs are untouched):

```bash
otherix lb delete web-lb
```

## Declarative manifest

A load balancer can be described declaratively and applied with
`otherix create -f` (see [Declarative manifests](declarative-manifests.md)):

```yaml
apiVersion: otherix/v1
kind: LoadBalancer
metadata:
  name: web-lb
spec:
  port: 80                 # guest TCP port (required)
  selector:                # required, one or more entries; VMs selected by these labels
    app: web
  healthCheck:             # optional; omitted keys take server defaults
    port: 8080             # optional; omit = follow spec.port
    intervalSeconds: 10
    timeoutSeconds: 2
    healthyThreshold: 2
    unhealthyThreshold: 3
```

```bash
otherix create -f web-lb.yaml
```

The whole `healthCheck` block is optional, and any key omitted inside it takes
the server default. Omitting `healthCheck.port` makes the probe follow
`spec.port`, exactly like the `--health-port` flag. `otherix lb get web-lb -o
yaml` projects the live load balancer back into this same apply-ready shape, so
the manifest round-trips.

## Use cases

- **Stateless web pool.** Front a set of identical web VMs (`--label app=web`)
  with one load balancer on port 80 and let connections spread across them; scale
  by launching more VMs with the same label.
- **Read-replica pool.** Balance read traffic over a set of database read
  replicas (`--label role=replica`) so no single replica takes every query.
- **Separate health port.** Probe a dedicated `/health` port (`--health-port
  8080`) while serving application traffic on another port, so the health signal
  reflects real readiness rather than just an open socket.
