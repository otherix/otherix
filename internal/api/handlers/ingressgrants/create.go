// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingressgrants

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// grantNameMaxLength bounds the grant name. The name is part of an etcd
// uniqueness-guard key path, so it must also be free of '/'.
const grantNameMaxLength = 200

// createVM is one VM entry in the create / add-vm request body.
type createVM struct {
	VMName string `json:"vm_name"`
	Ports  []int  `json:"ports"`
	Login  string `json:"login"`
}

// createRequest is the POST /v1/ingress-grants body. ttl is an optional Go
// duration string (e.g. "168h"); omitted/empty means the grant never
// expires (ExpiresAt nil).
type createRequest struct {
	Name           string     `json:"name"`
	RecipientLabel string     `json:"recipient_label"`
	VMs            []createVM `json:"vms"`
	TTL            string     `json:"ttl"`
}

// Create implements POST /v1/ingress-grants. Required permission:
// vm:ingress-grant. For a developer (scope=own) every referenced VM must be
// owned by the caller; a visible-but-unowned VM yields 403. Returns 201
// with the grant plus the one-time plaintext token.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if err := validateGrantName(req.Name); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}

	vms, err := validateVMs(req.VMs)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}

	expiresAt, err := parseTTL(req.TTL)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}

	if !h.authorizeVMs(w, r, caller, vms) {
		return
	}

	plaintext, hash, err := auth.GenerateIngressGrantToken()
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "generate grant token", nil)
		return
	}

	grant, err := h.store.CreateIngressGrant(r.Context(), store.CreateIngressGrantParams{
		Name:           req.Name,
		CreatedBy:      caller.ID,
		RecipientLabel: strings.TrimSpace(req.RecipientLabel),
		TokenHash:      hash,
		VMs:            vms,
		ExpiresAt:      expiresAt,
	})
	if err != nil {
		if errors.Is(err, store.ErrIngressGrantNameExists) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "ingress grant name already in use", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "persist ingress grant", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusCreated, grantCreateResponse{
		grantView: toView(grant),
		Token:     plaintext,
	})
}

// authorizeVMs resolves every referenced VM by name and enforces the
// caller's vm:ingress-grant scope against each VM's owner. A missing VM
// yields 404 (the VM name is caller-supplied, not secret); a visible VM
// the caller may not grant on yields 403. It returns false and writes the
// response on the first failure.
func (h *Handler) authorizeVMs(w http.ResponseWriter, r *http.Request, caller *auth.User, vms []store.IngressGrantVM) bool {
	for _, vm := range vms {
		row, err := h.store.VMByName(r.Context(), vm.VMName)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				response.WriteError(w, r, http.StatusNotFound,
					response.CodeNotFound, "vm not found: "+vm.VMName, nil)
				return false
			}
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "load vm", nil)
			return false
		}
		owner := row.OwnerID
		if err := auth.CheckOwnership(caller, &owner, auth.PermVMIngressGrant); err != nil {
			if errors.Is(err, auth.ErrPermissionDenied) {
				response.WriteError(w, r, http.StatusForbidden,
					response.CodePermissionDenied,
					"vm:ingress-grant on this vm is limited to its owner",
					map[string]any{"vm_name": vm.VMName})
				return false
			}
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "authorize vm", nil)
			return false
		}
	}
	return true
}

// validateGrantName checks the grant name (already trimmed): non-empty,
// within the length bound, and free of '/' (which would poison the etcd
// uniqueness-guard key path).
func validateGrantName(name string) error {
	switch {
	case name == "":
		return errors.New("name is required")
	case utf8.RuneCountInString(name) > grantNameMaxLength:
		return errors.New("name is too long")
	case strings.ContainsRune(name, '/'):
		return errors.New("name must not contain '/'")
	}
	return nil
}

// validateVMs trims and validates the VM-scope entries, rejecting an empty
// vm_name, an empty or invalid port set, a duplicate vm_name, and a missing
// login only when the port set includes the SSH port (22). It returns the
// normalised store entries.
func validateVMs(in []createVM) ([]store.IngressGrantVM, error) {
	out := make([]store.IngressGrantVM, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, vm := range in {
		entry, err := validateVM(vm)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[entry.VMName]; dup {
			return nil, errors.New("duplicate vm_name: " + entry.VMName)
		}
		seen[entry.VMName] = struct{}{}
		out = append(out, entry)
	}
	return out, nil
}

// validateVM trims and validates a single VM-scope entry. A non-empty port set
// is required and every port must be in 1..65535 with no duplicate. When set,
// the login must be a safe SSH principal: it is signed into a guest SSH
// certificate and printed in an ssh <login>@host command, so it is held to the
// same charset/length rule as the cert-mint path (validation.ValidateSSHLogin).
// A login is required only when the port set includes the SSH port (22): the
// cert-mint path signs that pinned login into the guest cert, and an empty
// principal is rejected by every sshd. A non-SSH port set (e.g. db:5432 only)
// may omit the login.
func validateVM(vm createVM) (store.IngressGrantVM, error) {
	name := strings.TrimSpace(vm.VMName)
	if name == "" {
		return store.IngressGrantVM{}, errors.New("vm_name is required")
	}
	if len(vm.Ports) == 0 {
		return store.IngressGrantVM{}, errors.New("at least one port is required for " + name)
	}
	seenPort := make(map[int]struct{}, len(vm.Ports))
	for _, p := range vm.Ports {
		if p < 1 || p > 65535 {
			return store.IngressGrantVM{}, errors.New("port must be in 1..65535")
		}
		if _, dup := seenPort[p]; dup {
			return store.IngressGrantVM{}, errors.New("duplicate port for " + name)
		}
		seenPort[p] = struct{}{}
	}
	login := strings.TrimSpace(vm.Login)
	if login != "" {
		sanitized, err := validation.ValidateSSHLogin(login)
		if err != nil {
			return store.IngressGrantVM{}, err
		}
		login = sanitized
	}
	if login == "" {
		for _, p := range vm.Ports {
			if p == 22 {
				return store.IngressGrantVM{}, errors.New("login is required when the SSH port (22) is granted for " + name)
			}
		}
	}
	return store.IngressGrantVM{VMName: name, Ports: vm.Ports, Login: login}, nil
}

// parseTTL turns the optional duration string into an absolute expiry. An
// empty string means no expiry. A non-positive or malformed duration is a
// validation error.
func parseTTL(ttl string) (*time.Time, error) {
	ttl = strings.TrimSpace(ttl)
	if ttl == "" {
		return nil, nil
	}
	d, err := time.ParseDuration(ttl)
	if err != nil {
		return nil, errors.New("ttl must be a valid duration (e.g. \"168h\")")
	}
	if d <= 0 {
		return nil, errors.New("ttl must be positive")
	}
	exp := time.Now().UTC().Add(d)
	return &exp, nil
}
