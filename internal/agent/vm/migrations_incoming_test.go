// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestStartIncomingReservesPortAndReturnsEndpoint(t *testing.T) {
	m := newTestManager(t)

	var createdDisk string
	var nbdArgs []string
	m.migCreateDisk = func(ctx context.Context, path string, virtualBytes int64) error {
		createdDisk = path
		return nil
	}
	m.migSpawnNBD = func(ctx context.Context, args []string) (int, error) {
		nbdArgs = args
		return 4321, nil
	}

	migID := uuid.New()
	res, err := m.StartIncoming(context.Background(), IncomingSpec{
		MigrationID:    migID,
		VMUUID:         uuid.New(),
		VMName:         "demo",
		VCPUs:          1,
		MemoryMB:       512,
		PoolName:       m.defaultTestPool(),
		Architecture:   "amd64",
		Mode:           "offline",
		ExpectedSize:   1 << 30,
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
	})
	if err != nil {
		t.Fatalf("StartIncoming() error = %v", err)
	}
	if res.ListenEndpoint == "" || res.AuthToken == "" {
		t.Errorf("StartIncoming result endpoint/token empty: %+v", res)
	}
	if createdDisk == "" {
		t.Errorf("destination disk not created")
	}
	if !argsContain(nbdArgs, "authz-simple,id=migauthz,identity=CN=node-src") {
		t.Errorf("qemu-nbd args missing source authz pin: %v", nbdArgs)
	}
	rec, ok := m.Migrations().Get(migID)
	if !ok || rec.Role != "target" || rec.Phase != "setup" {
		t.Errorf("record = %+v, ok=%v", rec, ok)
	}
}

func argsContain(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
