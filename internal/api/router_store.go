// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"context"

	apitokenshandlers "github.com/otherix/otherix/internal/api/handlers/apitokens"
	authhandlers "github.com/otherix/otherix/internal/api/handlers/auth"
	cahandlers "github.com/otherix/otherix/internal/api/handlers/ca"
	clusterhandlers "github.com/otherix/otherix/internal/api/handlers/cluster"
	clusterjoinhandlers "github.com/otherix/otherix/internal/api/handlers/clusterjoin"
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
	"github.com/otherix/otherix/internal/store"
)

// AgentCertLooker is the fingerprint -> agent-cert lookup the agent-mTLS verifier
// adapter wraps. *etcdstore.Store exposes it directly.
type AgentCertLooker interface {
	AgentCertByFingerprint(ctx context.Context, fingerprint []byte) (store.AgentCert, error)
}

// RouterStore is the storage surface the api-server router depends on: the union
// of every handler's Store contract plus the idempotency middleware store, the
// readiness pinger, and the agent-cert lookup. *etcdstore.Store satisfies it.
// Depending on the interface narrows the router's storage dependency to the
// methods it uses and lets tests substitute a fake.
type RouterStore interface {
	authhandlers.Store
	cahandlers.Store
	nodejoinhandlers.Store
	clusterjoinhandlers.Store
	usershandlers.Store
	apitokenshandlers.Store
	nodeshandlers.Store
	jointokenshandlers.Store
	networkshandlers.Store
	storagepoolshandlers.Store
	clusterhandlers.Store
	firmwareshandlers.Store
	templateshandlers.Store
	taskshandlers.Store
	vmshandlers.Store
	heartbeathandlers.Store
	middleware.IdempotencyStore
	health.Pinger
	AgentCertLooker
}

// Ensure the production SQL store satisfies the router contract. The etcd store
// is asserted in the etcdstore integration tests.
