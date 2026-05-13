// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build test_fast_argon
// +build test_fast_argon

package auth

// init lowers argon2id cost parameters for test builds tagged with
// `test_fast_argon`. The OWASP defaults (t=2, m=19 MiB) cost ~30 ms
// per HashPassword call on commodity CI hardware; e2e suites that
// seed dozens of users per package run pay that cost cumulatively.
//
// Test builds care only about argon2id format correctness and verify
// round-trip — internal/auth/password_test.go locks the PHC structure
// independently of cost — so the cheaper parameters are sound for
// every test path. Production binaries are built without the tag and
// retain the OWASP defaults declared in password.go.
//
// `t=1, m=8 KiB` is the RFC 9106 minimum and runs in roughly 100 µs.
func init() {
	argonTime = 1
	argonMemory = 8
	argonThreads = 1
}
