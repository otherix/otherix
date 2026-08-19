# ========== Variables ==========

BINARIES        := api agent
BIN_DIR         := bin
GO              := go
GOLANGCI_VERSION := v2.12.2
GOLANGCI_IMAGE   := golangci/golangci-lint:$(GOLANGCI_VERSION)
# GOIMPORTS_VERSION tracks golang.org/x/tools in go.mod (goimports ships from it).
GOFUMPT_VERSION  := v0.7.0
GOIMPORTS_VERSION := v0.44.0

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
  -X github.com/otherix/otherix/internal/version.Version=$(VERSION) \
  -X github.com/otherix/otherix/internal/version.Commit=$(COMMIT) \
  -X github.com/otherix/otherix/internal/version.Date=$(DATE)

REDOCLY_VERSION    = 2.31.2
SWAGGER_UI_VERSION = v5.17.14
REDOC_VERSION      = v2.2.0
API_PREVIEW_COMPOSE = docker compose -f tools/api-preview/docker-compose.yaml

.DEFAULT_GOAL := help

# ========== Help ==========

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage: make <target>\n\n"} \
	    /^# ==========/ {section=$$0; gsub(/# =+ */, "", section); gsub(/ =+$$/, "", section); printf "\n\033[1m%s\033[0m\n", section} \
	    /^[a-zA-Z0-9_-]+:.*##/ {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ========== Build ==========

.PHONY: build $(addprefix build-,$(BINARIES)) build-cli build-ssh
build: $(addprefix build-,$(BINARIES)) build-cli build-ssh ## Build all binaries for current platform

$(addprefix build-,$(BINARIES)): build-%: ## Build a single daemon binary (api/agent)
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/otherix-$* ./cmd/$*

# CLI is a special case: cmd dir is `cli/` for layout consistency with the
# daemons (cmd/<short>/), but the binary is `otherix` (no component
# suffix) per the kubectl/docker/gh convention for operator CLIs.
build-cli: ## Build the otherix operator CLI to bin/otherix
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/otherix ./cmd/cli

# otherix-ssh is the thin SSH-only connector an external person installs to
# reach a granted VM; it ships separately from the operator CLI and daemons.
build-ssh: ## Build the otherix-ssh external connector to bin/otherix-ssh
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/otherix-ssh ./cmd/otherix-ssh

.PHONY: build-linux-amd64 build-linux-arm64
build-linux-amd64: ## Cross-compile all daemons + otherix-ssh for linux/amd64
	@mkdir -p $(BIN_DIR)/linux-amd64
	@for b in $(BINARIES); do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
	    -o $(BIN_DIR)/linux-amd64/otherix-$$b ./cmd/$$b || exit 1; \
	done
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
	  -o $(BIN_DIR)/linux-amd64/otherix-ssh ./cmd/otherix-ssh

build-linux-arm64: ## Cross-compile all daemons + otherix-ssh for linux/arm64
	@mkdir -p $(BIN_DIR)/linux-arm64
	@for b in $(BINARIES); do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
	    -o $(BIN_DIR)/linux-arm64/otherix-$$b ./cmd/$$b || exit 1; \
	done
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
	  -o $(BIN_DIR)/linux-arm64/otherix-ssh ./cmd/otherix-ssh

.PHONY: clean
clean: ## Remove build artifacts (bin/, coverage reports)
	rm -rf $(BIN_DIR) coverage.out coverage.html

# ========== Test ==========

# Build tag list shared across test targets. `test_fast_argon` swaps
# the OWASP argon2 cost parameters for cheap ones (RFC 9106 minimum)
# inside the test binary — see internal/auth/argon_fast.go. Production
# builds never pass this tag and keep the OWASP defaults.
TEST_TAGS := test_fast_argon
INTEGRATION_TAGS := integration,$(TEST_TAGS)

.PHONY: test test-short test-etcd test-etcd-fast test-netfabric test-netfabric-native coverage

# The etcd-backed suites share one in-process member per test binary (TestMain),
# so the wall-clock is dominated by the work, not member churn.
ETCD_TEST_PKGS := ./internal/etcd/... ./internal/etcdstore/... ./internal/api/handlers/migrations/... ./internal/api/handlers/heartbeat/... ./cmd/api/... ./tests/apie2e/...
test: ## Run unit tests with race detector and coverage
	$(GO) test ./... -race -tags=$(TEST_TAGS) -coverprofile=coverage.out

test-short: ## Run unit tests in short mode
	$(GO) test ./... -short -tags=$(TEST_TAGS)

# test-etcd runs the etcd-backed suites: the store layer (internal/etcdstore)
# and the api-server e2e (tests/apie2e). Both embed etcd in-process, so they
# need NO Docker - this is the integration test path after the pgx cutover.
test-etcd: ## Run etcd-backed store + api e2e suites with -race (no Docker; CI gate)
	$(GO) test -tags=$(INTEGRATION_TAGS) -count=1 -race $(ETCD_TEST_PKGS)

test-etcd-fast: ## Same etcd-backed suites without -race (fast local iteration; the gate keeps -race via test-etcd)
	$(GO) test -tags=$(INTEGRATION_TAGS) -count=1 $(ETCD_TEST_PKGS)

# test-netfabric runs the agent network-fabric netns integration tests
# (bridge / tap / nft masquerade over real netlink). They are
# //go:build linux && integration and need root (CAP_NET_ADMIN). The Lima
# dev VM runs only the agent binary - no Go toolchain, no source - so we
# CROSS-COMPILE the test binary on the macOS host, copy it in, and run it as
# root, exactly like build-agent-lima / copy-agent-lima. Run this FROM THE
# macOS HOST (`make test-netfabric`), not from inside Lima. Not in CI (needs
# CAP_NET_ADMIN). On a native Linux host just run the $(GO) test line directly as root.
test-netfabric: lima-ensure ## netfabric netns integration tests in Lima (cross-compiled on host, run as root)
	@mkdir -p $(BIN_DIR)
	@arch=$$(limactl shell $(LIMA_VM) uname -m); \
	case "$$arch" in \
	  aarch64) goarch=arm64 ;; \
	  x86_64)  goarch=amd64 ;; \
	  *) echo "unsupported lima arch: $$arch"; exit 1 ;; \
	esac; \
	out=$(BIN_DIR)/netfabric.test; \
	echo ">> cross-compiling netfabric integration test for linux/$$goarch"; \
	CGO_ENABLED=0 GOOS=linux GOARCH=$$goarch $(GO) test -tags=$(INTEGRATION_TAGS) -c -o $$out ./internal/agent/netfabric/; \
	limactl cp $$out $(LIMA_VM):/tmp/netfabric.test; \
	echo ">> running netns integration tests as root in $(LIMA_VM)"; \
	limactl shell $(LIMA_VM) sudo /tmp/netfabric.test -test.v -test.count=1

# test-netfabric-native runs the SAME netns data-plane suite on a NATIVE
# linux host as root - no Lima, no cross-compile. This is the CI entrypoint
# (a privileged GitHub ubuntu runner): build the test binary as the normal
# user, then run it under sudo. OTHERIX_NETFABRIC_REQUIRE=1 turns the
# capability-missing skips (no CAP_NET_ADMIN / nftables / wireguard) into
# HARD failures, so the data plane can never pass green by being silently
# skipped - the whole point of putting it in CI. Needs root + the
# wireguard / vxlan / nft kernel modules loaded (the CI job modprobes them).
test-netfabric-native: ## netfabric netns tests on a native linux root host (CI)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) test -tags=$(INTEGRATION_TAGS) -c -o $(BIN_DIR)/netfabric.test ./internal/agent/netfabric/
	sudo OTHERIX_NETFABRIC_REQUIRE=1 $(BIN_DIR)/netfabric.test -test.v -test.count=1

coverage: test ## Generate HTML coverage report
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "open coverage.html"

.PHONY: smoke-ha
smoke-ha: ## HA multi-process smoke: 3 real api-server nodes form a cluster over peer mTLS (no Docker, no Lima)
	bash dev/scripts/smoke-ha.sh

.PHONY: smoke-tls
smoke-tls: ## CLI-over-HTTPS smoke: user /v1 API on TLS, `otherix config add cluster` pins the cluster CA inline (no Docker, no Lima)
	bash dev/scripts/smoke-tls.sh

.PHONY: smoke-networking
smoke-networking: ## Networking operator smoke: drives `otherix network`/`vm create --network` against the real Lima agent (run after local-dev-start)
	bash dev/smoke/networking/run.sh

.PHONY: smoke-wireguard-mesh
smoke-wireguard-mesh: ## Cross-agent WireGuard mesh smoke: real cross-host handshake between the two Lima nodes (run after local-dev-start)
	bash dev/smoke/wireguard-mesh/run.sh

.PHONY: smoke-overlay-vm
smoke-overlay-vm: ## Overlay VM-to-VM smoke: two real VMs cross-node ping over the overlay via CP-distributed FDB (run after local-dev-start)
	bash dev/smoke/overlay-vm/run.sh

.PHONY: smoke-geo-nat
smoke-geo-nat: ## Geo NAT smoke: CP drives VMs onto two NAT'd nodes via the gateway splice + guest VXLAN relayed hub-and-spoke + down-path gate (Lima only; run after local-dev-deploy)
	bash dev/smoke/geo-nat/run.sh

.PHONY: smoke-manifests
smoke-manifests: ## YAML-manifest CLI smoke: `otherix create -f` / `get -o yaml` / `delete -f` against the real Lima agent (run after local-dev-start)
	bash dev/smoke/manifests/run.sh

.PHONY: smoke-vm-lifecycle
smoke-vm-lifecycle: ## VM lifecycle smoke: `otherix vm` start/stop/poweroff/reboot/pause/resume/reset on a real agent (run after local-dev-start)
	bash dev/smoke/vm-lifecycle/run.sh

.PHONY: smoke-scheduler-memory-overcommit
smoke-scheduler-memory-overcommit: ## Memory overcommit smoke: a VM lands only on a zram-equipped node (run after local-dev-deploy)
	bash dev/smoke/scheduler-memory-overcommit/run.sh

.PHONY: smoke-vm-network-config
smoke-vm-network-config: ## VM network-config smoke: static guest IP via `otherix vm create --network-config` on a real agent (run after local-dev-start)
	bash dev/smoke/vm-network-config/run.sh

.PHONY: smoke-vm-ssh
smoke-vm-ssh: ## VM SSH-ingress smoke: real guest login over the relay via `otherix ssh` + external `ssh-grant`/`otherix-ssh` (operator, external, add-vm, revoke; run after local-dev-start)
	bash dev/smoke/vm-ssh/run.sh

.PHONY: smoke-ingress-gateway
smoke-ingress-gateway: ## Ingress-gateway smoke: brokered `otherix forward`/`otherix ssh` reach a guest (gateway + relay), survive a live migration, recover from gateway death, and enforce no-gateway 409 + non-owner 404 (run after local-dev-start)
	bash dev/smoke/ingress-gateway/run.sh

.PHONY: smoke-ingress-gateway-colocated
smoke-ingress-gateway-colocated: ## Co-located ingress-gateway smoke: a hypervisor node also serves ingress; `otherix node gateway enable` + `otherix forward` reach a guest on another node, multi-overlay works, and a role move keeps a live session (run after local-dev-start)
	bash dev/smoke/ingress-gateway-colocated/run.sh

.PHONY: smoke-ingress-grant
smoke-ingress-grant: ## Ingress-grant smoke: an external grant reaches a VM with `otherix forward` using only the grant token (gateway + relay), enforces per-port scope + source-IP pin (404), and survives a live migration (run after local-dev-start)
	bash dev/smoke/ingress-grant/run.sh

.PHONY: smoke-lb
smoke-lb: ## Load-balancer smoke: otherix lb fronts a label-selected VM pool, balances across backends, excludes a stopped backend, and 409s when none are eligible (run after local-dev-start)
	bash dev/smoke/lb/run.sh

.PHONY: smoke-lb-health
smoke-lb-health: ## Load-balancer active-health smoke: a warming LB still serves, a backend whose traffic port closes (VM still running) is excluded on the health verdict and rejoins on re-open, a split health port is honored, and an all-down pool 409s (run after local-dev-start)
	bash dev/smoke/lb-health/run.sh

.PHONY: smoke-published-port
smoke-published-port: ## Published-LB-port traffic smoke: a raw peer client reaches a backend guest THROUGH `otherix lb ... --publish` on a gateway-role node, a source-CIDR ACL excluding the client fails closed, a powered-off backend is never mis-routed, and `--no-publish` reaps the listener (run after local-dev-deploy)
	bash dev/smoke/published-port/run.sh

.PHONY: smoke-vm-create-redelivery
smoke-vm-create-redelivery: ## VM create-redelivery smoke: agent restart + vm.create redelivery does not clobber a live VM and reconciles to success (audit R2-M1/M2; run after local-dev-start)
	bash dev/smoke/vm-create-redelivery/run.sh

.PHONY: smoke-node-drain
smoke-node-drain: ## Node-drain smoke: `otherix node drain` evacuates a node's VMs to other nodes (task success) and leaves a stuck VM running on timeout (task failed); run after local-dev-start
	bash dev/smoke/node-drain/run.sh

.PHONY: smoke-node-lifecycle
smoke-node-lifecycle: ## Node-lifecycle smoke: `otherix node delete` an empty node, `node delete --force` a VM-hosting node (VM orphaned, agent cert revoked, re-join accepts a fresh cert), and automatic self-heal of a stopped node (unreachable -> ready on reconnect, no readmit) (run after local-dev-start)
	bash dev/smoke/node-lifecycle/run.sh

.PHONY: smoke-node-placement-pressure
smoke-node-placement-pressure: ## Node-placement-pressure smoke: a node under disk pressure is excluded from placement - VMs avoid it, a pinned VM stays pending, and clearing the pressure lets placement converge (run after local-dev-start)
	bash dev/smoke/node-placement-pressure/run.sh

.PHONY: smoke-zram-safety-net
smoke-zram-safety-net: ## zram safety-net smoke: enabling zram brings up a mem_limit-capped swap device, `node get` reports compressed_swap.kind=zram, pages spilled under bounded cgroup pressure compress in zram, and disabling tears the device down (run after local-dev-start)
	bash dev/smoke/zram-safety-net/run.sh

.PHONY: smoke-user
smoke-user: ## User CLI smoke: otherix user create/get/set-role/set-password/delete + login + RBAC gating (run after local-dev-start)
	bash dev/smoke/user/run.sh

.PHONY: smoke-api-token
smoke-api-token: ## api-token CLI smoke: otherix api-token create/list/revoke + token auth lifecycle (run after local-dev-start)
	bash dev/smoke/api-token/run.sh

.PHONY: smoke-vm-migration
smoke-vm-migration: ## Offline VM migration smoke: `otherix vm migrate --offline` across two nodes (run after local-dev-start)
	bash dev/smoke/vm-migration/run.sh

.PHONY: smoke-vm-migration-live
smoke-vm-migration-live: ## Live VM migration smoke: `otherix vm migrate` (live) across two nodes, asserts console-heartbeat continuity (run after local-dev-start)
	bash dev/smoke/vm-migration-live/run.sh

.PHONY: smoke-vm-migration-live-stats
smoke-vm-migration-live-stats: ## Live VM migration stats smoke: live-migrate, assert `migration get` shows the statistics section with sane ram.total/total_time_ms (run after local-dev-start)
	bash dev/smoke/vm-migration-live-stats/run.sh

.PHONY: smoke-vm-migration-live-overlay
smoke-vm-migration-live-overlay: ## Live VM migration + overlay smoke: live-migrate an overlay-attached VM, asserts cross-node overlay connectivity follows the guest at cutover (run after local-dev-start)
	bash dev/smoke/vm-migration-live-overlay/run.sh

.PHONY: smoke-vm-migration-live-bridge
smoke-vm-migration-live-bridge: ## Live VM migration + bridge smoke: live-migrate a type=bridge-attached VM, asserts L2 connectivity follows the guest at cutover via announce-self (run after local-dev-start)
	bash dev/smoke/vm-migration-live-bridge/run.sh

.PHONY: smoke-vm-migration-live-default-disk
smoke-vm-migration-live-default-disk: ## Real-agent smoke: live-migrate an image-sized (no --disk-gib) VM
	@bash dev/smoke/vm-migration-live-default-disk/run.sh

.PHONY: smoke-vm-migration-live-cleanup
smoke-vm-migration-live-cleanup: ## Real-agent smoke: repeated live migration (1->2->1->2) validates the migtls + port-leak cleanup fix (run after local-dev-start)
	@dev/smoke/vm-migration-live-cleanup/run.sh

.PHONY: smoke-vm-migration-live-logs
smoke-vm-migration-live-logs: ## Live VM migration + logs smoke: `vm logs -f` must follow the VM across cutover with a gapless SEQ stream (run after local-dev-start)
	@dev/smoke/vm-migration-live-logs/run.sh

.PHONY: smoke-vm-migration-live-console
smoke-vm-migration-live-console: ## Live VM migration + console smoke: an open `vm console` session must follow the VM across cutover (PRE+POST markers echo on the SAME WS) (run after local-dev-start)
	@dev/smoke/vm-migration-live-console/run.sh

.PHONY: smoke-dhcp-overlay-teardown
smoke-dhcp-overlay-teardown: ## DHCP-overlay teardown smoke: repeated create/delete of a dhcp overlay must never wedge the agent network reconciler (run after local-dev-start)
	@dev/smoke/dhcp-overlay-teardown/run.sh

.PHONY: smoke-overlay-isolated-dns
smoke-overlay-isolated-dns: ## Isolated-overlay DNS smoke: dhcp without egress hands out IP + DNS, no default route
	bash dev/smoke/overlay-isolated-dns/run.sh

.PHONY: smoke-bridge-managed-dhcp
smoke-bridge-managed-dhcp: ## Managed-bridge DHCP/DNS smoke: dhcp + dns + nat egress hands out IP + DNS + default route via 169.254.1.1
	bash dev/smoke/bridge-managed-dhcp/run.sh

.PHONY: smoke-bridge-managed-dns-false
smoke-bridge-managed-dns-false: ## Managed-bridge dns=false smoke: a manifest with dhcp:true,dns:false round-trips, the VM gets a lease + default route but no resolver (option 6 withheld)
	bash dev/smoke/bridge-managed-dns-false/run.sh

.PHONY: smoke-chaos-cp-crash-migrate
smoke-chaos-cp-crash-migrate: ## Chaos: SIGKILL the CP mid vm.migrate; the lease reaper reclaims the stranded job, the migration recovers, the VM is migratable again (P0; run after local-dev-start)
	@bash dev/smoke/chaos-cp-crash-migrate/run.sh

.PHONY: smoke-chaos-target-crash-incoming
smoke-chaos-target-crash-incoming: ## Chaos: crash the target agent mid-incoming; recovery reaps the orphaned inmigrate qemu (no leak), the source stays safe (P1a; run after local-dev-start)
	@bash dev/smoke/chaos-target-crash-incoming/run.sh

.PHONY: smoke-vm-migration-cancel
smoke-vm-migration-cancel: ## Migration cancel propagation: `otherix migration cancel` aborts the source + reaps the target PROMPTLY (no agent timeout); task finalizes cancelled, source stays running (run after local-dev-start)
	@bash dev/smoke/vm-migration-cancel/run.sh

.PHONY: smoke-vm-snapshots
smoke-vm-snapshots: ## VM snapshot smoke: `otherix vm snapshot create` then `vm create --from-snapshot`; asserts the source's post-boot disk state survives the snapshot -> recreate and the restored guest hostname is the new VM name (run after local-dev-start)
	@bash dev/smoke/vm-snapshots/run.sh

.PHONY: smoke-artifact-pool
smoke-artifact-pool: ## Artifact-pool concept smoke (slice B): create pool, assert `vm create --pool <artifact>` -> pool_role_invalid, snapshot into the pool carries artifact_pool_name, recreate boots, fail-closed delete blocked by referencing snapshots (run after local-dev-start)
	@bash dev/smoke/artifact-pool/run.sh

.PHONY: smoke-artifact-pull
smoke-artifact-pull: ## Cross-node artifact-pull smoke (slice C1): snapshot on node-1, recreate-from-snapshot STEERED to the non-producing node-2 forces a CP-brokered peer pull (node-2 <- node-1) before materialize; asserts pull-dst boots on node-2 off the pulled blob (run after local-dev-start)
	@bash dev/smoke/artifact-pull/run.sh

.PHONY: smoke-artifact-durability
smoke-artifact-durability: ## Artifact durability smoke: snapshot replicates to the pool replication factor, survives a holder loss, and re-replicates onto a surviving node (run after local-dev-start)
	@bash dev/smoke/artifact-durability/run.sh

.PHONY: smoke-artifact-pool-rf-update
smoke-artifact-pool-rf-update: ## Artifact-pool RF update smoke: raising replication_factor re-replicates existing snapshots to the new factor (run after local-dev-start)
	@bash dev/smoke/artifact-pool-rf-update/run.sh

.PHONY: smoke-artifact-gc
smoke-artifact-gc: ## Artifact GC smoke: the blob collector reclaims orphaned and over-replicated copies and never reclaims a copy a snapshot still references (run after local-dev-start)
	@bash dev/smoke/artifact-gc/run.sh

.PHONY: smoke-artifact-janitor
smoke-artifact-janitor: ## Artifact janitor smoke: the agent boot-time store hygiene sweep clears staging and repairs a missing sidecar, and the CP backstop reclaims a leaked blob whose placement record was lost while never touching a referenced blob (run after local-dev-start)
	@bash dev/smoke/artifact-janitor/run.sh

.PHONY: smoke-blob-scrub
smoke-blob-scrub: ## Real-agent smoke: blob scrubber detects corruption and durability heals (3-node)
	@bash dev/smoke/blob-scrub/run.sh

.PHONY: smoke-node-cache-hygiene
smoke-node-cache-hygiene: ## Node-cache-hygiene smoke: a batch of concurrent creates from one new unpinned --image-url on one node downloads the image once (within-node import coalescing), a stray file in a pool's legacy basename image cache is swept on agent restart (the images/ dir survives), and a stray .staging orphan in the node image store is swept by the artifact sweeper boot pass (run after local-dev-start)
	@bash dev/smoke/node-cache-hygiene/run.sh

.PHONY: smoke-image-cache
smoke-image-cache: ## Image-cache smoke: a pinned image (--image-sha256) is peer-pulled onto a second node from a node that already caches it (cache hit, no re-download), the image tier stays out of the durability placement map, and a no-peer-holder create falls back to a source download and boots (run after local-dev-start)
	@bash dev/smoke/image-cache/run.sh

.PHONY: smoke-image-cache-eviction
smoke-image-cache-eviction: ## Image-cache eviction smoke: caching a second pinned image over the (dev-shrunk) ceiling evicts the coldest cached image first (LRU), a VM created from the evicted image keeps running (its disk is an independent copy), and recreating from the evicted image re-fetches it from source (run after local-dev-start)
	@bash dev/smoke/image-cache-eviction/run.sh

.PHONY: smoke-unpinned-peer-pull
smoke-unpinned-peer-pull: ## Unpinned peer-pull smoke: a VM from an UNPINNED --image-url is downloaded on node-1 and registered URL -> digest; a second node creating from the same URL peer-pulls the image instead of re-downloading; a third create with --pull-policy always force-downloads despite the peer holders; `vm get -o yaml` round-trips imagePullPolicy: always (run after local-dev-start)
	@bash dev/smoke/unpinned-peer-pull/run.sh

# smoke-all runs the stack-dependent smokes in sequence (fail-fast) against a
# stand brought up by `make local-dev-start`. smoke-ha is NOT included — it
# spins its own 3 api-server processes and does not use the dev stand; run it
# separately.
.PHONY: smoke-all
smoke-all: ## Run all stack-dependent smokes in sequence (run after local-dev-start; excludes smoke-ha)
	@for s in networking wireguard-mesh overlay-vm manifests vm-lifecycle vm-migration vm-migration-live vm-network-config ingress-grant; do \
	  echo ">> smoke: $$s"; \
	  bash dev/smoke/$$s/run.sh || { echo "✗ smoke-$$s failed"; exit 1; }; \
	done
	@echo ">> smoke-all complete (smoke-ha is standalone — run 'make smoke-ha' separately)"

# ========== Lint ==========

.PHONY: lint fmt fmt-check vet
lint: ## Run golangci-lint for the host platform AND GOOS=linux (a macOS host build excludes *_linux.go, which CI on ubuntu lints; this matches CI)
	@if command -v golangci-lint >/dev/null 2>&1; then \
	  echo ">> using local golangci-lint ($$(golangci-lint version 2>/dev/null | head -n1))"; \
	  rc=0; \
	  for goos in "$$(go env GOHOSTOS)" linux; do \
	    echo ">> golangci-lint run (GOOS=$$goos)"; \
	    GOOS=$$goos golangci-lint run --timeout 5m || rc=1; \
	  done; \
	  exit $$rc; \
	else \
	  echo ">> golangci-lint not found locally, using docker $(GOLANGCI_IMAGE)"; \
	  rc=0; \
	  for goos in linux "$$(go env GOHOSTOS)"; do \
	    echo ">> golangci-lint run (GOOS=$$goos) in docker"; \
	    docker run --rm -e GOOS=$$goos -v $(PWD):/app -w /app $(GOLANGCI_IMAGE) golangci-lint run --timeout 5m || rc=1; \
	  done; \
	  exit $$rc; \
	fi

fmt: ## Format with gofumpt + goimports (installed locally on demand)
	$(GO) run mvdan.cc/gofumpt@$(GOFUMPT_VERSION) -l -w .
	$(GO) run golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION) -local github.com/otherix/otherix -l -w .

fmt-check: ## Verify formatting without writing (fails if any file needs gofumpt/goimports)
	@out="$$($(GO) run mvdan.cc/gofumpt@$(GOFUMPT_VERSION) -l .; \
	         $(GO) run golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION) -local github.com/otherix/otherix -l .)"; \
	if [ -n "$$out" ]; then echo ">> these files need formatting (run 'make fmt'):"; echo "$$out" | sort -u; exit 1; fi

vet: ## go vet all packages
	$(GO) vet ./...

# ci reproduces the merge-gating CI jobs locally, minus the privileged netfabric
# data-plane suite (needs root / CAP_NET_ADMIN - run `make test-netfabric`
# separately). Mirrors .github/workflows/ci.yaml: lint, vuln, test-unit,
# test-etcd, and the agent-api drift check.
.PHONY: ci
ci: fmt-check vet lint test test-etcd vuln agent-api-verify ## Run the local CI gate (everything CI gates except privileged netfabric)
	@echo ">> ci gate passed"

.PHONY: vuln
vuln: ## Scan for known vulnerabilities via govulncheck (pinned)
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.3.0 ./...

# ========== Modules ==========

.PHONY: tidy download
tidy: ## go mod tidy
	$(GO) mod tidy

download: ## go mod download
	$(GO) mod download

# ========== Local dev stack ==========

# local-dev-* is the documented dev-stack control surface (macOS Lima 3 VMs /
# Linux netns 3 nodes). start/stop/clean/cleanrestart are the lifecycle;
# restart/deploy are the non-destructive inner loop (etcd/pki + VMs/certs
# preserved). The per-OS internals (bootstrap-dev/deploy-dev/clean-dev,
# *-linux/*-macos, lima-*) are hidden from `make help`.
.PHONY: local-dev-start local-dev-stop local-dev-destroy local-dev-clean \
        local-dev-restart local-dev-deploy local-dev-cleanrestart

local-dev-start: ## Dev stack up: api-server (embedded etcd) + agents + CLI (admin@otherix.local / correct-horse-battery-staple)
	@bash dev/scripts/local-dev-start.sh

local-dev-stop: ## Dev stack down: stop api + stop VMs (kept, reused on next start) + wipe etcd/pki
	@bash dev/scripts/local-dev-stop.sh

local-dev-destroy: ## Full nuke: stop api + DELETE VMs/netns + wipe etcd/pki (forces a from-scratch VM rebuild)
	@bash dev/scripts/local-dev-stop.sh --destroy

local-dev-clean: ## local-dev-stop + remove .local/ and the dev CLI cluster (pristine slate)
	@bash dev/scripts/local-dev-clean.sh

local-dev-restart: ## Bounce api + agents in place, no rebuild (state preserved)
	@bash dev/scripts/restart-api-dev.sh
	@$(MAKE) --no-print-directory restart-agent

local-dev-deploy: build-api build-cli ## Rebuild + restart api + agents + host CLI to pick up code changes (state preserved)
	@$(MAKE) --no-print-directory deploy-dev
	@bash dev/scripts/restart-api-dev.sh

local-dev-cleanrestart: ## Fresh cluster (wipe etcd + re-seed); VMs are stopped and reused, not rebuilt
	@$(MAKE) --no-print-directory local-dev-stop
	@$(MAKE) --no-print-directory local-dev-start

# etcd-reset wipes the dev member's gitignored data dir AND the dev PKI for a
# clean-slate smoke run. The api-server recreates the data dir, regenerates the
# on-disk cluster CA + peer cert, and bootstraps the admin on next boot. Wiping
# both in lockstep avoids a disk-CA/etcd-CA divergence (the on-disk CA is the
# source of truth synced into etcd at boot). Paths mirror dev/config/api.yaml.
.PHONY: etcd-reset
etcd-reset: ## Wipe the dev embedded-etcd data dir + PKI for a clean-slate smoke run
	rm -rf .local/etcd .local/pki

# ----- Dev environment internals (per-OS dispatchers; not in make help) -----

# Dev pipeline: build agent + run as systemd unit. Linux runs natively
# (user-mode systemd unit). macOS runs the agent inside a Lima VM
# (Ubuntu 24.04, system unit) and reaches the CP via host.lima.internal.
# See docs/macos-development.md for the macOS path.
# The macOS dev stack runs THREE Lima VMs so the WireGuard underlay has a real
# cross-host mesh and a blob can re-replicate to a surviving node after a holder
# dies: otherix-dev-1 (node-1), otherix-dev-2 (node-2), otherix-dev-3 (node-3).
# All join the user-v2 network for VM-to-VM L3; each VM's CP->agent forward maps
# guest 9443 to a distinct host port (VM1=9443, VM2=9444, VM3=9445). LIMA_VM is
# the primary VM (node-1), used by the single-host operations: the netns
# netfabric tests and the networking smoke.
LIMA_VM_1   := otherix-dev-1
LIMA_VM_2   := otherix-dev-2
LIMA_VM_3   := otherix-dev-3
LIMA_VM     := $(LIMA_VM_1)

# dev-zram-on / dev-zram-off toggle the host compressed-swap net (zram) on a Lima
# dev node the way an operator would - via systemd-zram-generator. The agent is
# unprivileged and only OBSERVES the result, so provisioning is a host action;
# these run it for you over `limactl shell ... sudo`. NODE selects the Lima VM
# (default node-1); ZRAM_SIZE / ZRAM_ALGO tune the device. Lima-only (the Linux
# netns dev stack shares the real host kernel, so toggling zram there is refused).
NODE      ?= $(LIMA_VM_1)
ZRAM_SIZE ?= ram / 4
ZRAM_ALGO ?= zstd

.PHONY: dev-zram-on dev-zram-off

dev-zram-on: ## Enable host zram on a Lima dev node (NODE=otherix-dev-1 ZRAM_SIZE='ram / 4' ZRAM_ALGO=zstd)
	@command -v limactl >/dev/null || { echo "limactl not found - dev-zram-* is for the macOS Lima dev stack"; exit 1; }
	@echo ">> enabling zram on $(NODE) (size='$(ZRAM_SIZE)' algo=$(ZRAM_ALGO))"
	@limactl shell $(NODE) sudo bash -c 'apt-get update -qq && apt-get install -y -qq linux-modules-extra-$$(uname -r) systemd-zram-generator'
	@printf '[zram0]\nzram-size = %s\ncompression-algorithm = %s\nswap-priority = 100\n' '$(ZRAM_SIZE)' '$(ZRAM_ALGO)' | limactl shell $(NODE) sudo tee /etc/systemd/zram-generator.conf >/dev/null
	@limactl shell $(NODE) sudo sh -c 'systemctl daemon-reload && systemctl restart systemd-zram-setup@zram0'
	@limactl shell $(NODE) swapon --show
	@echo ">> zram on. verify: otherix node get <node> -o json | jq .capabilities.compressed_swap"

dev-zram-off: ## Disable host zram on a Lima dev node (NODE=otherix-dev-1)
	@command -v limactl >/dev/null || { echo "limactl not found - dev-zram-* is for the macOS Lima dev stack"; exit 1; }
	@echo ">> disabling zram on $(NODE)"
	@limactl shell $(NODE) sudo sh -c 'systemctl stop systemd-zram-setup@zram0 2>/dev/null; rm -f /etc/systemd/zram-generator.conf; systemctl daemon-reload; swapoff /dev/zram0 2>/dev/null; true'
	@limactl shell $(NODE) swapon --show || true
	@echo ">> zram off."

dev-zram-stat: ## Show host zram utilization on a Lima dev node (NODE=otherix-dev-1)
	@command -v limactl >/dev/null || { echo "limactl not found - dev-zram-* is for the macOS Lima dev stack"; exit 1; }
	@limactl shell $(NODE) sudo sh -c '\
	  d=/sys/block/zram0; \
	  [ -e $$d/mm_stat ] || { echo "zram not active on $(NODE)"; exit 0; }; \
	  set -- $$(cat $$d/mm_stat); orig=$$1; compr=$$2; memused=$$3; \
	  disksize=$$(cat $$d/disksize); \
	  echo "swap (logical):"; swapon --show=NAME,SIZE,USED,PRIO; \
	  echo; \
	  printf "disksize (ceiling):        %6s MiB\n" $$((disksize/1048576)); \
	  printf "orig  (logical swapped):   %6s MiB  (%s%% of ceiling)\n" $$((orig/1048576)) $$((orig*100/disksize)); \
	  printf "compr (physical RAM used): %6s MiB\n" $$((compr/1048576)); \
	  printf "mem_used_total:            %6s MiB\n" $$((memused/1048576)); \
	  [ $$compr -gt 0 ] && printf "compression ratio:         %s.%02sx\n" $$((orig/compr)) $$(((orig*100/compr)%100)) || true'

# bootstrap-dev / deploy-dev / clean-dev / restart-agent are internal per-OS
# dispatchers used by the local-dev-* family. They are intentionally NOT in
# `make help` (no `##`) — the documented surface is local-dev-*.
.PHONY: bootstrap-dev deploy-dev clean-dev destroy-dev restart-agent seed-dev \
        bootstrap-dev-linux deploy-dev-linux clean-dev-linux \
        bootstrap-dev-macos deploy-dev-macos clean-dev-macos destroy-dev-macos \
        lima-check lima-ensure lima-ensure-one \
        build-agent-lima copy-agent-lima copy-config-lima \
        restart-agent-lima

bootstrap-dev:
	@case "$$(uname -s)" in \
	  Linux)  $(MAKE) bootstrap-dev-linux ;; \
	  Darwin) $(MAKE) bootstrap-dev-macos ;; \
	  *) echo "unsupported OS: $$(uname -s)"; exit 1 ;; \
	esac

deploy-dev:
	@case "$$(uname -s)" in \
	  Linux)  $(MAKE) deploy-dev-linux ;; \
	  Darwin) $(MAKE) deploy-dev-macos ;; \
	  *) echo "unsupported OS: $$(uname -s)"; exit 1 ;; \
	esac

clean-dev:
	@case "$$(uname -s)" in \
	  Linux)  $(MAKE) clean-dev-linux ;; \
	  Darwin) $(MAKE) clean-dev-macos ;; \
	  *) echo "unsupported OS: $$(uname -s)"; exit 1 ;; \
	esac

# destroy-dev is the full-delete teardown. On Linux the netns topology is torn
# down completely either way (there is no "reuse" of a netns stack), so it maps
# to clean-dev-linux; on macOS it deletes the Lima VMs.
destroy-dev:
	@case "$$(uname -s)" in \
	  Linux)  $(MAKE) clean-dev-linux ;; \
	  Darwin) $(MAKE) destroy-dev-macos ;; \
	  *) echo "unsupported OS: $$(uname -s)"; exit 1 ;; \
	esac

# restart-agent bounces the agents WITHOUT rebuilding (used by local-dev-restart).
restart-agent:
	@case "$$(uname -s)" in \
	  Linux)  sudo $(MULTINODE_SH) restart ;; \
	  Darwin) $(MAKE) --no-print-directory restart-agent-lima ;; \
	  *) echo "unsupported OS: $$(uname -s)"; exit 1 ;; \
	esac

# seed-dev orchestrates the join-token bootstrap end-to-end:
# mints token, provisions bootstrap.env + token to agent host, starts agent,
# waits for cert-material commit, seeds the default pool; VMs are created from an image URL via
# CLI. `make local-dev-start` runs this for you; invoke standalone only to re-seed a running stand.
seed-dev: build-cli ## Run the join-token bootstrap + cluster seed (requires CP running + bootstrap-dev staged)
	@bash dev/scripts/seed-dev.sh

.PHONY: demo-manifest
demo-manifest: ## Render dev/manifests/demo-vm.yaml for the host arch (amd64/arm64)
	@bash dev/scripts/render-demo-manifest.sh

# ----- Linux (two-node netns topology) -----

MULTINODE_SH := dev/scripts/linux-multinode.sh

# Build the agent, then bring up the privileged netns topology (bridge + 2 netns
# + veth + host NAT). The agents are bootstrapped + started by seed-dev.sh.
bootstrap-dev-linux: build-agent
	@sudo $(MULTINODE_SH) up
	@echo ">> bootstrap-dev-linux done; topology up (driven by 'make local-dev-start')"

# Rebuild the agent binary and restart both agents in place (config + cert
# material persist across a restart).
deploy-dev-linux: build-agent
	@sudo $(MULTINODE_SH) restart
	@echo ">> deploy-dev-linux done"

clean-dev-linux:
	-sudo $(MULTINODE_SH) down --wipe
	@echo ">> clean-dev-linux done"

# ----- macOS (Lima) -----

# Stage Lima VM + agent binary + config. Agent NOT started — seed-dev.sh
# provisions bootstrap material and starts it.
bootstrap-dev-macos: lima-ensure copy-config-lima build-agent-lima copy-agent-lima
	@echo ">> bootstrap-dev-macos done; agent staged inside Lima '$(LIMA_VM)' (driven by 'make local-dev-start')"

deploy-dev-macos: build-agent-lima copy-agent-lima restart-agent-lima
	@echo ">> deploy-dev-macos done"

# clean-dev-macos stops the VMs but KEEPS them, so the next local-dev-start
# reuses them (no image convert / disk expand / apt re-run). destroy-dev-macos
# is the explicit full delete for when a VM is wedged or you want a from-scratch
# rebuild.
clean-dev-macos:
	-limactl stop $(LIMA_VM_1) 2>/dev/null || true
	-limactl stop $(LIMA_VM_2) 2>/dev/null || true
	-limactl stop $(LIMA_VM_3) 2>/dev/null || true
	@echo ">> clean-dev-macos done (VMs stopped, reused on next start)"

destroy-dev-macos:
	-limactl stop $(LIMA_VM_1) 2>/dev/null || true
	-limactl delete $(LIMA_VM_1) 2>/dev/null || true
	-limactl stop $(LIMA_VM_2) 2>/dev/null || true
	-limactl delete $(LIMA_VM_2) 2>/dev/null || true
	-limactl stop $(LIMA_VM_3) 2>/dev/null || true
	-limactl delete $(LIMA_VM_3) 2>/dev/null || true
	@echo ">> destroy-dev-macos done (VMs deleted)"

lima-check:
	@command -v limactl >/dev/null 2>&1 || { \
	  echo "limactl not found. Install with: brew install lima"; exit 1; \
	}

# lima-ensure brings up ALL dev VMs. Each VM's CP->agent forward maps guest 9443
# to a distinct host port (VM1=9443, VM2=9444, VM3=9445) so the CP reaches each
# agent at a distinct advertised_endpoint. lima-ensure-one is the per-VM helper,
# invoked recursively with VM + HOSTPORT so the create path can override the forward.
lima-ensure: lima-check
	@$(MAKE) --no-print-directory lima-ensure-one VM=$(LIMA_VM_1) HOSTPORT=9443
	@$(MAKE) --no-print-directory lima-ensure-one VM=$(LIMA_VM_2) HOSTPORT=9444
	@$(MAKE) --no-print-directory lima-ensure-one VM=$(LIMA_VM_3) HOSTPORT=9445

# lima-ensure-one ensures one dev VM exists, is Running, and is fully
# provisioned, with a bounded boot retry and a self-heal for a half-provisioned
# VM. The logic lives in dev/scripts/lima-ensure-vm.sh (nested-virtualization
# detection, --timeout, retry, provision-marker heal) - too much to keep legible
# as an inline Make recipe. Tunables: LIMA_BOOT_TIMEOUT, LIMA_CREATE_ATTEMPTS.
lima-ensure-one:
	@bash dev/scripts/lima-ensure-vm.sh $(VM) $(HOSTPORT)

# Cross-build agent for Lima VM's arch via the existing build-linux-{amd64,arm64}
# targets. Those targets build both daemons (api + agent); for an agent-only
# iteration the extra cost is ~one second of compile time, kept simple over
# a per-binary cross-build helper.
build-agent-lima: lima-ensure
	@arch=$$(limactl shell $(LIMA_VM) uname -m); \
	case "$$arch" in \
	  aarch64) $(MAKE) build-linux-arm64 ;; \
	  x86_64)  $(MAKE) build-linux-amd64 ;; \
	  *) echo "unsupported lima arch: $$arch"; exit 1 ;; \
	esac

copy-agent-lima: lima-ensure
	@arch=$$(limactl shell $(LIMA_VM_1) uname -m); \
	case "$$arch" in \
	  aarch64) goarch=arm64 ;; \
	  x86_64)  goarch=amd64 ;; \
	  *) echo "unsupported lima arch: $$arch"; exit 1 ;; \
	esac; \
	for vm in $(LIMA_VM_1) $(LIMA_VM_2) $(LIMA_VM_3); do \
	  echo ">> staging otherix-agent into $$vm"; \
	  limactl cp $(BIN_DIR)/linux-$$goarch/otherix-agent $$vm:/tmp/otherix-agent; \
	  limactl shell $$vm sudo mv /tmp/otherix-agent /usr/local/bin/otherix-agent; \
	  limactl shell $$vm sudo chmod +x /usr/local/bin/otherix-agent; \
	done

# Stage the agent config into both VMs. The WireGuard advertised_endpoint
# placeholder is substituted per VM with that VM's own user-v2 IP
# (192.168.104.x) so each agent advertises a peer-reachable UDP endpoint.
# The Lima VM's primary user differs from the macOS host's $USER (Lima may
# suffix .guest, reuses host UID 501), so the chown determines user/group at
# runtime via `id -un`/`id -gn` inside the VM shell.
copy-config-lima: lima-ensure
	@for vm in $(LIMA_VM_1) $(LIMA_VM_2) $(LIMA_VM_3); do \
	  wgip=$$(limactl shell $$vm -- ip -4 -o addr show 2>/dev/null | grep -oE '192\.168\.104\.[0-9]+' | head -1); \
	  if [ -z "$$wgip" ]; then echo "no user-v2 (192.168.104.x) IP on $$vm yet — is the user-v2 network up?"; exit 1; fi; \
	  echo ">> $$vm WireGuard advertised endpoint: $$wgip:51820"; \
	  sed -e "s|__WG_ADVERTISED_ENDPOINT__|$$wgip:51820|" -e "s|__MIGRATION_HOST__|$$wgip|" dev/config/agent-macos.yaml > /tmp/agent-$$vm.yaml; \
	  limactl cp /tmp/agent-$$vm.yaml $$vm:/tmp/agent.yaml; \
	  limactl shell $$vm -- sh -c 'sudo mv /tmp/agent.yaml /etc/otherix/agent.yaml && sudo chown "$$(id -un):$$(id -gn)" /etc/otherix/agent.yaml'; \
	done

restart-agent-lima: lima-ensure
	@for vm in $(LIMA_VM_1) $(LIMA_VM_2) $(LIMA_VM_3); do \
	  limactl shell $$vm sudo systemctl restart otherix-agent; \
	done
	@sleep 1
	@for vm in $(LIMA_VM_1) $(LIMA_VM_2) $(LIMA_VM_3); do \
	  echo "== $$vm =="; \
	  limactl shell $$vm sudo systemctl status otherix-agent --no-pager || true; \
	done

# ========== Docker ==========

.PHONY: docker-build docker-build-multiarch
docker-build: ## Build local images for api + agent (current arch)
	docker build -f deploy/docker/controlplane.Dockerfile \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg DATE=$(DATE) \
	  -t otherix-api:dev .
	docker build -f deploy/docker/agent.Dockerfile \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg DATE=$(DATE) \
	  -t otherix-agent:dev .

docker-build-multiarch: ## Build multi-arch (amd64+arm64) images via buildx
	docker buildx create --name otherix-builder --use 2>/dev/null || true
	docker buildx build -f deploy/docker/controlplane.Dockerfile \
	  --platform linux/amd64,linux/arm64 \
	  --build-arg VERSION=$(VERSION) \
	  -t otherix-api:dev .
	docker buildx build -f deploy/docker/agent.Dockerfile \
	  --platform linux/amd64,linux/arm64 \
	  --build-arg VERSION=$(VERSION) \
	  -t otherix-agent:dev .

# ========== OpenAPI ==========

.PHONY: api-validate api-bundle api-preview api-preview-stop api-preview-logs agent-api-generate agent-api-verify
api-validate: ## Lint every OpenAPI spec under api/openapi/
	@for f in api/openapi/*.yaml; do \
	  case "$$f" in *.bundled.yaml|*/oapi-codegen.yaml) continue ;; esac; \
	  echo ">> linting $$f"; \
	  npx --yes @redocly/cli@$(REDOCLY_VERSION) lint "$$f" || exit 1; \
	done

api-bundle: ## Bundle control-plane.yaml into a single resolved spec
	npx --yes @redocly/cli@$(REDOCLY_VERSION) bundle api/openapi/control-plane.yaml -o api/openapi/control-plane.bundled.yaml

api-preview: ## Start Swagger UI + Redoc for browsing the spec
	SWAGGER_UI_VERSION=$(SWAGGER_UI_VERSION) REDOC_VERSION=$(REDOC_VERSION) $(API_PREVIEW_COMPOSE) up -d
	@echo
	@echo "Swagger UI: http://localhost:8081"
	@echo "Redoc:      http://localhost:8082"

api-preview-stop: ## Stop the OpenAPI preview stack
	$(API_PREVIEW_COMPOSE) down

api-preview-logs: ## Follow OpenAPI preview logs
	$(API_PREVIEW_COMPOSE) logs -f

# Codegen for the agent server interface. Pipeline:
#   agent.yaml -> openapi-normalize (3.1 nullable -> 3.0) -> oapi-codegen
agent-api-generate: ## Regenerate internal/agentapi/agent.gen.go from agent.yaml
	@mkdir -p internal/agentapi && tmp=$$(mktemp) && trap "rm -f $$tmp" EXIT && \
	  $(GO) run ./tools/openapi-normalize api/openapi/agent.yaml $$tmp && \
	  $(GO) tool oapi-codegen --config api/openapi/oapi-codegen.yaml \
	    -o internal/agentapi/agent.gen.go $$tmp

agent-api-verify: ## Fail when the committed agent.gen.go diverges from a fresh regeneration
	@tmpdir=$$(mktemp -d) && trap "rm -rf $$tmpdir" EXIT && \
	  $(GO) run ./tools/openapi-normalize api/openapi/agent.yaml $$tmpdir/normalized.yaml && \
	  $(GO) tool oapi-codegen --config api/openapi/oapi-codegen.yaml \
	    -o $$tmpdir/agent.gen.go $$tmpdir/normalized.yaml && \
	  if ! diff -u internal/agentapi/agent.gen.go $$tmpdir/agent.gen.go; then \
	    echo ">> internal/agentapi/agent.gen.go is out of sync with api/openapi/agent.yaml"; \
	    echo ">> run 'make agent-api-generate' and commit the result"; \
	    exit 1; \
	  fi

# ========== Docs ==========

# User-facing documentation site (mkdocs-material -> docs.otherix.dev via the
# docs.yaml workflow). Needs the Python deps: pip install -r requirements-docs.txt.
.PHONY: docs-serve docs-build
docs-serve: ## Serve the docs site locally with live reload (http://127.0.0.1:8000)
	mkdocs serve

docs-build: ## Build the docs site into ./site (strict: fails on broken links / nav gaps)
	mkdocs build --strict

# ========== Mock-agent ==========

.PHONY: agentmock-certs
agentmock-certs: ## Regenerate mock-agent test certificates (run-on-demand; output committed)
	$(GO) run ./internal/agentmock/internal/certgen

# ========== Release ==========

GORELEASER_VERSION := v2.5.0
GORELEASER_IMAGE   := goreleaser/goreleaser:$(GORELEASER_VERSION)

# release-snapshot builds the full release locally with no publish step.
# The build MUST run on the host Go toolchain (go.mod requires Go 1.26 and the
# `tool` directive): every released goreleaser Docker image still bundles an
# older Go (v2.5.0 -> 1.23, even v2.12 -> 1.25), so the pinned image cannot
# parse go.mod. Prefer a local goreleaser binary; otherwise fall back to
# `go run` at the pinned version, which compiles against the host's Go.
.PHONY: release-snapshot
release-snapshot: ## Build a local snapshot release (.deb + archives, no publish)
	@if command -v goreleaser >/dev/null 2>&1; then \
	  echo ">> using local goreleaser ($$(goreleaser --version 2>/dev/null | head -n1))"; \
	  goreleaser release --snapshot --clean --skip=publish; \
	else \
	  echo ">> goreleaser not found locally, using go run goreleaser@$(GORELEASER_VERSION) (host Go toolchain)"; \
	  $(GO) run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) release --snapshot --clean --skip=publish; \
	fi

# ========== Harness (AWS) ==========

HARNESS_DIR := deploy/terraform/test-harness

# The harness uses the standard AWS credential chain (environment variables,
# ~/.aws profiles, SSO, or an instance role). ensure-aws-creds.sh verifies a
# usable credential is present before each run. It is sourced from the repo
# root, so it must run before any `cd`.
AWS_ENV := . dev/aws/ensure-aws-creds.sh

.PHONY: harness-spot-report harness-up harness-config harness-down harness-chaos-kill harness-chaos-partition harness-chaos-heal harness-chaos-latency

harness-spot-report: ## Survey spot price + 90-day stability + capacity availability for the harness instance types
	$(AWS_ENV) && bash dev/aws/spot-report.sh && bash dev/aws/spot-stability.sh && bash dev/aws/spot-availability.sh

harness-up: ## Bring up a named stand: make harness-up NAME=<env> [OTHERIX_VERSION=<ver>] (empty = latest release, resolved at node boot)
ifndef NAME
	$(error NAME is required, e.g. make harness-up NAME=smoke1)
endif
	$(AWS_ENV) && cd $(HARNESS_DIR) && (tofu workspace select $(NAME) 2>/dev/null || tofu workspace new $(NAME)) && tofu apply -var env_name=$(NAME) -var otherix_version=$(OTHERIX_VERSION)

harness-config: ## Point the local CLI at a stand: make harness-config NAME=<env>
ifndef NAME
	$(error NAME is required)
endif
	$(AWS_ENV) && cd $(HARNESS_DIR) && tofu workspace select $(NAME) && bash ../../../dev/aws/harness-config.sh $(NAME)

harness-down: ## Tear a stand down: make harness-down NAME=<env>
ifndef NAME
	$(error NAME is required)
endif
	$(AWS_ENV) && cd $(HARNESS_DIR) && tofu workspace select $(NAME) && tofu destroy -var env_name=$(NAME)

harness-chaos-kill: ## Permanently terminate a node: make harness-chaos-kill NAME=<env> ROLE=<cp|agent|gateway> [INDEX=n]
ifndef NAME
	$(error NAME is required, e.g. make harness-chaos-kill NAME=smoke1 ROLE=agent)
endif
ifndef ROLE
	$(error ROLE is required (cp|agent|gateway), e.g. make harness-chaos-kill NAME=smoke1 ROLE=agent)
endif
	$(AWS_ENV) && cd $(HARNESS_DIR) && tofu workspace select $(NAME) && bash ../../../dev/chaos/kill.sh $(NAME) $(ROLE) $(INDEX)

harness-chaos-partition: ## Network-isolate a node: make harness-chaos-partition NAME=<env> ROLE=<cp|agent|gateway> [INDEX=n]
ifndef NAME
	$(error NAME is required, e.g. make harness-chaos-partition NAME=smoke1 ROLE=agent)
endif
ifndef ROLE
	$(error ROLE is required (cp|agent|gateway), e.g. make harness-chaos-partition NAME=smoke1 ROLE=agent)
endif
	$(AWS_ENV) && cd $(HARNESS_DIR) && tofu workspace select $(NAME) && bash ../../../dev/chaos/partition.sh $(NAME) $(ROLE) $(INDEX)

harness-chaos-heal: ## Reverse partition/latency on a node: make harness-chaos-heal NAME=<env> ROLE=<cp|agent|gateway> [INDEX=n]
ifndef NAME
	$(error NAME is required, e.g. make harness-chaos-heal NAME=smoke1 ROLE=agent)
endif
ifndef ROLE
	$(error ROLE is required (cp|agent|gateway), e.g. make harness-chaos-heal NAME=smoke1 ROLE=agent)
endif
	$(AWS_ENV) && cd $(HARNESS_DIR) && tofu workspace select $(NAME) && bash ../../../dev/chaos/heal.sh $(NAME) $(ROLE) $(INDEX)

harness-chaos-latency: ## Add latency/loss to a node: make harness-chaos-latency NAME=<env> ROLE=<cp|agent|gateway> [INDEX=n] [DELAY=ms] [LOSS=pct]
ifndef NAME
	$(error NAME is required, e.g. make harness-chaos-latency NAME=smoke1 ROLE=agent)
endif
ifndef ROLE
	$(error ROLE is required (cp|agent|gateway), e.g. make harness-chaos-latency NAME=smoke1 ROLE=agent)
endif
	$(AWS_ENV) && cd $(HARNESS_DIR) && tofu workspace select $(NAME) && bash ../../../dev/chaos/latency.sh $(NAME) $(ROLE) $(INDEX) $(DELAY) $(LOSS)
