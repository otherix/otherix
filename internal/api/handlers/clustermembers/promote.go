// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package clustermembers

import (
	"context"
	"log/slog"
	"time"
)

// Promoter is the promotion surface the loop depends on. It is satisfied
// by *etcd.MembershipClient.
type Promoter interface {
	TryPromoteLearners(ctx context.Context) (int, error)
}

// RunPromoteLoop ticks at interval and calls TryPromoteLearners on each tick
// until ctx is cancelled. It runs on every CP replica because membership
// convergence is a cluster concern, not a job-dispatch one: each CP forwards
// the promote RPC to the current etcd leader, so concurrent loops from
// multiple replicas converge harmlessly. On a single-node cluster there are
// no learners, so each tick is a cheap membership-list read and a no-op.
func RunPromoteLoop(ctx context.Context, p Promoter, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := p.TryPromoteLearners(ctx)
			if err != nil {
				log.WarnContext(ctx, "promote learners tick failed",
					slog.String("error", err.Error()))
				continue
			}
			if n > 0 {
				log.InfoContext(ctx, "learners promoted to voter",
					slog.Int("count", n))
			}
		}
	}
}
