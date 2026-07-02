// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import (
	"errors"

	"github.com/otherix/otherix/internal/store"
)

// resolveHealthCheck folds an optional healthCheckRequest onto a base
// (the defaults for create, or the stored value for update), validates the
// result, and returns the effective config. A nil req yields base unchanged.
func resolveHealthCheck(req *healthCheckRequest, base store.LoadBalancerHealthCheck) (store.LoadBalancerHealthCheck, error) {
	hc := base
	if req != nil {
		if req.Port != nil {
			hc.Port = *req.Port
		}
		if req.IntervalSeconds != nil {
			hc.IntervalSeconds = *req.IntervalSeconds
		}
		if req.TimeoutSeconds != nil {
			hc.TimeoutSeconds = *req.TimeoutSeconds
		}
		if req.HealthyThreshold != nil {
			hc.HealthyThreshold = *req.HealthyThreshold
		}
		if req.UnhealthyThreshold != nil {
			hc.UnhealthyThreshold = *req.UnhealthyThreshold
		}
	}
	if err := validateHealthCheck(hc); err != nil {
		return store.LoadBalancerHealthCheck{}, err
	}
	return hc, nil
}

func validateHealthCheck(hc store.LoadBalancerHealthCheck) error {
	switch {
	case hc.Port != 0 && (hc.Port < 1 || hc.Port > 65535):
		// Port==0 is the follow-the-traffic-port sentinel; any explicit port
		// must be a valid TCP port.
		return errors.New("health_check.port must be between 1 and 65535")
	case hc.IntervalSeconds < 1 || hc.IntervalSeconds > 300:
		return errors.New("health_check.interval_seconds must be between 1 and 300")
	case hc.TimeoutSeconds < 1 || hc.TimeoutSeconds > 60:
		return errors.New("health_check.timeout_seconds must be between 1 and 60")
	case hc.HealthyThreshold < 1 || hc.HealthyThreshold > 10:
		return errors.New("health_check.healthy_threshold must be between 1 and 10")
	case hc.UnhealthyThreshold < 1 || hc.UnhealthyThreshold > 10:
		return errors.New("health_check.unhealthy_threshold must be between 1 and 10")
	}
	return nil
}
