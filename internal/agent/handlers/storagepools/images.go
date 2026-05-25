// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/api/response"
)

// importImageBody is the wire shape the import handler decodes. Mirrors
// agent.yaml's `ImageImportRequest`. Only source_url is accepted;
// source_path is rejected with 400 unsupported_format (ROADMAP entry
// tracks the future-iteration local-file ingestion path).
type importImageBody struct {
	SourceURL              *string `json:"source_url"`
	SourcePath             *string `json:"source_path"`
	ExpectedChecksumSHA256 string  `json:"expected_checksum_sha256"`
	Format                 string  `json:"format"`
}

// cachedImageView mirrors agent.yaml's `CachedImage` schema. Hand-
// crafted (a la asyncAccepted) pending a shared types package; keep
// in sync with CachedImageList + the agent.yaml component.
type cachedImageView struct {
	ChecksumSHA256 string  `json:"checksum_sha256"`
	Format         string  `json:"format"`
	Path           string  `json:"path"`
	SizeBytes      int64   `json:"size_bytes"`
	LastUsedAt     *string `json:"last_used_at"`
	SourceURL      *string `json:"source_url"`
}

// cachedImageList mirrors agent.yaml's `CachedImageList`. `Meta` carries
// the pagination cursor; first-cut implementation always emits
// `next_cursor: null` because ListImages returns the entire inventory.
type cachedImageList struct {
	Data []cachedImageView `json:"data"`
	Meta cachedImageMeta   `json:"meta"`
}

type cachedImageMeta struct {
	NextCursor *string `json:"next_cursor"`
}

// ImportImage handles POST /v1/storage-pools/{pool_name}/images.
//
//   - Decodes the body, rejecting source_path (deferred enum
//     extension) and empty/malformed fields with 400.
//   - Asks the manager to begin an import; receives an agent task in
//     `pending` status.
//   - Returns 202 with the task_id immediately; CP polls
//     /v1/tasks/{task_id} to observe terminal status and result.
//
// Errors:
//   - 400 validation_failed — pool_name is empty, body decode failed,
//     source_url empty, or expected_checksum_sha256 malformed.
//   - 400 unsupported_format — format != "qcow2" or source_path set.
//   - 404 not_found — pool_name does not match a configured pool.
//   - 500 internal — unexpected manager failure.
func (h *Handler) ImportImage(w http.ResponseWriter, r *http.Request) {
	poolName := chi.URLParam(r, "pool_name")
	if poolName == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "pool_name is required", nil)
		return
	}

	var body importImageBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "decode body: "+err.Error(), nil)
		return
	}

	if body.SourcePath != nil && *body.SourcePath != "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeUnsupportedFormat,
			"source_path is not implemented (use source_url instead)", nil)
		return
	}
	if body.SourceURL == nil || *body.SourceURL == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "source_url is required", nil)
		return
	}

	req := vm.ImportImageRequest{
		SourceURL:              *body.SourceURL,
		ExpectedChecksumSHA256: body.ExpectedChecksumSHA256,
		Format:                 body.Format,
	}
	task, err := h.manager.ImportImage(r.Context(), poolName, req)
	if err != nil {
		switch {
		case errors.Is(err, vm.ErrPoolUnknown):
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "pool does not match a configured pool", nil)
		case errors.Is(err, vm.ErrUnsupportedFormat):
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeUnsupportedFormat,
				"only qcow2 format is supported", nil)
		case errors.Is(err, vm.ErrMissingSourceURL):
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "source_url is required", nil)
		case errors.Is(err, vm.ErrInvalidChecksumFormat):
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed,
				"expected_checksum_sha256 must be 64-char lowercase hex", nil)
		default:
			h.log.ErrorContext(r.Context(), "import image failed",
				"pool_name", poolName, "err", err)
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "internal error", nil)
		}
		return
	}

	response.WriteJSON(w, r, http.StatusAccepted, asyncAccepted{
		TaskID: task.ID.String(),
		Status: string(task.Status),
		Links:  map[string]any{"self": "/v1/tasks/" + task.ID.String()},
	})
}

// DeleteImage handles DELETE /v1/storage-pools/{pool_name}/images/{checksum}.
//
// Synchronous, idempotent — 204 whether or not the file was present
// (matches the CP-side agentclient.DeleteImage contract, which
// collapses 204 / 404 to nil so that a stale row removal stays safe
// under agent-side manual cleanup).
//
// Errors:
//   - 400 validation_failed — pool_name empty or checksum malformed.
//   - 404 not_found — pool_name does not match a configured pool.
//   - 500 internal — unexpected filesystem failure.
func (h *Handler) DeleteImage(w http.ResponseWriter, r *http.Request) {
	poolName := chi.URLParam(r, "pool_name")
	if poolName == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "pool_name is required", nil)
		return
	}
	checksum := chi.URLParam(r, "checksum")

	if err := h.manager.DeleteImage(r.Context(), poolName, checksum); err != nil {
		switch {
		case errors.Is(err, vm.ErrPoolUnknown):
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "pool does not match a configured pool", nil)
		case errors.Is(err, vm.ErrInvalidChecksumFormat):
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed,
				"checksum must be 64-char lowercase hex", nil)
		default:
			h.log.ErrorContext(r.Context(), "delete image failed",
				"pool_name", poolName, "checksum", checksum, "err", err)
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "internal error", nil)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListImages handles GET /v1/storage-pools/{pool_name}/images.
//
// First-cut pagination returns the entire inventory in `data` with
// `meta.next_cursor = null`. ROADMAP entry "agent storage_images list
// — cursor pagination" tracks the deferred cursor implementation;
// CP-side agentclient.ListImages handles null next_cursor gracefully.
//
// Errors:
//   - 400 validation_failed — pool_name is empty.
//   - 404 not_found — pool_name does not match a configured pool.
//   - 500 internal — filesystem failure walking templates/.
func (h *Handler) ListImages(w http.ResponseWriter, r *http.Request) {
	poolName := chi.URLParam(r, "pool_name")
	if poolName == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "pool_name is required", nil)
		return
	}

	images, err := h.manager.ListImages(r.Context(), poolName)
	if err != nil {
		if errors.Is(err, vm.ErrPoolUnknown) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "pool does not match a configured pool", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "list images failed",
			"pool_name", poolName, "err", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
		return
	}

	views := make([]cachedImageView, 0, len(images))
	for _, img := range images {
		view := cachedImageView{
			ChecksumSHA256: img.ChecksumSHA256,
			Format:         img.Format,
			Path:           img.Path,
			SizeBytes:      img.SizeBytes,
		}
		if img.LastUsedAt != nil {
			ts := img.LastUsedAt.UTC().Format(time.RFC3339Nano)
			view.LastUsedAt = &ts
		}
		if img.SourceURL != nil {
			view.SourceURL = img.SourceURL
		}
		views = append(views, view)
	}
	response.WriteJSON(w, r, http.StatusOK, cachedImageList{
		Data: views,
		Meta: cachedImageMeta{NextCursor: nil},
	})
}
