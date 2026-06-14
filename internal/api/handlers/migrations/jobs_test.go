// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
)

func TestMigrationRunArgs_Kind(t *testing.T) {
	if got, want := (MigrationRunArgs{}).Kind(), "vm.migrate"; got != want {
		t.Errorf("MigrationRunArgs{}.Kind() = %q, want %q", got, want)
	}
}

func TestMigrationRunArgs_JSONRoundTrip(t *testing.T) {
	in := MigrationRunArgs{
		TaskID:      uuid.New(),
		MigrationID: uuid.New(),
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("json.Marshal(%v) = %v, want nil", in, err)
	}

	var out MigrationRunArgs
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("json.Unmarshal(%s) = %v, want nil", data, err)
	}

	if diff := cmp.Diff(in, out); diff != "" {
		t.Errorf("MigrationRunArgs round-trip mismatch (-want +got):\n%s", diff)
	}
}
