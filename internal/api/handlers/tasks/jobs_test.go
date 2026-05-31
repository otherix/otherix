// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package tasks

import (
	"testing"
	"time"
)

func TestRetentionConfig_WithDefaults(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   RetentionConfig
		want RetentionConfig
	}{
		{
			name: "all zero - both defaulted",
			in:   RetentionConfig{},
			want: RetentionConfig{Completed: defaultCompletedRetention, Failed: defaultFailedRetention},
		},
		{
			name: "completed set, failed zero - failed defaulted",
			in:   RetentionConfig{Completed: 1 * time.Hour},
			want: RetentionConfig{Completed: 1 * time.Hour, Failed: defaultFailedRetention},
		},
		{
			name: "both set - pass through",
			in:   RetentionConfig{Completed: 3 * time.Minute, Failed: 4 * time.Hour},
			want: RetentionConfig{Completed: 3 * time.Minute, Failed: 4 * time.Hour},
		},
		{
			name: "negative treated as zero",
			in:   RetentionConfig{Completed: -1 * time.Second},
			want: RetentionConfig{Completed: defaultCompletedRetention, Failed: defaultFailedRetention},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.withDefaults(); got != tc.want {
				t.Errorf("withDefaults() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
