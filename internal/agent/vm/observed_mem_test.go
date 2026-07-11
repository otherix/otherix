// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import "testing"

func TestClampObservedMemUsed(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	tests := []struct {
		name    string
		used    *int64
		allocMi int
		want    *int64 // nil means "dropped / no observation"
	}{
		{name: "nil passes through", used: nil, allocMi: 2048, want: nil},
		{name: "unknown allocation disables clamp", used: i(9_000_000), allocMi: 0, want: i(9_000_000)},
		{name: "within allocation kept", used: i(1500), allocMi: 2048, want: i(1500)},
		{name: "equal to allocation kept", used: i(2048), allocMi: 2048, want: i(2048)},
		{name: "above allocation dropped (lying guest)", used: i(9_000_000_000), allocMi: 2048, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clampObservedMemUsed(tt.used, tt.allocMi)
			switch {
			case got == nil && tt.want == nil:
			case got == nil || tt.want == nil:
				t.Errorf("clampObservedMemUsed() = %v, want %v", got, tt.want)
			case *got != *tt.want:
				t.Errorf("clampObservedMemUsed() = %d, want %d", *got, *tt.want)
			}
		})
	}
}
