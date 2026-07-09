# AWS test harness

An ephemeral, on-demand Otherix cluster on AWS for real-world multi-node,
multi-AZ, and fault-injection testing. One stand is 3 control-plane nodes
(`m6g.large`, public), 2 agents (bare-metal arm64 with a local NVMe RAID0 pool,
private), and 2 gateways (`m6g.large`, public), in `eu-north-1`, on spot capacity
(CP-0 is on-demand). Each agent is a per-node EC2 Fleet that spreads its one
instance over `var.agent_instance_pool` (interchangeable metals: `c6gd.metal`,
`c7gd.metal`, `m6gd.metal`, `m7gd.metal`, `r6gd.metal`) with `price-capacity-optimized`,
so a single-pool `InsufficientInstanceCapacity` no longer blocks the stand.
DNS lives under the Route53 public zone `aws.otherix.dev`.

Everything is driven from the repo root through `make harness-*` targets. Each
target relies on the standard AWS credential chain (environment variables, an
`~/.aws` profile, SSO, or an instance role); `dev/aws/ensure-aws-creds.sh`
verifies a usable credential is present before running tofu. Configure
credentials however you like (for example `export AWS_PROFILE=<profile>` or
`aws sso login`).

Bring a stand up, exercise it, and always `harness-down` when finished; the
compute is billed by the hour.

## 1. One-time bootstrap (once per AWS account)

Create the two persistent resources every stand depends on: the OpenTofu state
bucket and the Route53 public hosted zone. See
[`../bootstrap/README.md`](../bootstrap/README.md) for the full detail.

```
cd deploy/terraform/bootstrap
tofu init
tofu apply
```

Then delegate the subdomain: take the `zone_name_servers` output and add ONE
`NS` record in Cloudflare on the parent `otherix.dev` zone:

```
aws.otherix.dev  NS  -> <bootstrap zone_name_servers>
```

Note the `state_bucket` output; the harness backend needs it.

## 2. Init the harness with the remote backend

```
cd deploy/terraform/test-harness
tofu init -backend-config="bucket=<state_bucket>"
```

## 3. Lifecycle (happy path)

Run these from the repo root.

Re-validate spot pricing and 90-day AZ stability before bring-up:

```
make harness-spot-report
```

The agent fleet already diversifies across the metal pool, so a single type
running dry no longer blocks a stand. Use the report to gate the region on
capacity (SPS >= 7) and then rank by price/stability; if the whole pool in
`eu-north-1a` / `eu-north-1c` has drifted, consider a different AZ or region.

Bring up a named stand (each `NAME` is its own tofu workspace):

```
make harness-up NAME=<env> [OTHERIX_VERSION=<ver>]
```

`OTHERIX_VERSION` is optional. Leave it unset and each node resolves the latest
published GitHub release at boot; pass it (the release tag WITHOUT a leading
`v`, e.g. `0.1.0`) to pin a specific version. Cloud-init reconstructs the
release URL and detects the node architecture with `dpkg --print-architecture`,
so the same templates install the right `arm64` or `amd64` artifacts if you
change the instance type.

Point the local `otherix` CLI at the stand (fetches the cluster CA over
trust-on-first-use and registers a CLI cluster profile):

```
make harness-config NAME=<env>
```

This logs in to mint an API token, so it needs the operator credentials the CLI
expects in the environment: `OTHERIX_LOGIN` and `OTHERIX_PASSWORD` (the
bootstrap admin seeded into the stand). See the header of
`dev/aws/harness-config.sh`.

Now drive the stand from your laptop. The CLI target is
`https://cp.<env>.aws.otherix.dev`. A healthy stand shows all seven nodes:

```
otherix node list    # 3 cp + 2 agent + 2 gateway
```

Tear the stand down (instances and DNS records gone; the state bucket and
Route53 zone persist):

```
make harness-down NAME=<env>
```

## 4. Storage pool on the fast NVMe

Each agent's local NVMe RAID0 is mounted at `/var/lib/otherix/pools/local`. To
land VM disks on the fast instance-store NVMe rather than the EBS root, create
the storage pool at that exact path:

```
otherix pool create <pool-name> --node <agent-node> --path /var/lib/otherix/pools/local
```

Run it once per agent (a pool name maps to one `(name, node)` row). A pool
created at any other path under `/var/lib/otherix/pools/` sits on the EBS root,
not the NVMe.

If the stand was applied with `raid0_pool=false`, the two NVMe devices mount
separately at `.../pools/local0` and `.../pools/local1` instead of the single
`.../pools/local`.

## 5. Published load-balancer port

The gateway security group opens only the ingress band `30000-32767` (the
`published_port_from` / `published_port_to` variables). Any harness published
load balancer MUST use a `--publish-port` inside that band:

```
otherix lb create <name> --selector <sel> --publish --publish-port <30000-32767>
```

It is then reachable from the public internet at
`gw.<env>.aws.otherix.dev:<port>`.

## 6. Chaos runbook

Permanent host loss (terminates the instance; for an agent this also destroys
its instance-store NVMe pool). Tests node-gone handling, VM re-home / loss, and
etcd quorum loss - one CP is survivable, killing two loses quorum until you
re-apply:

```
make harness-chaos-kill NAME=<env> ROLE=<cp|agent|gateway> [INDEX=n]
```

Temporary full network isolation via a blackhole security group, then restore.
Unlike a stop, a partition PRESERVES the agent instance-store pool - which is
exactly why the temporary-fault primitive is partition, not stop/start:

```
make harness-chaos-partition NAME=<env> ROLE=<cp|agent|gateway> [INDEX=n]
make harness-chaos-heal      NAME=<env> ROLE=<cp|agent|gateway> [INDEX=n]
```

Degrade (not sever) a link with `tc netem` over SSM RunCommand:

```
make harness-chaos-latency NAME=<env> ROLE=<cp|agent|gateway> [INDEX=n] [DELAY=ms] [LOSS=pct]
```

Reverse latency with `harness-chaos-heal`. PREREQUISITE: the target must run
the `amazon-ssm-agent` and be registered with SSM. Ubuntu AMIs do not always
ship it; when the node is not SSM-managed, `latency` reports that and does
nothing. `kill` and `partition` do NOT need SSM.

WARNING: never `aws ec2 stop-instances` a bare-metal agent to simulate a
temporary fault - the instance-store pool is wiped on stop. Use `partition`.

## 7. Cost

Roughly `$0.65/hr` compute on spot plus about `$0.045/hr` for the single NAT
gateway. CP-0 runs on-demand because it is the bootstrap seed and formation
single point of failure. An ephemeral session runs a few dollars; always
`harness-down` when finished.

## 8. Recovery: CP-0 lost during formation

If CP-0 is lost during formation (before the cluster forms and the join tokens
seed), the stand can wedge: joiners keep polling a dead CP-0 against tokens that
were never seeded. Recover by re-running bring-up so tofu re-creates the missing
instance:

```
make harness-up NAME=<env>
```

If that does not clear it, do a full `harness-down` followed by `harness-up`.
