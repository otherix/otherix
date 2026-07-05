// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package worker

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
)

// guardPanic runs fn and converts a panic into an error, logging the panic with
// its stack at ERROR. Without it, a panic in any registered job handler or
// periodic func unwinds the worker goroutine and crashes the whole api-server
// process - and, since etcd is embedded in-process, the etcd member with it. On
// a single-node deployment that is a permanent crash loop: the job is redelivered
// after the lease reclaim (which does not consume the attempt budget), panics
// again, and the process dies again. Converting the panic to an error routes it
// through the caller's normal retry/fail path, so the poison job consumes its
// attempt budget and terminally fails instead of wedging the control plane.
func guardPanic(ctx context.Context, log *slog.Logger, name string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.ErrorContext(ctx, "worker: recovered panic",
				"name", name, "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			err = fmt.Errorf("panic recovered: %v", r)
		}
	}()
	return fn()
}
