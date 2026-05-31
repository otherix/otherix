// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package clustermembers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/etcd"
)

// fakeManager is a MemberManager test fake. It returns the canned members
// slice on ListMembers and records the ids passed to RemoveMember.
type fakeManager struct {
	members    []etcd.Member
	listErr    error
	removeErr  error
	removedIDs []uint64
}

func (f *fakeManager) ListMembers(_ context.Context) ([]etcd.Member, error) {
	return f.members, f.listErr
}

func (f *fakeManager) RemoveMember(_ context.Context, id uint64) error {
	f.removedIDs = append(f.removedIDs, id)
	return f.removeErr
}

func newTestRouter(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/v1/cluster/members", h.List)
	r.Delete("/v1/cluster/members/{id}", h.Remove)
	return r
}

func TestListMembers(t *testing.T) {
	mgr := &fakeManager{
		members: []etcd.Member{
			{
				ID:         0x1a2b,
				Name:       "n0",
				PeerURLs:   []string{"https://10.0.0.1:2380"},
				ClientURLs: []string{"https://10.0.0.1:2379"},
				IsLearner:  false,
			},
			{
				ID:         0x3c4d,
				Name:       "",
				PeerURLs:   nil,
				ClientURLs: nil,
				IsLearner:  true,
			},
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(mgr, log)
	router := newTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/v1/cluster/members", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Fatalf("List() status = %d, want %d", got, want)
	}

	var body memberList
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("List() decode body: %v", err)
	}

	if got, want := len(body.Data), 2; got != want {
		t.Fatalf("List() len(data) = %d, want %d", got, want)
	}

	if got, want := body.Data[0].ID, "1a2b"; got != want {
		t.Errorf("List() data[0].id = %q, want %q", got, want)
	}
	if got, want := body.Data[0].Name, "n0"; got != want {
		t.Errorf("List() data[0].name = %q, want %q", got, want)
	}
	if got, want := body.Data[0].IsLearner, false; got != want {
		t.Errorf("List() data[0].is_learner = %v, want %v", got, want)
	}
	if got, want := body.Data[1].ID, "3c4d"; got != want {
		t.Errorf("List() data[1].id = %q, want %q", got, want)
	}
	if got, want := body.Data[1].IsLearner, true; got != want {
		t.Errorf("List() data[1].is_learner = %v, want %v", got, want)
	}
}

func TestListMembersError(t *testing.T) {
	mgr := &fakeManager{listErr: context.DeadlineExceeded}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(mgr, log)
	router := newTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/v1/cluster/members", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got, want := rec.Code, http.StatusInternalServerError; got != want {
		t.Errorf("List() error status = %d, want %d", got, want)
	}
}

func TestRemoveMember(t *testing.T) {
	mgr := &fakeManager{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(mgr, log)
	router := newTestRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/v1/cluster/members/3c4d", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got, want := rec.Code, http.StatusNoContent; got != want {
		t.Fatalf("Remove() status = %d, want %d", got, want)
	}

	if got, want := len(mgr.removedIDs), 1; got != want {
		t.Fatalf("Remove() removedIDs len = %d, want %d", got, want)
	}
	if got, want := mgr.removedIDs[0], uint64(0x3c4d); got != want {
		t.Errorf("Remove() removedIDs[0] = %#x, want %#x", got, want)
	}
}

func TestRemoveMemberBadID(t *testing.T) {
	mgr := &fakeManager{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(mgr, log)
	router := newTestRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/v1/cluster/members/not-hex", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got, want := rec.Code, http.StatusBadRequest; got != want {
		t.Errorf("Remove() bad id status = %d, want %d", got, want)
	}

	if got, want := len(mgr.removedIDs), 0; got != want {
		t.Errorf("Remove() bad id removedIDs len = %d, want %d", got, want)
	}
}
