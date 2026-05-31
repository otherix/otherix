// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

// Package apie2e holds the etcd-backed api-server end-to-end suite: it builds
// the real chi router (full middleware stack) over an embedded etcd member and
// drives it through an httptest.Server the way a real client would. No Docker,
// no Postgres - the embedded member runs in-process. It is the etcd successor
// to the Postgres-testcontainer e2e harness that lived in internal/api.
//
// The suite covers the backend-agnostic HTTP surface that needs no dependent-
// row seeding: auth (login/refresh/logout rotation), user CRUD + RBAC, the
// infrastructure-resource CRUD (networks, firmwares, nodes, templates),
// idempotency replay, and the health probes. Cross-resource delete-blocking
// (which seeds vm_nics / vm_disks / migrations) stays covered at the store
// layer by the etcdstore integration suite plus the Lima smoke.
package apie2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// sharedEtcdClient is the embedded member every test runs against. Each newE2E
// wipes the keyspace for a clean slate, so the sequential suite stays isolated
// without paying a per-test member-startup cost.
var sharedEtcdClient *etcd.Client

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("freePort: %v", err))
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "otherix-apie2e")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdtemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rt, err := etcd.Start(ctx, &etcd.Config{
		Mode:         etcd.ModeSingle,
		Name:         "apie2e",
		DataDir:      filepath.Join(dir, "member"),
		PeerURL:      fmt.Sprintf("http://127.0.0.1:%d", freePort()),
		ClientURL:    fmt.Sprintf("http://127.0.0.1:%d", freePort()),
		ClusterToken: "otherix-apie2e",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "etcd.Start: %v\n", err)
		os.Exit(1)
	}
	sharedEtcdClient = etcd.NewClient(rt)

	code := m.Run()

	_ = sharedEtcdClient.Close()
	rt.Stop(10 * time.Second)
	os.Exit(code)
}

// harness wires the etcd-backed router into an httptest.Server.
type harness struct {
	srv   *httptest.Server
	store *etcdstore.Store
	svc   *auth.Service
}

// newE2E wipes the keyspace and builds a fresh router over the shared member.
func newE2E(t *testing.T) *harness {
	t.Helper()
	if sharedEtcdClient == nil {
		t.Fatal("sharedEtcdClient not initialised")
	}
	ctx := context.Background()
	if _, err := sharedEtcdClient.Raw().Delete(ctx, etcd.KeyPrefix, clientv3.WithPrefix()); err != nil {
		t.Fatalf("wipe keyspace: %v", err)
	}
	s := etcdstore.New(sharedEtcdClient)

	svc, err := auth.NewService(auth.Config{
		JWTSecret:    []byte("apie2e-secret-32-bytes-padding!!"),
		JWTAccessTTL: 15 * time.Minute,
		RefreshTTL:   720 * time.Hour,
	}, s)
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}

	if err := api.BootstrapClusterCA(ctx, s, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("BootstrapClusterCA: %v", err)
	}

	router := api.NewRouter(api.RouterDeps{
		Store:       s,
		AuthService: svc,
		StoragePools: config.StoragePoolsConfig{
			AllowedPathPrefixes: []string{"/opt/otherix/pools/"},
		},
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		RequestTimeout:  10 * time.Second,
		HealthCheckName: "etcd",
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	return &harness{srv: srv, store: s, svc: svc}
}

func (h *harness) do(t *testing.T, method, path string, body any, bearer string, headers map[string]string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func (h *harness) post(t *testing.T, path string, body any, bearer string) *http.Response {
	return h.do(t, http.MethodPost, path, body, bearer, nil)
}

func (h *harness) get(t *testing.T, path, bearer string) *http.Response {
	return h.do(t, http.MethodGet, path, nil, bearer, nil)
}

func (h *harness) patch(t *testing.T, path string, body any, bearer string) *http.Response {
	return h.do(t, http.MethodPatch, path, body, bearer, nil)
}

func (h *harness) delete(t *testing.T, path, bearer string) *http.Response {
	return h.do(t, http.MethodDelete, path, nil, bearer, nil)
}

func decodeJSON(t *testing.T, resp *http.Response, target any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// seedUser inserts a user with the given role and a real argon2id hash,
// returning id, email, and plaintext password.
func seedUser(t *testing.T, s *etcdstore.Store, role auth.Role) (uuid.UUID, string, string) {
	t.Helper()
	const pw = "correct-horse-battery-staple"
	hash, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	id := uuid.New()
	email := fmt.Sprintf("e2e-%s-%s@example.test", role, uuid.NewString()[:8])
	if _, err := s.CreateUser(context.Background(), store.CreateUserParams{
		ID: id, Email: email, PasswordHash: hash, DisplayName: "E2E " + string(role), Role: string(role),
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return id, email, pw
}

// loginAs seeds a user with the role and logs in over HTTP, returning the
// access token and the user id. Exercises the real auth.login path.
func loginAs(t *testing.T, h *harness, role auth.Role) (token string, userID uuid.UUID) {
	t.Helper()
	id, email, pw := seedUser(t, h.store, role)
	resp := h.post(t, "/v1/auth/login", map[string]string{"email": email, "password": pw}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, resp, &out)
	if out.AccessToken == "" {
		t.Fatal("login returned empty access_token")
	}
	return out.AccessToken, id
}
