// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build !linux

package zram

// Observe returns nil on non-Linux builds; zram is a Linux-only feature.
func Observe() *Active { return nil }
