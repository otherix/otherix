// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// Shared low-level reconfiguration helpers that MembershipClient (membership.go)
// builds on: a short-lived single-member dialer, the settle-gated reconfig
// classifier, and the timing constants both ends share.
//
// etcd serializes reconfiguration and settle-gates it: its strict reconfig check
// (isConnectedFullySince) rejects a membership change with "unhealthy cluster"
// until the leader has been continuously connected to every peer for ~one
// election timeout. Right after a prior change that connection has not matured,
// so adds/removes retry until the cluster settles - the operator rule of "one
// member at a time, let it settle" encoded as a loop in MembershipClient.
const (
	reconfigSettleTimeout = 20 * time.Second
	reconfigRetryInterval = 500 * time.Millisecond

	// A freshly promoted voter applies the config change locally a moment after
	// the leader accepts it; until then it rejects reads. Poll until it serves.
	memberServingTimeout = 15 * time.Second
	memberServingPoll    = 100 * time.Millisecond

	memberDialTimeout = 5 * time.Second
)

// dialMember opens a short-lived client to a single member's client endpoint.
// The caller closes it.
func dialMember(clientURL string) (*clientv3.Client, error) {
	return clientv3.New(clientv3.Config{
		Endpoints:   []string{clientURL},
		DialTimeout: memberDialTimeout,
	})
}

// isTransientReconfigErr reports whether err is one of etcd's settling-related
// reconfiguration rejections that clears with time (as opposed to a permanent
// failure like an unknown member).
func isTransientReconfigErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "unhealthy cluster") ||
		strings.Contains(msg, "too many learner") ||
		strings.Contains(msg, "not enough started members")
}
