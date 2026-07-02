// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import (
	"context"

	"github.com/otherix/otherix/internal/store"
)

// EligibleBackends exposes the unexported eligibleBackends to the external
// loadbalancers_test package so the health-subtraction rule can be asserted
// deterministically without driving the full Connect handler.
func (h *Handler) EligibleBackends(ctx context.Context, lb store.LoadBalancer) ([]store.VM, error) {
	return h.eligibleBackends(ctx, lb)
}
