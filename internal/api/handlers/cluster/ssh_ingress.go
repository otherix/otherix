// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cluster

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
)

// SSHIngress is the wire shape of the cluster SSH-ingress configuration.
// Mirrors api/openapi/control-plane.yaml#components/schemas/ClusterSSHIngress.
// Enabled is the cluster-wide master switch VM create consults before
// provisioning a guest to trust the cluster SSH user-CA (effective value:
// ON by default); ClusterSuffix is the DNS suffix SSH-ingress VM hostnames
// are addressed under (effective value: the default when unconfigured).
type SSHIngress struct {
	Enabled       bool   `json:"enabled"`
	ClusterSuffix string `json:"cluster_suffix"`
}

// setSSHIngressRequest is the PUT body. When Enabled is true ClusterSuffix
// must be a non-empty DNS domain; the handler validates it before writing
// to cluster_settings.
type setSSHIngressRequest struct {
	Enabled       bool   `json:"enabled"`
	ClusterSuffix string `json:"cluster_suffix"`
}

// GetSSHIngress implements GET /v1/cluster/ssh-ingress. Returns the current
// SSH-ingress master switch and DNS suffix from the cluster_settings
// singleton; the suffix is empty when never configured. Permission:
// cluster:read.
func (h *Handler) GetSSHIngress(w http.ResponseWriter, r *http.Request) {
	settings, err := h.store.ClusterSettings(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "load cluster settings", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load cluster settings", nil)
		return
	}
	// Report the EFFECTIVE values: an unconfigured cluster resolves to ON with
	// the default suffix. The defaults live on store.ClusterSetting so the GET
	// view, the create gating, and the *Store accessors never drift.
	response.WriteJSON(w, r, http.StatusOK, SSHIngress{
		Enabled:       settings.SSHIngressEnabledOrDefault(),
		ClusterSuffix: settings.SSHClusterSuffixOrDefault(),
	})
}

// SetSSHIngress implements PUT /v1/cluster/ssh-ingress. It sets the
// SSH-ingress master switch and DNS suffix. Enabling requires a non-empty,
// valid DNS-domain suffix (the connector / cert-mint address a VM under it);
// disabling clears the requirement. Permission: cluster:manage.
func (h *Handler) SetSSHIngress(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "read request body", nil)
		return
	}
	var req setSSHIngressRequest
	if err := json.Unmarshal(body, &req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "decode request body", nil)
		return
	}
	suffix := strings.TrimSpace(req.ClusterSuffix)

	// A suffix is required to enable: without it the connector bundle and
	// cert-mint cannot address a VM. A non-empty suffix is validated even when
	// disabling so a stored value is always a well-formed domain.
	if req.Enabled && suffix == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"cluster_suffix is required when enabling SSH ingress", nil)
		return
	}
	if suffix != "" {
		if err := validation.ValidateDNSDomain(suffix); err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, err.Error(), nil)
			return
		}
	}

	// Persist the coupled (enabled, suffix) pair in a single store write so a
	// torn commit can never leave the singleton enabled with an empty suffix -
	// the combination the validation above forbids.
	var suffixPtr *string
	if suffix != "" {
		suffixPtr = &suffix
	}
	if err := h.store.SetSSHIngress(r.Context(), req.Enabled, suffixPtr); err != nil {
		h.log.ErrorContext(r.Context(), "set ssh ingress", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "persist ssh ingress", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, SSHIngress{
		Enabled:       req.Enabled,
		ClusterSuffix: suffix,
	})
}
