// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build !linux

package zram

import "log/slog"

// Ensure is a no-op on non-Linux builds; zram is Linux-only.
func Ensure(_ Params, _ *slog.Logger) (*Active, error) { return nil, nil }
