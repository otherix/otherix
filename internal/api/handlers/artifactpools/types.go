// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactpools

import (
	"time"

	"github.com/otherix/otherix/internal/store"
)

type createRequest struct {
	Name              string                   `json:"name"`
	ReplicationFactor *store.ReplicationFactor `json:"replication_factor"`
	Membership        *membershipBody          `json:"membership,omitempty"`
}

type membershipBody struct {
	AllNodes bool     `json:"all_nodes"`
	Nodes    []string `json:"nodes,omitempty"`
}

type patchRequest struct {
	ReplicationFactor *store.ReplicationFactor `json:"replication_factor"`
	Membership        *membershipBody          `json:"membership"`
}

type artifactPoolView struct {
	ID                string                  `json:"id"`
	Name              string                  `json:"name"`
	ReplicationFactor store.ReplicationFactor `json:"replication_factor"`
	Membership        membershipBody          `json:"membership"`
	CreatedAt         string                  `json:"created_at"`
	UpdatedAt         string                  `json:"updated_at"`
}

type listResponse struct {
	Data []artifactPoolView `json:"data"`
	Meta paginationMeta     `json:"meta"`
}

type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
}

func toView(ap store.ArtifactPool) artifactPoolView {
	return artifactPoolView{
		ID:                ap.ID.String(),
		Name:              ap.Name,
		ReplicationFactor: ap.ReplicationFactor,
		Membership:        membershipBody{AllNodes: ap.Membership.AllNodes, Nodes: ap.Membership.Nodes},
		CreatedAt:         ap.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:         ap.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}
