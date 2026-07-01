// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	apitokenshandlers "github.com/otherix/otherix/internal/api/handlers/apitokens"
	artifactpoolshandlers "github.com/otherix/otherix/internal/api/handlers/artifactpools"
	authhandlers "github.com/otherix/otherix/internal/api/handlers/auth"
	cahandlers "github.com/otherix/otherix/internal/api/handlers/ca"
	clusterhandlers "github.com/otherix/otherix/internal/api/handlers/cluster"
	clusterjoinhandlers "github.com/otherix/otherix/internal/api/handlers/clusterjoin"
	clustermembershandlers "github.com/otherix/otherix/internal/api/handlers/clustermembers"
	firmwareshandlers "github.com/otherix/otherix/internal/api/handlers/firmwares"
	heartbeathandlers "github.com/otherix/otherix/internal/api/handlers/heartbeat"
	ingressgrantshandlers "github.com/otherix/otherix/internal/api/handlers/ingressgrants"
	jointokenshandlers "github.com/otherix/otherix/internal/api/handlers/jointokens"
	migrationshandlers "github.com/otherix/otherix/internal/api/handlers/migrations"
	networkshandlers "github.com/otherix/otherix/internal/api/handlers/networks"
	nodejoinhandlers "github.com/otherix/otherix/internal/api/handlers/nodejoin"
	nodeshandlers "github.com/otherix/otherix/internal/api/handlers/nodes"
	snapshotshandlers "github.com/otherix/otherix/internal/api/handlers/snapshots"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	taskshandlers "github.com/otherix/otherix/internal/api/handlers/tasks"
	usershandlers "github.com/otherix/otherix/internal/api/handlers/users"
	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/api/health"
	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/ratelimit"
)

// DefaultMaxRequestBodyBytes is the request-body cap applied when
// RouterDeps.MaxRequestBodyBytes is zero. 1 MiB is generous for the
// REST surface: CSRs are ~1-2 KB, login bodies tiny, VM-create
// cloud-init blobs well under this.
const DefaultMaxRequestBodyBytes int64 = 1 << 20

// DefaultMaxAgentBodyBytes is the request-body cap applied to the
// agent listener when RouterDeps.MaxAgentBodyBytes is zero. The agent
// listener is mTLS-trusted and its heartbeat body scales with node
// density (per-pool image inventory + per-VM runtime states /
// reconciliation conditions), so its cap is a generous memory backstop,
// not a tight anti-DoS limit. The anonymous join/CA endpoints it shares
// (nodes.join, cluster.join, ca.get) are kilobytes and sit far under
// it. 16 MiB.
const DefaultMaxAgentBodyBytes int64 = 16 << 20

// RouterDeps bundles the dependencies the router needs. Passed as a
// struct rather than positional args because the API surface grows; new
// fields can land without breaking call sites.
type RouterDeps struct {
	Store               RouterStore
	AuthService         *auth.Service
	LoginRateLimiter    *ratelimit.FailureLimiter // per-replica failed-login throttle; nil disables throttling
	HealthCheckName     string                    // /readyz dependency label; empty falls to "database"
	StoragePools        config.StoragePoolsConfig // path allowlist
	Logger              *slog.Logger
	RequestTimeout      time.Duration
	MaxRequestBodyBytes int64 // public-router request-body cap in bytes; 0 means DefaultMaxRequestBodyBytes, negative disables the cap
	MaxAgentBodyBytes   int64 // agent-router request-body cap in bytes; 0 means DefaultMaxAgentBodyBytes, negative disables the cap
	PressureMemory      config.PressureConditionConfig
	PressureSystemDisk  config.PressureConditionConfig
	PressureDisk        config.PressureConditionConfig
	VMLifecycle         vmshandlers.LifecycleDeps // sync pause/resume/reset agentclient
	VMConsole           vmshandlers.ConsoleDeps   // console token issuance + proxy relay
	SSHCertTTL          time.Duration             // guest SSH user-cert validity; 0 falls back to vmshandlers.defaultGuestCertTTL
	ClusterMembership   ClusterMembership         // CP-mediated etcd membership seam (join + admin + promote loop)
	MaxConcurrentDrains int                       // cap on simultaneous node drains; 0 falls back to config.DefaultMaxConcurrentDrains in the nodes handler
}

// NewRouter constructs the api-server's HTTP handler: a chi router with
// the standard middleware stack, health probes, and (when an auth
// service is supplied) the /v1/* business surface.
//
// Cross-cutting middleware order is fixed:
//
//   - RequestID first so every other middleware can read the id off the
//     context.
//   - Logger second so it observes the final status, including the 500
//     written by Recoverer.
//   - Recoverer third so panics from the handler — and from the timeout
//     middleware downstream — turn into the standard error envelope.
//   - Timeout last among the cross-cutting middleware so its deadline
//     applies to the handler only, not to the response logging itself.
//
// Health endpoints live outside /v1/: Kubernetes probes are
// version-independent infrastructure and must not break when the API
// rolls forward.
func NewRouter(deps RouterDeps) http.Handler {
	// The etcd store self-enqueues (EnqueueTask writes the job row inline), so
	// there is no separate queue binder to wire here.
	r := chi.NewRouter()

	bodyLimit := deps.MaxRequestBodyBytes
	if bodyLimit == 0 {
		bodyLimit = DefaultMaxRequestBodyBytes
	}

	r.Use(middleware.RequestID)
	r.Use(middleware.Logger(deps.Logger))
	r.Use(middleware.Recoverer(deps.Logger))
	// middleware.Timeout is NOT applied at root — long-lived WebSocket
	// routes (vms.consoleStream) must opt out, because the pump
	// goroutines inherit r.Context() and would terminate at
	// deps.RequestTimeout (~30s by default). The bounded-REST subtree
	// below opts back in via a Group. The same Group carries
	// MaxBodyBytes: the streaming siblings are GET/WebSocket routes with
	// no request body and intentionally stay uncapped.

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "the requested resource was not found", nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		response.WriteError(w, r, http.StatusMethodNotAllowed,
			response.CodeMethodNotAllowed, "method not allowed for this resource", nil)
	})

	// Streaming endpoints - registered before the Timeout Group below.
	// console-stream is anonymous (token = auth credential); vms.logs
	// runs under Authn + RequirePermission(PermVMConsole), with
	// ownership enforced inside the handler. Hijacked http.Server
	// deadlines are cleared inside each handler so the long-lived
	// stream is not killed at 30 s.
	if deps.AuthService != nil && deps.Store != nil {
		streamingVMs := vmshandlers.New(deps.Store, deps.Logger,
			deps.VMLifecycle, deps.VMConsole,
			vmshandlers.SSHDeps{Verifier: deps.AuthService, CertTTL: deps.SSHCertTTL})
		r.Get("/v1/vms/{id}/console-stream", streamingVMs.ConsoleStream)
		// ssh-stream is the L4 sibling of console-stream: a long-lived WSS
		// relay that carries an end-to-end SSH session to the guest. Like
		// console-stream it is anonymous to the Authn group (it accepts an
		// ingress-grant token, read by the handler itself) and registered
		// directly here, OUTSIDE the Timeout group, so the long-lived stream
		// is not killed at the request deadline.
		r.Get("/v1/vms/{id}/ssh-stream", streamingVMs.SSHStream)
		// ssh-cert is mounted OUTSIDE the Authn group (like console-stream):
		// it must accept an ingress-grant token, which is not an Authn principal,
		// and structurally guarantee a grant token reaches nothing else. The
		// handler reads the bearer itself and dual-dispatches grant vs CLI.
		//
		// Unlike its streaming siblings it is a bodied POST, so it opts back
		// into Timeout + MaxBodyBytes via its own Group (the same bounds the
		// bounded-REST surface applies) WITHOUT taking on Authn: a caller with
		// any non-empty garbage bearer must not be able to slow-trickle or
		// balloon the request body. The handler additionally caps its own body.
		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(deps.RequestTimeout))
			r.Use(middleware.MaxBodyBytes(bodyLimit))
			r.Post("/v1/vms/{id}/ssh-cert", streamingVMs.IssueSSHCert)
		})

		streamAuthn := middleware.Authn(deps.AuthService)
		r.Group(func(r chi.Router) {
			r.Use(streamAuthn)
			r.Use(middleware.RequirePermission(auth.PermVMConsole, deps.Logger))
			r.Get("/v1/vms/{id}/logs", streamingVMs.Logs)
		})
	}

	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(deps.RequestTimeout))
		// Cap request bodies on the whole bounded-REST surface,
		// including the anonymous bodied endpoints (auth.login,
		// nodes.join) an unauthenticated client could otherwise use to
		// force unbounded buffering. A read past the cap fails inside
		// the handler's JSON decode and surfaces as 400
		// validation_failed.
		r.Use(middleware.MaxBodyBytes(bodyLimit))

		checkName := deps.HealthCheckName
		if checkName == "" {
			checkName = "database"
		}
		healthHandler := health.New(deps.Store, checkName)
		r.Get("/healthz", healthHandler.Live)
		r.Get("/readyz", healthHandler.Ready)

		if deps.AuthService != nil && deps.Store != nil {
			mountV1(r, deps)
		}
	})

	return r
}

// mountV1 wires the /v1/ business surface. The login and refresh
// endpoints sit outside the Authn + Idempotency block because
// token-issuance is excluded from idempotency replay. Every other
// route runs under Authn, then Idempotency, then per-route
// RequirePermission checks where applicable.
//
// The /v1/ca and /v1/nodes/join-tokens subtrees also live outside the
// main Authn + Idempotency block:
//   - /v1/ca is anonymous (bootstrap-time agents have no credentials).
//   - /v1/nodes/join-tokens POST mints a fresh token per call —
//     replaying a cached response would compromise the once-only
//     plaintext invariant. The remaining methods opt in to idem
//     individually.
func mountV1(r chi.Router, deps RouterDeps) {
	authH := authhandlers.New(deps.AuthService, deps.Store, deps.LoginRateLimiter)
	caH := cahandlers.New(deps.Store, deps.Logger)
	nodeJoinH := nodejoinhandlers.New(deps.Store, deps.Logger)
	usersH := usershandlers.New(deps.Store)
	tokensH := apitokenshandlers.New(deps.Store)
	nodesH := nodeshandlers.New(deps.Store, deps.Logger, deps.MaxConcurrentDrains)
	joinTokensH := jointokenshandlers.New(deps.Store, deps.Logger)
	networksH := networkshandlers.New(deps.Store, deps.Logger)
	storagePoolsH := storagepoolshandlers.New(deps.Store, deps.StoragePools, deps.Logger)
	artifactPoolsH := artifactpoolshandlers.New(deps.Store, deps.Logger)
	clusterH := clusterhandlers.New(deps.Store, deps.Logger)
	clusterMembersH := clustermembershandlers.New(deps.ClusterMembership, deps.Logger)
	firmwaresH := firmwareshandlers.New(deps.Store, deps.Logger)
	tasksH := taskshandlers.New(deps.Store, deps.Logger)
	// The authenticated /v1/vms group does not serve ssh-cert (mounted
	// outside Authn above), so it carries a zero SSHDeps.
	vmsH := vmshandlers.New(deps.Store, deps.Logger, deps.VMLifecycle, deps.VMConsole, vmshandlers.SSHDeps{})
	// The sync-lifecycle agent client (production: *agentclient.Client) also
	// satisfies the migrations cancel seam; assert to it so a CP-side cancel
	// best-effort propagates to the source + target agents. A nil / non-matching
	// client (agent router passes a zero LifecycleDeps) yields nil, which the
	// handler treats as "skip propagation" (the cancel still succeeds).
	migCancelClient, _ := deps.VMLifecycle.AgentClient.(migrationshandlers.MigrationCancelClient)
	migH := migrationshandlers.New(deps.Store, migCancelClient, deps.Logger)
	snapH := snapshotshandlers.New(deps.Store, deps.Logger)
	ingressGrantsH := ingressgrantshandlers.New(deps.Store, deps.Logger)

	authn := middleware.Authn(deps.AuthService)
	idem := middleware.Idempotency(deps.Store, deps.Logger)

	r.Route("/v1", func(r chi.Router) {
		r.Route("/auth", func(r chi.Router) {
			r.Post("/login", authH.Login)
			r.Post("/refresh", authH.Refresh)
			r.With(authn, idem).Post("/logout", authH.Logout)
		})

		// Anonymous CA fetch — agents at bootstrap time have no
		// credentials. Returns the active cluster CA cert PEM +
		// fingerprint for TOFU validation.
		r.Get("/ca", caH.Get)

		// NOTE: vms.consoleStream is mounted in NewRouter (outside the
		// Timeout Group) — anonymous, agent-issued token in query string
		// is the auth credential (single-use, 30s TTL, sha256 stored
		// on agent). Direct-mode operators bypass that handler entirely
		// (websocket_url returned by `vms.console` points straight to the
		// agent).

		// Anonymous redemption endpoint (Step 2 of the join-token
		// bootstrap landing). Token plaintext in body is the
		// bearer credential; TLS protects transport. The handler
		// orchestrates an atomic transaction that validates the token,
		// signs the CSR, upserts the node row, and records a consumption
		// audit entry — all under a SELECT FOR UPDATE row lock on the
		// join_tokens row for race-safe max_uses enforcement.
		r.Post("/nodes/join", nodeJoinH.Join)

		// NOTE: /v1/cluster/join is intentionally NOT mounted here. It returns
		// the cluster CA private key and must only be served on the TLS agent
		// listener (see NewAgentRouter), never on this plain-HTTP listener.

		// Join-token management. Mounted before the main Authn +
		// Idempotency block so POST (create) can opt out of idem
		// replay (each call mints a fresh token).
		// The remaining methods opt in to idem individually.
		r.Route("/nodes/join-tokens", func(r chi.Router) {
			r.Use(authn)
			// Create — authn yes, idem NO (intentional fresh-token-
			// per-call). Idempotency-Key headers are silently
			// ignored by virtue of the middleware not being applied.
			r.With(middleware.RequirePermission(auth.PermNodeManage, deps.Logger)).Post("/", joinTokensH.Create)
			// Read-only + delete with idem support.
			r.Group(func(r chi.Router) {
				r.Use(idem)
				r.With(middleware.RequirePermission(auth.PermNodeManage, deps.Logger)).Get("/", joinTokensH.List)
				r.With(middleware.RequirePermission(auth.PermNodeManage, deps.Logger)).Get("/{id}/consumptions", joinTokensH.ListConsumptions)
				r.With(middleware.RequirePermission(auth.PermNodeManage, deps.Logger)).Delete("/{id}", joinTokensH.Delete)
			})
		})

		r.Group(func(r chi.Router) {
			r.Use(authn)
			r.Use(idem)

			r.Route("/users", func(r chi.Router) {
				r.Get("/me", usersH.GetMe)
				r.Patch("/me", usersH.UpdateMe)

				r.Group(func(r chi.Router) {
					r.Use(middleware.RequirePermission(auth.PermAPITokenManage, deps.Logger))
					r.Post("/me/api-tokens", tokensH.CreateMe)
					r.Get("/me/api-tokens", tokensH.ListMe)
					r.Delete("/me/api-tokens/{token_id}", tokensH.DeleteMe)
					r.Post("/{id}/api-tokens", tokensH.CreateForUser)
					r.Get("/{id}/api-tokens", tokensH.ListForUser)
					r.Delete("/{id}/api-tokens/{token_id}", tokensH.DeleteForUser)
				})

				r.With(middleware.RequirePermission(auth.PermUserRead, deps.Logger)).Get("/", usersH.List)
				r.With(middleware.RequirePermission(auth.PermUserRead, deps.Logger)).Get("/{id}", usersH.Get)
				r.With(middleware.RequirePermission(auth.PermUserManage, deps.Logger)).Post("/", usersH.Create)
				r.With(middleware.RequirePermission(auth.PermUserManage, deps.Logger)).Patch("/{id}", usersH.Update)
				r.With(middleware.RequirePermission(auth.PermUserManage, deps.Logger)).Delete("/{id}", usersH.Delete)
			})

			r.Route("/nodes", func(r chi.Router) {
				r.With(middleware.RequirePermission(auth.PermNodeRead, deps.Logger)).Get("/", nodesH.List)
				r.With(middleware.RequirePermission(auth.PermNodeRead, deps.Logger)).Get("/{id}", nodesH.Get)
				r.With(middleware.RequirePermission(auth.PermNodeManage, deps.Logger)).Post("/", nodesH.Create)
				r.With(middleware.RequirePermission(auth.PermNodeMaintenance, deps.Logger)).Post("/{id}/cordon", nodesH.Cordon)
				r.With(middleware.RequirePermission(auth.PermNodeMaintenance, deps.Logger)).Post("/{id}/uncordon", nodesH.Uncordon)
				r.With(middleware.RequirePermission(auth.PermNodeMaintenance, deps.Logger)).Post("/{id}/readmit", nodesH.Readmit)
				r.With(middleware.RequirePermission(auth.PermNodeMaintenance, deps.Logger)).Post("/{id}/drain", nodesH.Drain)
				r.With(middleware.RequirePermission(auth.PermNodeManage, deps.Logger)).Delete("/{id}", nodesH.Delete)
			})

			r.Route("/firmwares", func(r chi.Router) {
				r.With(middleware.RequirePermission(auth.PermFirmwareRead, deps.Logger)).Get("/", firmwaresH.List)
				r.With(middleware.RequirePermission(auth.PermFirmwareRead, deps.Logger)).Get("/{id}", firmwaresH.Get)
				r.With(middleware.RequirePermission(auth.PermFirmwareManage, deps.Logger)).Post("/", firmwaresH.Create)
				r.With(middleware.RequirePermission(auth.PermFirmwareManage, deps.Logger)).Patch("/{id}", firmwaresH.Update)
				r.With(middleware.RequirePermission(auth.PermFirmwareManage, deps.Logger)).Delete("/{id}", firmwaresH.Delete)
			})

			r.Route("/networks", func(r chi.Router) {
				r.With(middleware.RequirePermission(auth.PermNetworkRead, deps.Logger)).Get("/", networksH.List)
				r.With(middleware.RequirePermission(auth.PermNetworkRead, deps.Logger)).Get("/{id}", networksH.Get)
				r.With(middleware.RequirePermission(auth.PermNetworkManage, deps.Logger)).Post("/", networksH.Create)
				r.With(middleware.RequirePermission(auth.PermNetworkManage, deps.Logger)).Patch("/{id}", networksH.Update)
				r.With(middleware.RequirePermission(auth.PermNetworkManage, deps.Logger)).Delete("/{id}", networksH.Delete)
			})

			// /v1/tasks surface. `tasks.list` and `tasks.get` are
			// gated by `task:read` at the role level and further
			// scope-checked inside the handlers (admin / operator →
			// any; developer / viewer → own keyed on created_by);
			// `tasks.cancel` is gated by `task:cancel`.
			r.Route("/tasks", func(r chi.Router) {
				r.With(middleware.RequirePermission(auth.PermTaskRead, deps.Logger)).Get("/", tasksH.List)
				r.With(middleware.RequirePermission(auth.PermTaskRead, deps.Logger)).Get("/{id}", tasksH.Get)
				r.With(middleware.RequirePermission(auth.PermTaskCancel, deps.Logger)).Post("/{id}/cancel", tasksH.Cancel)
			})

			// /v1/vms surface. RequirePermission gates on role-level
			// capability; ownership scope checks run inside the handler
			// bodies. Delete runs auth.CheckOwnership after the row
			// loads — cross-user developer attempts surface as 404 (no
			// leak).
			r.Route("/vms", func(r chi.Router) {
				r.With(middleware.RequirePermission(auth.PermVMRead, deps.Logger)).Get("/", vmsH.List)
				r.With(middleware.RequirePermission(auth.PermVMRead, deps.Logger)).Get("/{id}", vmsH.Get)
				r.With(middleware.RequirePermission(auth.PermVMCreate, deps.Logger)).Post("/", vmsH.Create)
				r.With(middleware.RequirePermission(auth.PermVMDelete, deps.Logger)).Delete("/{id}", vmsH.Delete)
				r.With(middleware.RequirePermission(auth.PermVMLifecycle, deps.Logger)).Post("/{id}/pause", vmsH.Pause)
				r.With(middleware.RequirePermission(auth.PermVMLifecycle, deps.Logger)).Post("/{id}/resume", vmsH.Resume)
				r.With(middleware.RequirePermission(auth.PermVMLifecycle, deps.Logger)).Post("/{id}/reset", vmsH.Reset)
				r.With(middleware.RequirePermission(auth.PermVMLifecycle, deps.Logger)).Post("/{id}/start", vmsH.Start)
				r.With(middleware.RequirePermission(auth.PermVMLifecycle, deps.Logger)).Post("/{id}/stop", vmsH.Stop)
				r.With(middleware.RequirePermission(auth.PermVMLifecycle, deps.Logger)).Post("/{id}/poweroff", vmsH.Poweroff)
				r.With(middleware.RequirePermission(auth.PermVMLifecycle, deps.Logger)).Post("/{id}/reboot", vmsH.Reboot)
				// vms.console — sync. Token issuance gated
				// by RequirePermission(vm:console); ownership scope=own
				// for developer is enforced inside the handler.
				r.With(middleware.RequirePermission(auth.PermVMConsole, deps.Logger)).Post("/{id}/console", vmsH.Console)
				// vms.ingress — sync. The L4 ingress broker: gated by
				// RequirePermission(vm:connect) at the route (viewer 403);
				// ownership scope=own for developer is enforced inside the
				// handler (cross-owner -> 404). Returns connect coordinates
				// synchronously (200) - the data-plane connect is the client's.
				r.With(middleware.RequirePermission(auth.PermVMConnect, deps.Logger)).Post("/{id}/ingress", vmsH.Ingress)
				// vms.migrate — async (202). vm:migrate gates the route
				// (admin / operator only; developer / viewer 403 at the
				// middleware); ownership scope is enforced inside the
				// handler. Migration is a VM sub-resource action (ADR
				// 0009) — created here, polled via /v1/migrations/{id}.
				r.With(middleware.RequirePermission(auth.PermVMMigrate, deps.Logger)).Post("/{id}/migrate", migH.Start)
				// vms.snapshots — snapshot is a VM sub-resource (ADR
				// 0009). List/create live under the VM; the snapshot
				// record's own GET-by-id + delete live under
				// /v1/snapshots/{id}. snapshot:read gates list,
				// snapshot:create gates create; ownership scope is
				// enforced inside the handlers (cross-owner -> 404).
				r.With(middleware.RequirePermission(auth.PermSnapshotRead, deps.Logger)).Get("/{id}/snapshots", snapH.List)
				r.With(middleware.RequirePermission(auth.PermSnapshotCreate, deps.Logger)).Post("/{id}/snapshots", snapH.Create)
			})

			// /v1/snapshots surface. The snapshot record's own
			// GET-by-id (status polling) + delete. Snapshots are never
			// CREATED through this subtree — only through POST
			// /v1/vms/{id}/snapshots.
			r.Route("/snapshots", func(r chi.Router) {
				r.With(middleware.RequirePermission(auth.PermSnapshotRead, deps.Logger)).Get("/", snapH.ListAll)
				r.With(middleware.RequirePermission(auth.PermSnapshotRead, deps.Logger)).Get("/{id}", snapH.Get)
				r.With(middleware.RequirePermission(auth.PermSnapshotDelete, deps.Logger)).Delete("/{id}", snapH.Delete)
			})

			// /v1/ingress-grants surface. A grant is a top-level resource
			// (it spans multiple VMs), gated end to end by vm:ingress-grant.
			// RequirePermission gates role-level capability (viewer ->
			// 403); ownership scope runs inside the handler bodies: read /
			// edit / revoke key on the grant's creator (cross-user ->
			// 404), create / add-vm additionally check each referenced
			// VM's owner (visible-but-unowned -> 403). The plaintext token
			// is surfaced once on create.
			r.Route("/ingress-grants", func(r chi.Router) {
				r.With(middleware.RequirePermission(auth.PermVMIngressGrant, deps.Logger)).Get("/", ingressGrantsH.List)
				r.With(middleware.RequirePermission(auth.PermVMIngressGrant, deps.Logger)).Get("/{id}", ingressGrantsH.Get)
				r.With(middleware.RequirePermission(auth.PermVMIngressGrant, deps.Logger)).Delete("/{id}", ingressGrantsH.Delete)
				r.With(middleware.RequirePermission(auth.PermVMIngressGrant, deps.Logger)).Post("/", ingressGrantsH.Create)
				r.With(middleware.RequirePermission(auth.PermVMIngressGrant, deps.Logger)).Post("/{id}/vms", ingressGrantsH.AddVM)
				r.With(middleware.RequirePermission(auth.PermVMIngressGrant, deps.Logger)).Delete("/{id}/vms/{vm_name}", ingressGrantsH.RemoveVM)
				r.With(middleware.RequirePermission(auth.PermVMIngressGrant, deps.Logger)).Post("/{id}/revoke", ingressGrantsH.Revoke)
			})

			// /v1/migrations surface. The migration record's own
			// GET-by-id + list for status polling, plus the best-effort
			// cancel. Get / List are gated by vm:read so any VM viewer can
			// poll a migration; Start (above) and Cancel are gated by
			// vm:migrate. Migrations are never CREATED through this subtree,
			// only through POST /v1/vms/{id}/migrate.
			r.Route("/migrations", func(r chi.Router) {
				r.With(middleware.RequirePermission(auth.PermVMRead, deps.Logger)).Get("/", migH.List)
				r.With(middleware.RequirePermission(auth.PermVMRead, deps.Logger)).Get("/{id}", migH.Get)
				r.With(middleware.RequirePermission(auth.PermVMMigrate, deps.Logger)).Post("/{id}/cancel", migH.Cancel)
			})

			r.Route("/storage-pools", func(r chi.Router) {
				r.With(middleware.RequirePermission(auth.PermStoragePoolRead, deps.Logger)).Get("/", storagePoolsH.List)
				r.With(middleware.RequirePermission(auth.PermStoragePoolRead, deps.Logger)).Get("/{id}", storagePoolsH.Get)
				r.With(middleware.RequirePermission(auth.PermStoragePoolManage, deps.Logger)).Post("/", storagePoolsH.Create)
				r.With(middleware.RequirePermission(auth.PermStoragePoolManage, deps.Logger)).Patch("/{id}", storagePoolsH.Update)
				r.With(middleware.RequirePermission(auth.PermStoragePoolManage, deps.Logger)).Delete("/{id}", storagePoolsH.Delete)
				// Async storage_pool.scan handler. Worker registration
				// lives in storagepools.ScanWorkers (BuildRiverClient);
				// the executor is the production agentScanExecutor.
				r.With(middleware.RequirePermission(auth.PermStoragePoolScan, deps.Logger)).Post("/{id}/scan", storagePoolsH.Scan)

				// The per-pool cached-image inventory is no longer a
				// dedicated sub-resource: it is agent-reported observed
				// state embedded under `images[]` on the pool-get
				// response (storage_pool:read). The former
				// GET/DELETE `/{pool_id}/images[/{image_id}]` endpoints
				// were removed with the template entity.
			})

			r.Route("/artifact-pools", func(r chi.Router) {
				r.With(middleware.RequirePermission(auth.PermStoragePoolRead, deps.Logger)).Get("/", artifactPoolsH.List)
				r.With(middleware.RequirePermission(auth.PermStoragePoolRead, deps.Logger)).Get("/{id}", artifactPoolsH.Get)
				r.With(middleware.RequirePermission(auth.PermStoragePoolManage, deps.Logger)).Post("/", artifactPoolsH.Create)
				r.With(middleware.RequirePermission(auth.PermStoragePoolManage, deps.Logger)).Patch("/{id}", artifactPoolsH.Patch)
				r.With(middleware.RequirePermission(auth.PermStoragePoolManage, deps.Logger)).Delete("/{id}", artifactPoolsH.Delete)
			})

			// /v1/cluster surface. Default-pool reference is the only
			// setting today; the route group anchors future
			// default-network and similar knobs to the same
			// singleton table without re-opening URL design.
			r.Route("/cluster", func(r chi.Router) {
				r.With(middleware.RequirePermission(auth.PermClusterRead, deps.Logger)).Get("/default-pool", clusterH.GetDefaultPool)
				r.With(middleware.RequirePermission(auth.PermClusterManage, deps.Logger)).Put("/default-pool", clusterH.SetDefaultPool)
				r.With(middleware.RequirePermission(auth.PermClusterManage, deps.Logger)).Delete("/default-pool", clusterH.ClearDefaultPool)

				r.With(middleware.RequirePermission(auth.PermClusterRead, deps.Logger)).Get("/default-artifact-pool", clusterH.GetDefaultArtifactPool)
				r.With(middleware.RequirePermission(auth.PermClusterManage, deps.Logger)).Put("/default-artifact-pool", clusterH.SetDefaultArtifactPool)
				r.With(middleware.RequirePermission(auth.PermClusterManage, deps.Logger)).Delete("/default-artifact-pool", clusterH.ClearDefaultArtifactPool)

				r.With(middleware.RequirePermission(auth.PermClusterRead, deps.Logger)).Get("/default-network", clusterH.GetDefaultNetwork)
				r.With(middleware.RequirePermission(auth.PermClusterManage, deps.Logger)).Put("/default-network", clusterH.SetDefaultNetwork)
				r.With(middleware.RequirePermission(auth.PermClusterManage, deps.Logger)).Delete("/default-network", clusterH.ClearDefaultNetwork)

				r.With(middleware.RequirePermission(auth.PermClusterRead, deps.Logger)).Get("/ssh-ingress", clusterH.GetSSHIngress)
				r.With(middleware.RequirePermission(auth.PermClusterManage, deps.Logger)).Put("/ssh-ingress", clusterH.SetSSHIngress)

				// etcd cluster membership admin. cluster:manage gates both
				// the inspection read and the member eviction - the routes
				// surface raw etcd topology, an operator-only concern.
				r.With(middleware.RequirePermission(auth.PermClusterManage, deps.Logger)).Get("/members", clusterMembersH.List)
				r.With(middleware.RequirePermission(auth.PermClusterManage, deps.Logger)).Delete("/members/{id}", clusterMembersH.Remove)
			})
		})
	})
}

// NewAgentRouter constructs the chi router that backs the dedicated
// agent-only listener. The middleware stack mirrors NewRouter
// (RequestID / Logger / Recoverer / Timeout) and additionally
// installs agentMTLS in front of every route — the listener itself
// is configured with RequireAndVerifyClientCert, so by the time
// agentMTLS runs the chain has already been validated; the
// middleware is responsible for the fingerprint → node-id binding
// and the `details.reason` envelope on auth failure.
//
// Authn / Idempotency / RequirePermission are NOT installed here.
// Agents are a separate caller class (auth.Agent), not RBAC
// principals; heartbeat carve-out from Idempotency-Key is part of
// the contract per api/openapi/control-plane.yaml.
func NewAgentRouter(deps RouterDeps) http.Handler {
	r := chi.NewRouter()

	bodyLimit := deps.MaxAgentBodyBytes
	if bodyLimit == 0 {
		bodyLimit = DefaultMaxAgentBodyBytes
	}

	r.Use(middleware.RequestID)
	r.Use(middleware.Logger(deps.Logger))
	r.Use(middleware.Recoverer(deps.Logger))
	r.Use(middleware.Timeout(deps.RequestTimeout))
	// The agent listener carries the anonymous bodied bootstrap
	// endpoints (nodes.join, cluster.join) named by the body-cap
	// finding AND the mTLS-trusted heartbeat. The heartbeat body scales
	// with node density (pool image inventory + per-VM runtime/conditions)
	// and can exceed 1 MiB on a dense node, so this router uses a separate,
	// generous cap (DefaultMaxAgentBodyBytes) as a memory backstop rather
	// than the public router's 1 MiB anti-DoS limit. The tiny anonymous
	// join/CA endpoints sit far under it.
	r.Use(middleware.MaxBodyBytes(bodyLimit))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "the requested resource was not found", nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		response.WriteError(w, r, http.StatusMethodNotAllowed,
			response.CodeMethodNotAllowed, "method not allowed for this resource", nil)
	})

	if deps.Store == nil {
		return r
	}

	verifier := newAgentVerifier(deps.Store)
	heartbeatH := heartbeathandlers.New(deps.Store, deps.Logger, deps.PressureMemory, deps.PressureSystemDisk)

	// Bootstrap-time endpoints are anonymous — agents have no cert
	// material yet (that is the entire point of these endpoints).
	// The listener uses tls.VerifyClientCertIfGiven so anonymous TLS
	// connections reach this routing tree; routes that require a
	// verified agent identity (heartbeat) opt in to the AgentMTLS
	// middleware below. Ordering matters only in so far as both
	// groups must be siblings — chi router resolves routes by exact
	// match before fall-through, and the URLs do not overlap.
	caH := cahandlers.New(deps.Store, deps.Logger)
	nodeJoinH := nodejoinhandlers.New(deps.Store, deps.Logger)
	clusterJoinH := clusterjoinhandlers.New(deps.Store, deps.ClusterMembership, deps.Logger)
	r.Get("/v1/ca", caH.Get)
	r.Post("/v1/nodes/join", nodeJoinH.Join)
	// Cluster-replica join lives on the TLS listener, not the plain-HTTP
	// control-plane listener: the response carries the cluster CA private key,
	// so it must never traverse a plain-HTTP socket. Anonymous like the other
	// bootstrap endpoints (token in body is the credential; the joiner TOFU-pins
	// the CA fingerprint), so it sits outside the AgentMTLS group.
	r.Post("/v1/cluster/join", clusterJoinH.Join)

	r.Group(func(r chi.Router) {
		r.Use(middleware.AgentMTLS(verifier))
		r.Post("/v1/nodes/{name}/heartbeat", heartbeatH.Receive)
	})

	return r
}
