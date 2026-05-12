// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package resolver

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/store"
)

// stubQuerier is a hand-rolled fake implementing the resolver's
// Querier interface. Each `*ByName` / pool `ByID` slot can be
// programmed per test case to return a row, pgx.ErrNoRows, or an
// arbitrary error; the corresponding `called` flag lets a test assert
// that the branch it expected to fire actually fired (asserting on
// side effects, not just the return value, catches "looked up by name
// when it should have been by id" regressions cheaply).
//
// Template / Node / VM resolve name-only; GetTemplate / GetNodeByID /
// GetVMByID are not part of the Querier surface. Pool retains both
// branches for the multi-instance carve-out.
type stubQuerier struct {
	template       store.Template
	templateErr    error
	templateByName bool

	pool                 store.StoragePool
	poolList             []store.StoragePool
	poolErr              error
	poolByID, poolByName bool

	clusterSettings    store.ClusterSetting
	clusterSettingsErr error

	node       store.Node
	nodeErr    error
	nodeByName bool

	vm       store.Vm
	vmErr    error
	vmByName bool
}

func (s *stubQuerier) GetTemplateByName(_ context.Context, _ string) (store.Template, error) {
	s.templateByName = true
	return s.template, s.templateErr
}

func (s *stubQuerier) GetStoragePoolByID(_ context.Context, _ uuid.UUID) (store.StoragePool, error) {
	s.poolByID = true
	return s.pool, s.poolErr
}

func (s *stubQuerier) ListStoragePoolsByName(_ context.Context, _ string) ([]store.StoragePool, error) {
	s.poolByName = true
	if s.poolErr != nil {
		return nil, s.poolErr
	}
	return s.poolList, nil
}

func (s *stubQuerier) GetClusterSettings(_ context.Context) (store.ClusterSetting, error) {
	return s.clusterSettings, s.clusterSettingsErr
}

func (s *stubQuerier) GetNodeByName(_ context.Context, _ string) (store.Node, error) {
	s.nodeByName = true
	return s.node, s.nodeErr
}

func (s *stubQuerier) GetVMByName(_ context.Context, _ string) (store.Vm, error) {
	s.vmByName = true
	return s.vm, s.vmErr
}

// resolverCase drives every Resolve* test. The kind field switches
// which underlying function runs; lookupErr is the value the stub
// query returns. We assert on (route taken, error envelope) only —
// the row contents themselves are stub-supplied and uninteresting.
//
// poolListLen overrides the default name-path stub for KindPool: 0
// rows simulates a missing name, 2+ rows simulates the multi-instance
// ambiguity case. The default (zero value) materialises to one row
// for happy-path lookups.
type resolverCase struct {
	name        string
	kind        Kind
	identifier  string
	lookupErr   error
	poolListLen int
	wantByID    bool
	wantByName  bool
	wantCode    ErrorCode // empty means success expected
}

func TestResolve(t *testing.T) {
	someErr := errors.New("connection reset")
	cases := []resolverCase{
		// Template - name-only. UUID literal rejected before DB.
		{
			name: "template uuid rejected", kind: KindTemplate,
			identifier: "01928cf4-1f00-7c80-8b00-000000000001",
			wantCode:   CodeUUIDInName,
		},
		{
			name: "template name hit", kind: KindTemplate,
			identifier: "ubuntu-jammy",
			wantByName: true,
		},
		{
			name: "template name missing", kind: KindTemplate,
			identifier: "no-such-template",
			lookupErr:  pgx.ErrNoRows,
			wantByName: true, wantCode: CodeNotFound,
		},
		{
			name: "template internal error", kind: KindTemplate,
			identifier: "ubuntu-jammy",
			lookupErr:  someErr,
			wantByName: true, wantCode: CodeInternal,
		},

		// Pool - multi-instance carve-out: stays polymorphic.
		{
			name: "pool uuid hit", kind: KindPool,
			identifier: "01928cf4-1f00-7c80-8b00-000000000003",
			wantByID:   true,
		},
		{
			name: "pool name hit", kind: KindPool,
			identifier:  "default",
			poolListLen: 1,
			wantByName:  true,
		},
		{
			name: "pool name missing", kind: KindPool,
			identifier:  "no-such-pool",
			poolListLen: 0,
			wantByName:  true, wantCode: CodeNotFound,
		},
		{
			name: "pool name ambiguous", kind: KindPool,
			identifier:  "default",
			poolListLen: 2,
			wantByName:  true, wantCode: CodeAmbiguous,
		},

		// Node - name-only.
		{
			name: "node uuid rejected", kind: KindNode,
			identifier: "01928cf4-1f00-7c80-8b00-000000000004",
			wantCode:   CodeUUIDInName,
		},
		{
			name: "node name hit", kind: KindNode,
			identifier: "worker-1",
			wantByName: true,
		},
		{
			name: "node name missing", kind: KindNode,
			identifier: "no-such-node",
			lookupErr:  pgx.ErrNoRows,
			wantByName: true, wantCode: CodeNotFound,
		},

		// VM - name-only.
		{
			name: "vm uuid rejected", kind: KindVM,
			identifier: "01928cf4-1f00-7c80-8b00-000000000006",
			wantCode:   CodeUUIDInName,
		},
		{
			name: "vm name hit", kind: KindVM,
			identifier: "demo-vm",
			wantByName: true,
		},
		{
			name: "vm name missing", kind: KindVM,
			identifier: "no-such-vm",
			lookupErr:  pgx.ErrNoRows,
			wantByName: true, wantCode: CodeNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubQuerier{}
			programStub(stub, tc.kind, tc.lookupErr)
			if tc.kind == KindPool {
				stub.poolList = makePoolList(tc.poolListLen)
			}

			err := callResolve(t, stub, tc.kind, tc.identifier)
			gotByID, gotByName := stubRoute(stub, tc.kind)

			if gotByID != tc.wantByID {
				t.Errorf("Resolve(%s, %q) byID=%v, want %v", tc.kind, tc.identifier, gotByID, tc.wantByID)
			}
			if gotByName != tc.wantByName {
				t.Errorf("Resolve(%s, %q) byName=%v, want %v", tc.kind, tc.identifier, gotByName, tc.wantByName)
			}

			if tc.wantCode == "" {
				if err != nil {
					t.Errorf("Resolve(%s, %q) err=%v, want nil", tc.kind, tc.identifier, err)
				}
				return
			}
			var rerr *Error
			if !errors.As(err, &rerr) {
				t.Fatalf("Resolve(%s, %q) err type = %T, want *Error", tc.kind, tc.identifier, err)
			}
			if rerr.Code != tc.wantCode {
				t.Errorf("Resolve(%s, %q) code = %s, want %s", tc.kind, tc.identifier, rerr.Code, tc.wantCode)
			}
			if rerr.Kind != tc.kind {
				t.Errorf("Resolve(%s, %q) kind = %s, want %s", tc.kind, tc.identifier, rerr.Kind, tc.kind)
			}
			if rerr.Identifier != tc.identifier {
				t.Errorf("Resolve(%s, %q) identifier = %q, want %q", tc.kind, tc.identifier, rerr.Identifier, tc.identifier)
			}
		})
	}
}

func TestPoolByName(t *testing.T) {
	defaultName := "default"
	cases := []struct {
		name             string
		queryErr         error
		clusterErr       error
		instances        int
		clusterDefault   *string
		wantCode         ErrorCode
		wantIsDefault    bool
		wantInstancesLen int
	}{
		{
			name:     "empty list returns not_found",
			wantCode: CodeNotFound,
		},
		{
			name:             "single instance, not default",
			instances:        1,
			wantInstancesLen: 1,
		},
		{
			name:             "single instance, cluster default by exact match",
			instances:        1,
			clusterDefault:   &defaultName,
			wantIsDefault:    true,
			wantInstancesLen: 1,
		},
		{
			name:             "two instances aggregated",
			instances:        2,
			wantInstancesLen: 2,
		},
		{
			name:     "query error promotes to internal",
			queryErr: errors.New("connection reset"),
			wantCode: CodeInternal,
		},
		{
			name:       "cluster_settings error promotes to internal",
			instances:  1,
			clusterErr: errors.New("connection reset"),
			wantCode:   CodeInternal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubQuerier{
				poolList:           makePoolList(tc.instances),
				poolErr:            tc.queryErr,
				clusterSettings:    store.ClusterSetting{DefaultPoolName: tc.clusterDefault},
				clusterSettingsErr: tc.clusterErr,
			}
			got, err := PoolByName(context.Background(), stub, "default")
			if tc.wantCode != "" {
				var rerr *Error
				if !errors.As(err, &rerr) {
					t.Fatalf("PoolByName err type = %T, want *Error", err)
				}
				if rerr.Code != tc.wantCode {
					t.Errorf("PoolByName code = %s, want %s", rerr.Code, tc.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("PoolByName err = %v, want nil", err)
			}
			if got.IsClusterDefault != tc.wantIsDefault {
				t.Errorf("PoolByName IsClusterDefault = %v, want %v", got.IsClusterDefault, tc.wantIsDefault)
			}
			if len(got.Instances) != tc.wantInstancesLen {
				t.Errorf("PoolByName len(Instances) = %d, want %d", len(got.Instances), tc.wantInstancesLen)
			}
		})
	}
}

func TestIsNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "plain pgx", err: pgx.ErrNoRows, want: false},
		{name: "resolver not_found", err: &Error{Code: CodeNotFound}, want: true},
		{name: "resolver internal", err: &Error{Code: CodeInternal}, want: false},
		{name: "resolver uuid_in_name", err: &Error{Code: CodeUUIDInName}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNotFound(tc.err); got != tc.want {
				t.Errorf("IsNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsUUIDInName(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "plain pgx", err: pgx.ErrNoRows, want: false},
		{name: "resolver uuid_in_name", err: &Error{Code: CodeUUIDInName}, want: true},
		{name: "resolver not_found", err: &Error{Code: CodeNotFound}, want: false},
		{name: "resolver internal", err: &Error{Code: CodeInternal}, want: false},
		{name: "resolver ambiguous", err: &Error{Code: CodeAmbiguous}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUUIDInName(tc.err); got != tc.want {
				t.Errorf("IsUUIDInName(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func programStub(s *stubQuerier, kind Kind, err error) {
	switch kind {
	case KindTemplate:
		s.templateErr = err
	case KindPool:
		s.poolErr = err
	case KindNode:
		s.nodeErr = err
	case KindVM:
		s.vmErr = err
	}
}

func callResolve(t *testing.T, q Querier, kind Kind, identifier string) error {
	t.Helper()
	switch kind {
	case KindTemplate:
		_, err := Template(context.Background(), q, identifier)
		return err
	case KindPool:
		_, err := Pool(context.Background(), q, identifier)
		return err
	case KindNode:
		_, err := Node(context.Background(), q, identifier)
		return err
	case KindVM:
		_, err := VM(context.Background(), q, identifier)
		return err
	}
	t.Fatalf("unknown kind %q", kind)
	return nil
}

// makePoolList materialises n stub pool rows for the multi-instance
// resolver tests. The row contents are uninteresting - only the slice
// length drives the resolver's branch logic.
func makePoolList(n int) []store.StoragePool {
	if n <= 0 {
		return nil
	}
	rows := make([]store.StoragePool, n)
	for i := range rows {
		rows[i] = store.StoragePool{Name: "default", Type: "local_dir"}
	}
	return rows
}

func stubRoute(s *stubQuerier, kind Kind) (byID, byName bool) {
	switch kind {
	case KindTemplate:
		return false, s.templateByName
	case KindPool:
		return s.poolByID, s.poolByName
	case KindNode:
		return false, s.nodeByName
	case KindVM:
		return false, s.vmByName
	}
	return false, false
}
