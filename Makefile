# ========== Variables ==========

BINARIES        := api agent
BIN_DIR         := bin
GO              := go
GOLANGCI_VERSION := v2.12.1
GOLANGCI_IMAGE   := golangci/golangci-lint:$(GOLANGCI_VERSION)

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
# four daemons (cmd/<short>/), but the binary is `otherix` (no component
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

# ========== Test ==========

# Build tag list shared across test targets. `test_fast_argon` swaps
# the OWASP argon2 cost parameters for cheap ones (RFC 9106 minimum)
# inside the test binary — see internal/auth/argon_fast.go. Production
# builds never pass this tag and keep the OWASP defaults.
TEST_TAGS := test_fast_argon
INTEGRATION_TAGS := integration,$(TEST_TAGS)

.PHONY: test test-short test-etcd test-netfabric test-netfabric-native coverage
test: ## Run unit tests with race detector and coverage
	$(GO) test ./... -race -tags=$(TEST_TAGS) -coverprofile=coverage.out

test-short: ## Run unit tests in short mode
	$(GO) test ./... -short -tags=$(TEST_TAGS)

# test-etcd runs the etcd-backed suites: the store layer (internal/etcdstore)
# and the api-server e2e (tests/apie2e). Both embed etcd in-process, so they
# need NO Docker - this is the integration test path after the pgx cutover.
test-etcd: ## Run etcd-backed store + api e2e suites (no Docker)
	$(GO) test -tags=$(INTEGRATION_TAGS) -count=1 -race \
	  ./internal/etcdstore/... \
	  ./tests/apie2e/...

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

.PHONY: smoke-overlay
smoke-overlay: ## Overlay (VXLAN) smoke: VTEP+bridge attrs on both nodes + manual-FDB cross-node datapath (run after local-dev-start)
	bash dev/smoke/overlay/run.sh

.PHONY: smoke-overlay-vm
smoke-overlay-vm: ## Overlay VM-to-VM smoke: two real VMs cross-node ping over the overlay via CP-distributed FDB (run after local-dev-start)
	bash dev/smoke/overlay-vm/run.sh

.PHONY: smoke-manifests
smoke-manifests: ## YAML-manifest CLI smoke: `otherix create -f` / `get -o yaml` / `delete -f` against the real Lima agent (run after local-dev-start)
	bash dev/smoke/manifests/run.sh

# ========== Lint ==========

.PHONY: lint fmt vet
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
	$(GO) run mvdan.cc/gofumpt@v0.7.0 -l -w .
	$(GO) run golang.org/x/tools/cmd/goimports@latest -local github.com/otherix/otherix -l -w .

vet: ## go vet all packages
	$(GO) vet ./...

.PHONY: vuln
vuln: ## Scan for known vulnerabilities via govulncheck (pinned)
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.3.0 ./...

# ========== Modules ==========

.PHONY: tidy download
tidy: ## go mod tidy
	$(GO) mod tidy

download: ## go mod download
	$(GO) mod download

# ========== Run (local dev) ==========

.PHONY: $(addprefix run-,$(BINARIES))
$(addprefix run-,$(BINARIES)): run-%: build-% ## Build and run a binary against deploy/config/<binary>.example.yaml
	./$(BIN_DIR)/otherix-$* --config deploy/config/$*.example.yaml

.PHONY: run-api-dev
run-api-dev: build-api ## Run the api-server with the dev config (embedded etcd, no Postgres)
	./$(BIN_DIR)/otherix-api --config dev/config/api.yaml

.PHONY: restart-api-dev
restart-api-dev: build-api ## Rebuild + restart the dev api-server in background (preserves embedded-etcd state)
	@bash dev/scripts/restart-api-dev.sh

# ========== Dev environment ==========

# etcd-reset wipes the dev member's gitignored data dir AND the dev PKI for a
# clean-slate smoke run. The api-server recreates the data dir, regenerates the
# on-disk cluster CA + peer cert, and bootstraps the admin on next boot. Wiping
# both in lockstep avoids a disk-CA/etcd-CA divergence (the on-disk CA is the
# source of truth synced into etcd at boot). Paths mirror dev/config/api.yaml.
.PHONY: etcd-reset
etcd-reset: ## Wipe the dev embedded-etcd data dir + PKI for a clean-slate smoke run
	rm -rf .local/etcd .local/pki

# ========== Dev environment (agent) ==========

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

.PHONY: bootstrap-dev deploy-dev clean-dev seed-mvp \
        bootstrap-dev-linux deploy-dev-linux clean-dev-linux \
        bootstrap-dev-macos deploy-dev-macos clean-dev-macos \
        install-agent-systemd-user \
        lima-check lima-ensure lima-ensure-one \
        build-agent-lima copy-agent-lima copy-config-lima \
        restart-agent-lima

bootstrap-dev: ## Stage dev environment (per OS — agent NOT started; finalise with 'make seed-mvp')
	@case "$$(uname -s)" in \
	  Linux)  $(MAKE) bootstrap-dev-linux ;; \
	  Darwin) $(MAKE) bootstrap-dev-macos ;; \
	  *) echo "unsupported OS: $$(uname -s)"; exit 1 ;; \
	esac

deploy-dev: ## Rebuild + (re)deploy agent (per OS)
	@case "$$(uname -s)" in \
	  Linux)  $(MAKE) deploy-dev-linux ;; \
	  Darwin) $(MAKE) deploy-dev-macos ;; \
	  *) echo "unsupported OS: $$(uname -s)"; exit 1 ;; \
	esac

clean-dev: ## Tear down dev environment (per OS)
	@case "$$(uname -s)" in \
	  Linux)  $(MAKE) clean-dev-linux ;; \
	  Darwin) $(MAKE) clean-dev-macos ;; \
	  *) echo "unsupported OS: $$(uname -s)"; exit 1 ;; \
	esac

# seed-mvp orchestrates the join-token bootstrap end-to-end:
# mints token, provisions bootstrap.env + token to agent host, starts agent,
# waits for cert-material commit, seeds the default pool; VMs are created from an image URL via
# CLI. Run AFTER `make bootstrap-dev` + `make run-api-dev`.
seed-mvp: build-cli ## Run the join-token bootstrap + MVP seed (requires CP running + bootstrap-dev staged)
	@bash dev/scripts/seed-mvp.sh

# local-dev-start / local-dev-stop wrap the full dev stack lifecycle (api-server
# with embedded etcd + Lima VM + agent + CLI cluster config) into two commands.
# After `make local-dev-start`, `./bin/otherix` works against a fresh cluster
# with no further setup. `make local-dev-stop` wipes everything including the
# embedded-etcd data dir — pair these two when you need a clean slate.
.PHONY: local-dev-start local-dev-stop
local-dev-start: ## One-shot bring-up: api-server (embedded etcd) + Lima + agent + CLI (admin@otherix.local / correct-horse-battery-staple by default)
	@bash dev/scripts/local-dev-start.sh

local-dev-stop: ## Stop everything + etcd-reset (DESTRUCTIVE - wipes the embedded-etcd data dir)
	@bash dev/scripts/local-dev-stop.sh

# ----- Linux -----

# Stage user-mode systemd unit + binary. seed-mvp.sh starts the unit
# after provisioning bootstrap material (token + bootstrap.env);
# auto-start would race with the provisioning.
bootstrap-dev-linux: build-agent install-agent-systemd-user
	@echo ">> bootstrap-dev-linux done; agent staged. Finalise with 'make seed-mvp' after 'make run-api-dev'"

deploy-dev-linux: build-agent
	@cp $(BIN_DIR)/otherix-agent $(HOME)/.local/bin/otherix-agent
	@systemctl --user restart otherix-agent
	@sleep 1
	@systemctl --user status otherix-agent --no-pager || true

install-agent-systemd-user:
	@mkdir -p $(HOME)/.local/bin
	@mkdir -p $(HOME)/.config/otherix/certs
	@mkdir -p $(HOME)/.config/otherix/vms
	@mkdir -p $(HOME)/.config/otherix/pools/default/images
	@mkdir -p $(HOME)/.config/otherix/pools/default/vms
	@mkdir -p $(HOME)/.config/systemd/user
	@chmod 0750 $(HOME)/.config/otherix/certs
	@cp $(BIN_DIR)/otherix-agent $(HOME)/.local/bin/otherix-agent
	@sed "s|__OTHERIX_CONFIG__|$(HOME)/.config/otherix|g" dev/config/agent-linux.yaml \
	    > $(HOME)/.config/otherix/agent.yaml
	@cp dev/systemd/otherix-agent.service $(HOME)/.config/systemd/user/otherix-agent.service
	@systemctl --user daemon-reload
	@echo ">> agent user systemd unit installed at $(HOME)/.config/systemd/user/otherix-agent.service"

clean-dev-linux:
	-systemctl --user stop otherix-agent 2>/dev/null || true
	-systemctl --user disable otherix-agent 2>/dev/null || true
	-rm -f $(HOME)/.config/systemd/user/otherix-agent.service
	-systemctl --user daemon-reload 2>/dev/null || true
	-rm -f $(HOME)/.local/bin/otherix-agent
	-rm -rf $(HOME)/.config/otherix
	@echo ">> clean-dev-linux done"

# ----- macOS (Lima) -----

# Stage Lima VM + agent binary + config. Agent NOT started — seed-mvp.sh
# provisions bootstrap material and starts it.
bootstrap-dev-macos: lima-ensure copy-config-lima build-agent-lima copy-agent-lima
	@echo ">> bootstrap-dev-macos done; agent staged inside Lima '$(LIMA_VM)'. Finalise with 'make seed-mvp' after 'make run-api-dev'"

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

lima-ensure-one:
	@if ! limactl list -q 2>/dev/null | grep -q "^$(VM)$$"; then \
	  echo ">> creating + starting Lima VM $(VM) (cp->agent host port $(HOSTPORT))"; \
	  limactl start --tty=false --name=$(VM) --set ".portForwards[0].hostPort = $(HOSTPORT)" dev/lima/otherix-dev.yaml; \
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
	  sed "s|__WG_ADVERTISED_ENDPOINT__|$$wgip:51820|" dev/config/agent-macos.yaml > /tmp/agent-$$vm.yaml; \
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

# ========== Mock-agent ==========

.PHONY: agentmock-certs
agentmock-certs: ## Regenerate mock-agent test certificates (run-on-demand; output committed)
	$(GO) run ./internal/agentmock/internal/certgen
