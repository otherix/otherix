// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package clustermembers hosts the GET /v1/cluster/members and
// DELETE /v1/cluster/members/{id} HTTP handlers. The surface lets an admin
// inspect etcd cluster membership and evict a stale or failed member.
//
// Hex member IDs are used throughout: etcd prints them in hex in its own
// logs and tooling, so the API matches that convention rather than
// introducing a second notation.
package clustermembers

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/etcd"
)

// MemberManager is the membership surface the handler depends on. It is
// satisfied by *etcd.MembershipClient.
type MemberManager interface {
	ListMembers(ctx context.Context) ([]etcd.Member, error)
	RemoveMember(ctx context.Context, id uint64) error
}

// Handler bundles the dependencies for the cluster-members routes.
type Handler struct {
	mgr MemberManager
	log *slog.Logger
}

// New constructs a Handler.
func New(mgr MemberManager, log *slog.Logger) *Handler {
	return &Handler{mgr: mgr, log: log}
}

// memberView is the per-member JSON shape returned by List.
type memberView struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	PeerURLs   []string `json:"peer_urls"`
	ClientURLs []string `json:"client_urls"`
	IsLearner  bool     `json:"is_learner"`
}

// memberList is the top-level envelope for GET /v1/cluster/members.
type memberList struct {
	Data []memberView `json:"data"`
}

// List implements GET /v1/cluster/members. It renders the current etcd
// cluster membership as a JSON list, with each member ID expressed in
// lowercase hex to match etcd's own log and tooling convention.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	members, err := h.mgr.ListMembers(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "list cluster members",
			slog.String("error", err.Error()))
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "cluster membership unavailable", nil)
		return
	}

	views := make([]memberView, 0, len(members))
	for _, m := range members {
		views = append(views, memberView{
			ID:         strconv.FormatUint(m.ID, 16),
			Name:       m.Name,
			PeerURLs:   m.PeerURLs,
			ClientURLs: m.ClientURLs,
			IsLearner:  m.IsLearner,
		})
	}
	response.WriteJSON(w, r, http.StatusOK, memberList{Data: views})
}

// Remove implements DELETE /v1/cluster/members/{id}. The id path parameter
// must be a lowercase hex etcd member id. Removing a member that does not
// exist surfaces as a 500 from etcd - the CP does no existence pre-check.
// Quorum safety is etcd's responsibility: etcd rejects a remove that would
// lose quorum on a healthy cluster, so the CP does no quorum math.
func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(raw, 16, 64)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "member id must be a hex etcd member id", nil)
		return
	}

	if err := h.mgr.RemoveMember(r.Context(), id); err != nil {
		h.log.ErrorContext(r.Context(), "remove cluster member",
			slog.String("member_id", raw),
			slog.String("error", err.Error()))
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "could not remove member", nil)
		return
	}

	h.log.InfoContext(r.Context(), "cluster member removed",
		slog.String("member_id", raw))
	response.WriteNoContent(w)
}
