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

.PHONY: build $(addprefix build-,$(BINARIES)) build-cli
build: $(addprefix build-,$(BINARIES)) build-cli ## Build all binaries for current platform

$(addprefix build-,$(BINARIES)): build-%: ## Build a single daemon binary (api/agent)
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/otherix-$* ./cmd/$*

# CLI is a special case: cmd dir is `cli/` for layout consistency with the
# daemons (cmd/<short>/), but the binary is `otherix` (no component
# suffix) per the kubectl/docker/gh convention for operator CLIs.
build-cli: ## Build the otherix operator CLI to bin/otherix
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/otherix ./cmd/cli

.PHONY: build-linux-amd64 build-linux-arm64
build-linux-amd64: ## Cross-compile all daemons for linux/amd64
	@mkdir -p $(BIN_DIR)/linux-amd64
	@for b in $(BINARIES); do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
	    -o $(BIN_DIR)/linux-amd64/otherix-$$b ./cmd/$$b || exit 1; \
	done

build-linux-arm64: ## Cross-compile all daemons for linux/arm64
	@mkdir -p $(BIN_DIR)/linux-arm64
	@for b in $(BINARIES); do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
	    -o $(BIN_DIR)/linux-arm64/otherix-$$b ./cmd/$$b || exit 1; \
	done

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
ETCD_TEST_PKGS := ./internal/etcd/... ./internal/etcdstore/... ./internal/api/handlers/migrations/... ./tests/apie2e/...
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

.PHONY: smoke-networking
smoke-networking: ## Networking operator smoke: drives `otherix network`/`vm create --network` against the real Lima agent (run after local-dev-start)
	bash dev/smoke/networking/run.sh

.PHONY: smoke-wireguard-mesh
smoke-wireguard-mesh: ## Cross-agent WireGuard mesh smoke: real cross-host handshake between the two Lima nodes (run after local-dev-start)
	bash dev/smoke/wireguard-mesh/run.sh

.PHONY: smoke-overlay-vm
smoke-overlay-vm: ## Overlay VM-to-VM smoke: two real VMs cross-node ping over the overlay via CP-distributed FDB (run after local-dev-start)
	bash dev/smoke/overlay-vm/run.sh

.PHONY: smoke-manifests
smoke-manifests: ## YAML-manifest CLI smoke: `otherix create -f` / `get -o yaml` / `delete -f` against the real Lima agent (run after local-dev-start)
	bash dev/smoke/manifests/run.sh

.PHONY: smoke-vm-lifecycle
smoke-vm-lifecycle: ## VM lifecycle smoke: `otherix vm` start/stop/poweroff/reboot/pause/resume/reset on a real agent (run after local-dev-start)
	bash dev/smoke/vm-lifecycle/run.sh

.PHONY: smoke-vm-network-config
smoke-vm-network-config: ## VM network-config smoke: static guest IP via `otherix vm create --network-config` on a real agent (run after local-dev-start)
	bash dev/smoke/vm-network-config/run.sh

.PHONY: smoke-vm-create-redelivery
smoke-vm-create-redelivery: ## VM create-redelivery smoke: agent restart + vm.create redelivery does not clobber a live VM and reconciles to success (audit R2-M1/M2; run after local-dev-start)
	bash dev/smoke/vm-create-redelivery/run.sh

.PHONY: smoke-vm-migration
smoke-vm-migration: ## Offline VM migration smoke: `otherix vm migrate --offline` across two nodes (run after local-dev-start)
	bash dev/smoke/vm-migration/run.sh

.PHONY: smoke-vm-migration-live
smoke-vm-migration-live: ## Live VM migration smoke: `otherix vm migrate` (live) across two nodes, asserts console-heartbeat continuity (run after local-dev-start)
	bash dev/smoke/vm-migration-live/run.sh

.PHONY: smoke-vm-migration-live-overlay
smoke-vm-migration-live-overlay: ## Live VM migration + overlay smoke: live-migrate an overlay-attached VM, asserts cross-node overlay connectivity follows the guest at cutover (run after local-dev-start)
	bash dev/smoke/vm-migration-live-overlay/run.sh

.PHONY: smoke-vm-migration-live-bridge
smoke-vm-migration-live-bridge: ## Live VM migration + bridge smoke: live-migrate a type=bridge-attached VM, asserts L2 connectivity follows the guest at cutover via announce-self (run after local-dev-start)
	bash dev/smoke/vm-migration-live-bridge/run.sh

# smoke-all runs the stack-dependent smokes in sequence (fail-fast) against a
# stand brought up by `make local-dev-start`. smoke-ha is NOT included — it
# spins its own 3 api-server processes and does not use the dev stand; run it
# separately.
.PHONY: smoke-all
smoke-all: ## Run all stack-dependent smokes in sequence (run after local-dev-start; excludes smoke-ha)
	@for s in networking wireguard-mesh overlay-vm manifests vm-lifecycle vm-migration vm-migration-live vm-network-config; do \
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

# local-dev-* is the documented dev-stack control surface (macOS Lima 2 VMs /
# Linux netns 2 nodes). start/stop/clean/cleanrestart are the lifecycle;
# restart/deploy are the non-destructive inner loop (etcd/pki + VMs/certs
# preserved). The per-OS internals (bootstrap-dev/deploy-dev/clean-dev,
# *-linux/*-macos, lima-*) are hidden from `make help`.
.PHONY: local-dev-start local-dev-stop local-dev-clean \
        local-dev-restart local-dev-deploy local-dev-cleanrestart

local-dev-start: ## Dev stack up: api-server (embedded etcd) + agents + CLI (admin@otherix.local / correct-horse-battery-staple)
	@bash dev/scripts/local-dev-start.sh

local-dev-stop: ## Dev stack down + wipe etcd/pki + delete VMs/netns (DESTRUCTIVE)
	@bash dev/scripts/local-dev-stop.sh

local-dev-clean: ## local-dev-stop + remove .local/ and the dev CLI cluster (pristine slate)
	@bash dev/scripts/local-dev-clean.sh

local-dev-restart: ## Bounce api + agents in place, no rebuild (state preserved)
	@bash dev/scripts/restart-api-dev.sh
	@$(MAKE) --no-print-directory restart-agent

local-dev-deploy: build-api ## Rebuild + restart api + agents to pick up code changes (state preserved)
	@$(MAKE) --no-print-directory deploy-dev
	@bash dev/scripts/restart-api-dev.sh

local-dev-cleanrestart: ## local-dev-stop then local-dev-start (nuke + fresh cluster)
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
# The macOS dev stack runs TWO Lima VMs so the WireGuard underlay has a real
# cross-host mesh: otherix-dev-1 (node-1) and otherix-dev-2 (node-2). Both join
# the user-v2 network for VM-to-VM L3; VM2's CP->agent forward maps guest 9443
# to host 9444 (VM1 keeps 9443). LIMA_VM is the primary VM (node-1), used by the
# single-host operations: the netns netfabric tests and the networking smoke.
LIMA_VM_1   := otherix-dev-1
LIMA_VM_2   := otherix-dev-2
LIMA_VM     := $(LIMA_VM_1)

# bootstrap-dev / deploy-dev / clean-dev / restart-agent are internal per-OS
# dispatchers used by the local-dev-* family. They are intentionally NOT in
# `make help` (no `##`) — the documented surface is local-dev-*.
.PHONY: bootstrap-dev deploy-dev clean-dev restart-agent seed-dev \
        bootstrap-dev-linux deploy-dev-linux clean-dev-linux \
        bootstrap-dev-macos deploy-dev-macos clean-dev-macos \
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

clean-dev-macos:
	-limactl stop $(LIMA_VM_1) 2>/dev/null || true
	-limactl delete $(LIMA_VM_1) 2>/dev/null || true
	-limactl stop $(LIMA_VM_2) 2>/dev/null || true
	-limactl delete $(LIMA_VM_2) 2>/dev/null || true
	@echo ">> clean-dev-macos done"

lima-check:
	@command -v limactl >/dev/null 2>&1 || { \
	  echo "limactl not found. Install with: brew install lima"; exit 1; \
	}

# lima-ensure brings up BOTH dev VMs. VM2's CP->agent forward maps guest 9443
# to host 9444 (VM1 keeps the yaml default 9443) so the CP reaches each agent at
# a distinct advertised_endpoint. lima-ensure-one is the per-VM helper, invoked
# recursively with VM + HOSTPORT so the create path can override the forward.
lima-ensure: lima-check
	@$(MAKE) --no-print-directory lima-ensure-one VM=$(LIMA_VM_1) HOSTPORT=9443
	@$(MAKE) --no-print-directory lima-ensure-one VM=$(LIMA_VM_2) HOSTPORT=9444

# Nested virtualization (so /dev/kvm appears inside the guest and the agent's
# qemu can use KVM instead of TCG) needs Apple M3+ AND macOS 15+. Lima exposes it
# via `nestedVirtualization: true`, vz only. We inject it at create time ONLY on a
# capable host - it is deliberately NOT in dev/lima/otherix-dev.yaml because that
# option hard-fails Lima start on M1/M2 (the silicon lacks the capability).
lima-ensure-one:
	@extra=""; \
	if [ "$$(uname -s)" = Darwin ]; then \
	  gen=$$(sysctl -n machdep.cpu.brand_string 2>/dev/null | sed -nE 's/.*Apple M([0-9]+).*/\1/p'); \
	  osmaj=$$(sw_vers -productVersion 2>/dev/null | cut -d. -f1); \
	  if [ -n "$$gen" ] && [ "$$gen" -ge 3 ] && [ "$$osmaj" -ge 15 ]; then \
	    extra='--set .nestedVirtualization=true'; \
	  fi; \
	fi; \
	if ! limactl list -q 2>/dev/null | grep -q "^$(VM)$$"; then \
	  if [ -n "$$extra" ]; then \
	    echo ">> creating Lima VM $(VM) (host port $(HOSTPORT); nested virtualization on -> /dev/kvm in guest)"; \
	  else \
	    echo ">> creating Lima VM $(VM) (host port $(HOSTPORT); no nested virtualization -> agent uses TCG)"; \
	  fi; \
	  limactl start --tty=false --name=$(VM) --set ".portForwards[0].hostPort = $(HOSTPORT)" $$extra dev/lima/otherix-dev.yaml; \
	elif [ "$$(limactl list $(VM) --format '{{.Status}}' 2>/dev/null)" != "Running" ]; then \
	  echo ">> starting Lima VM $(VM)"; \
	  limactl start $(VM); \
	else \
	  echo ">> Lima VM $(VM) already running"; \
	fi

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
	for vm in $(LIMA_VM_1) $(LIMA_VM_2); do \
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
	@for vm in $(LIMA_VM_1) $(LIMA_VM_2); do \
	  wgip=$$(limactl shell $$vm -- ip -4 -o addr show 2>/dev/null | grep -oE '192\.168\.104\.[0-9]+' | head -1); \
	  if [ -z "$$wgip" ]; then echo "no user-v2 (192.168.104.x) IP on $$vm yet — is the user-v2 network up?"; exit 1; fi; \
	  echo ">> $$vm WireGuard advertised endpoint: $$wgip:51820"; \
	  sed -e "s|__WG_ADVERTISED_ENDPOINT__|$$wgip:51820|" -e "s|__MIGRATION_HOST__|$$wgip|" dev/config/agent-macos.yaml > /tmp/agent-$$vm.yaml; \
	  limactl cp /tmp/agent-$$vm.yaml $$vm:/tmp/agent.yaml; \
	  limactl shell $$vm -- sh -c 'sudo mv /tmp/agent.yaml /etc/otherix/agent.yaml && sudo chown "$$(id -un):$$(id -gn)" /etc/otherix/agent.yaml'; \
	done

restart-agent-lima: lima-ensure
	@for vm in $(LIMA_VM_1) $(LIMA_VM_2); do \
	  limactl shell $$vm sudo systemctl restart otherix-agent; \
	done
	@sleep 1
	@for vm in $(LIMA_VM_1) $(LIMA_VM_2); do \
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
