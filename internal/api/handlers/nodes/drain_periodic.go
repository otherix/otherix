// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"context"
	"encoding/json"
	"log/slog"
)

// DrainReconcileStore is the store surface the drain backstop needs.
type DrainReconcileStore interface {
	ReconcileStuckDrain(ctx context.Context, reconciledResult []byte) (int, error)
}

// DrainReconcileFunc returns a periodic that un-wedges drain-stuck nodes: nodes
// left in draining whose drain task is missing or terminal are finalized to
// cordoned, and a wedged non-terminal drain task is finalized so it is reaped.
// A node whose drain task is still in flight is left alone.
func DrainReconcileFunc(st DrainReconcileStore, log *slog.Logger) func(context.Context) error {
	// The task result the backstop stamps on a wedged drain task it finalizes.
	// Marshalled once; the store stamps it verbatim (opaque to the store layer).
	// DrainResult is a fixed struct with no un-marshalable fields, so this cannot
	// fail; a nil result on the impossible error is a harmless empty payload.
	reconciledResult, err := json.Marshal(DrainResult{Code: drainCodeReconciled})
	if err != nil {
		reconciledResult = nil
	}
	return func(ctx context.Context) error {
		n, err := st.ReconcileStuckDrain(ctx, reconciledResult)
		if err != nil {
			return err
		}
		if n > 0 {
			log.InfoContext(ctx, "drain backstop cordoned wedged nodes", "count", n)
		}
		return nil
	}
}
