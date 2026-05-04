// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package version

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestCurrentReturnsBuildValues(t *testing.T) {
	// No t.Parallel(): mutates package-level vars Version/Commit/Date.
	Version = "1.2.3"
	Commit = "abcd"
	Date = "2026-05-04"
	t.Cleanup(func() {
		Version = "dev"
		Commit = "unknown"
		Date = "unknown"
	})

	got := Current()
	want := Info{Version: "1.2.3", Commit: "abcd", Date: "2026-05-04"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Current() mismatch (-want +got):\n%s", diff)
	}
}

func TestCurrentReturnsDefaults(t *testing.T) {
	got := Current()
	want := Info{Version: "dev", Commit: "unknown", Date: "unknown"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Current() mismatch (-want +got):\n%s", diff)
	}
}
