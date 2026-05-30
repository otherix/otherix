// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	apitokenshandlers "github.com/otherix/otherix/internal/api/handlers/apitokens"
	authhandlers "github.com/otherix/otherix/internal/api/handlers/auth"
	cahandlers "github.com/otherix/otherix/internal/api/handlers/ca"
	clusterhandlers "github.com/otherix/otherix/internal/api/handlers/cluster"
	firmwareshandlers "github.com/otherix/otherix/internal/api/handlers/firmwares"
	heartbeathandlers "github.com/otherix/otherix/internal/api/handlers/heartbeat"
	jointokenshandlers "github.com/otherix/otherix/internal/api/handlers/jointokens"
	networkshandlers "github.com/otherix/otherix/internal/api/handlers/networks"
	nodejoinhandlers "github.com/otherix/otherix/internal/api/handlers/nodejoin"
	nodeshandlers "github.com/otherix/otherix/internal/api/handlers/nodes"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	taskshandlers "github.com/otherix/otherix/internal/api/handlers/tasks"
	templateshandlers "github.com/otherix/otherix/internal/api/handlers/templates"
	usershandlers "github.com/otherix/otherix/internal/api/handlers/users"
	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/api/health"
	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/queue/riverqueue"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// RouterDeps bundles the dependencies the router needs. Passed as a
// struct rather than positional args because the API surface grows; new
// fields can land without breaking call sites.
type RouterDeps struct {
	Store              *store.Store
	AuthService        *auth.Service
	RiverClient        *river.Client[pgx.Tx]
	ImageDeleter       storagepoolshandlers.ImageDeleter // may be nil when AgentClient.Enabled=false
	StoragePools       config.StoragePoolsConfig         // path allowlist
	Logger             *slog.Logger
	RequestTimeout     time.Duration
	PlacementAlgorithm string                    // empty falls to scheduler default
	PlacementResources scheduler.ResourcesConfig // zero-value disables every resource → count-based fallback
	PressureMemory     config.PressureConditionConfig
	PressureSystemDisk config.PressureConditionConfig
	PressureDisk       config.PressureConditionConfig
	VMLifecycle        vmshandlers.LifecycleDeps // sync pause/resume/reset agentclient
	VMConsole          vmshandlers.ConsoleDeps   // console token issuance + proxy relay
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
	// Wire the queue backend into the store so store.InTxEnqueue can
	// enqueue jobs atomically with their task rows. NewRouter is the
	// common choke point for production (via NewServer) and the e2e
	// harnesses, both of which supply the river client here.
	if deps.Store != nil && deps.RiverClient != nil {
		deps.Store.SetQueueBinder(riverqueue.New(deps.RiverClient))
	}

	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.Logger(deps.Logger))
	r.Use(middleware.Recoverer(deps.Logger))
	// middleware.Timeout is NOT applied at root — long-lived WebSocket
	// routes (vms.consoleStream) must opt out, because the pump
	// goroutines inherit r.Context() and would terminate at
	// deps.RequestTimeout (~30s by default). The bounded-REST subtree
	// below opts back in via a Group.

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
		streamingVMs := vmshandlers.New(deps.Store, deps.RiverClient, deps.Logger,
			deps.PlacementAlgorithm, deps.PlacementResources,
			deps.VMLifecycle, deps.VMConsole)
		r.Get("/v1/vms/{id}/console-stream", streamingVMs.ConsoleStream)

		streamAuthn := middleware.Authn(deps.AuthService)
		r.Group(func(r chi.Router) {
			r.Use(streamAuthn)
			r.Use(middleware.RequirePermission(auth.PermVMConsole, deps.Logger))
			r.Get("/v1/vms/{id}/logs", streamingVMs.Logs)
		})
	}

	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(deps.RequestTimeout))

		healthHandler := health.New(deps.Store)
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
	authH := authhandlers.New(deps.AuthService, deps.Store)
	caH := cahandlers.New(deps.Store, deps.Logger)
	nodeJoinH := nodejoinhandlers.New(deps.Store, deps.Logger)
	usersH := usershandlers.New(deps.Store)
	tokensH := apitokenshandlers.New(deps.Store)
	nodesH := nodeshandlers.New(deps.Store, deps.Logger)
	joinTokensH := jointokenshandlers.New(deps.Store, deps.Logger)
	networksH := networkshandlers.New(deps.Store, deps.Logger)
	storagePoolsH := storagepoolshandlers.New(deps.Store, deps.ImageDeleter, deps.StoragePools, deps.Logger)
	clusterH := clusterhandlers.New(deps.Store, deps.Logger)
	firmwaresH := firmwareshandlers.New(deps.Store, deps.Logger)
	templatesH := templateshandlers.New(deps.Store, deps.Logger)
	tasksH := taskshandlers.New(deps.Store, deps.Logger)
	vmsH := vmshandlers.New(deps.Store, deps.RiverClient, deps.Logger, deps.PlacementAlgorithm, deps.PlacementResources, deps.VMLifecycle, deps.VMConsole)

	authn := middleware.Authn(deps.AuthService)
	idem := middleware.Idempotency(deps.Store.Queries(), deps.Logger)

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
				r.With(middleware.RequirePermission(auth.PermNodeManage, deps.Logger)).Delete("/{id}", nodesH.Delete)
				r.With(middleware.RequirePermission(auth.PermFirmwareRead, deps.Logger)).Get("/{id}/firmwares", firmwaresH.ListByNode)
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

			r.Route("/templates", func(r chi.Router) {
				r.With(middleware.RequirePermission(auth.PermTemplateReadPublic, deps.Logger)).Get("/", templatesH.List)
				r.With(middleware.RequirePermission(auth.PermTemplateReadPublic, deps.Logger)).Get("/{id}", templatesH.Get)
				r.With(middleware.RequirePermission(auth.PermTemplateCreate, deps.Logger)).Post("/", templatesH.Create)
				r.With(middleware.RequirePermission(auth.PermTemplateUpdate, deps.Logger)).Patch("/{id}", templatesH.Update)
				r.With(middleware.RequirePermission(auth.PermTemplateDelete, deps.Logger)).Delete("/{id}", templatesH.Delete)
				r.With(middleware.RequirePermission(auth.PermTemplateSetVisibility, deps.Logger)).Post("/{id}/set-visibility", templatesH.SetVisibility)
				r.With(middleware.RequirePermission(auth.PermTemplateCreate, deps.Logger)).Post("/{id}/clone", templatesH.Clone)
				// storage_image.import async handler.
				// RequirePermission gates on role-level capability;
				// composite ownership / public-bypass enforcement lives
				// inside the handler.
				r.With(middleware.RequirePermission(auth.PermStorageImageImport, deps.Logger)).Post("/{id}/images", templatesH.ImportImage)
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
			// capability; the create handler runs the composite
			// template-usability check inside the body (matching the
			// storage_image:import pattern). Delete runs
			// auth.CheckOwnership after the row loads — cross-user
			// developer attempts surface as 404 (no leak).
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

				// storage_images read endpoints. image_cache:read is
				// held by every authenticated role, so the gate is the
				// only authorization layer; storage images are an
				// infrastructure projection without per-owner scope.
				r.With(middleware.RequirePermission(auth.PermImageCacheRead, deps.Logger)).Get("/{pool_id}/images", storagePoolsH.ListImages)
				r.With(middleware.RequirePermission(auth.PermImageCacheRead, deps.Logger)).Get("/{pool_id}/images/{image_id}", storagePoolsH.GetImage)

				// storage_image.delete sync handler.
				// RequirePermission gates on role-level capability;
				// composite ownership / public-bypass enforcement
				// happens inside the handler.
				r.With(middleware.RequirePermission(auth.PermStorageImageManage, deps.Logger)).Delete("/{pool_id}/images/{image_id}", storagePoolsH.DeleteImage)
			})

			// /v1/cluster surface. Default-pool reference is the only
			// setting today; the route group anchors the future
			// default-template / default-network knobs to the same
			// singleton table without re-opening URL design.
			r.Route("/cluster", func(r chi.Router) {
				r.With(middleware.RequirePermission(auth.PermClusterRead, deps.Logger)).Get("/default-pool", clusterH.GetDefaultPool)
				r.With(middleware.RequirePermission(auth.PermClusterManage, deps.Logger)).Put("/default-pool", clusterH.SetDefaultPool)
				r.With(middleware.RequirePermission(auth.PermClusterManage, deps.Logger)).Delete("/default-pool", clusterH.ClearDefaultPool)
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

	r.Use(middleware.RequestID)
	r.Use(middleware.Logger(deps.Logger))
	r.Use(middleware.Recoverer(deps.Logger))
	r.Use(middleware.Timeout(deps.RequestTimeout))

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

	verifier := newAgentVerifier(deps.Store.Queries())
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
	r.Get("/v1/ca", caH.Get)
	r.Post("/v1/nodes/join", nodeJoinH.Join)

	r.Group(func(r chi.Router) {
		r.Use(middleware.AgentMTLS(verifier))
		r.Post("/v1/nodes/{name}/heartbeat", heartbeatH.Receive)
	})

	return r
}
