// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// The methods in this file are the firmware domain entrypoints on
// *Store: they wrap the raw sqlc-generated *Queries primitives,
// translate driver errors into store sentinels, and assemble
// transactional operations. Handlers depend on these, not on *Queries.

// Firmware-domain sentinel errors returned by the firmware methods on
// *Store. They translate the firmware uniqueness constraints into
// stable values handlers match with errors.Is, so the conflict policy
// is expressed in domain terms rather than constraint names.
var (
	// ErrFirmwareNameExists is returned when a firmware with the same
	// (name, architecture) already exists (uq_firmwares_name_arch).
	ErrFirmwareNameExists = errors.New("store: firmware name already in use for architecture")
	// ErrFirmwareDefaultExists is returned when another firmware is
	// already the default for the (architecture, type) pair
	// (uq_firmwares_default).
	ErrFirmwareDefaultExists = errors.New("store: default firmware already exists for architecture and type")
)

// translateFirmwareUnique maps the firmware uniqueness constraint
// violations to their domain sentinels; any other error passes through
// unchanged.
func translateFirmwareUnique(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "uq_firmwares_default":
			return ErrFirmwareDefaultExists
		case "uq_firmwares_name_arch":
			return ErrFirmwareNameExists
		}
	}
	return err
}

// FirmwareByID returns the firmware with the given id, or ErrNotFound
// when no such row exists.
func (s *Store) FirmwareByID(ctx context.Context, id uuid.UUID) (Firmware, error) {
	f, err := s.queries.GetFirmwareByID(ctx, id)
	if err != nil {
		return Firmware{}, translateNoRows(err)
	}
	return f, nil
}

// DefaultFirmwareForArchType returns the default firmware for the
// (architecture, type) pair, or ErrNotFound when none is marked
// default.
func (s *Store) DefaultFirmwareForArchType(ctx context.Context, arch CPUArch, ftype FirmwareType) (Firmware, error) {
	f, err := s.queries.GetDefaultFirmwareForArchAndType(ctx, GetDefaultFirmwareForArchAndTypeParams{
		Architecture: arch,
		Type:         ftype,
	})
	if err != nil {
		return Firmware{}, translateNoRows(err)
	}
	return f, nil
}

// CreateFirmware inserts a firmware, translating the uniqueness
// violations to ErrFirmwareNameExists / ErrFirmwareDefaultExists.
func (s *Store) CreateFirmware(ctx context.Context, arg CreateFirmwareParams) (Firmware, error) {
	f, err := s.queries.CreateFirmware(ctx, arg)
	if err != nil {
		return Firmware{}, translateFirmwareUnique(err)
	}
	return f, nil
}

// UpdateFirmware updates a firmware, translating the uniqueness
// violations to ErrFirmwareNameExists / ErrFirmwareDefaultExists.
func (s *Store) UpdateFirmware(ctx context.Context, arg UpdateFirmwareParams) (Firmware, error) {
	f, err := s.queries.UpdateFirmware(ctx, arg)
	if err != nil {
		return Firmware{}, translateFirmwareUnique(err)
	}
	return f, nil
}

// ListFirmwares returns firmwares matching the cursor-paginated
// filters in arg.
func (s *Store) ListFirmwares(ctx context.Context, arg ListFirmwaresParams) ([]Firmware, error) {
	return s.queries.ListFirmwares(ctx, arg)
}

// ListNodeFirmwares returns the firmwares reported available on a node,
// joined with their firmware metadata, cursor-paginated.
func (s *Store) ListNodeFirmwares(ctx context.Context, arg ListNodeFirmwaresParams) ([]ListNodeFirmwaresRow, error) {
	return s.queries.ListNodeFirmwares(ctx, arg)
}

// DeleteFirmware removes the firmware in a single transaction after
// verifying it exists and is unreferenced. Returns ErrNotFound when the
// row is missing, or *ResourceInUseError (keys "vms" / "templates")
// when active vms or templates still reference it.
func (s *Store) DeleteFirmware(ctx context.Context, id uuid.UUID) error {
	return s.InTx(ctx, func(q *Queries) error {
		if _, err := q.GetFirmwareByID(ctx, id); err != nil {
			return translateNoRows(err)
		}
		vmCount, err := q.CountVMsForFirmware(ctx, id)
		if err != nil {
			return err
		}
		tplCount, err := q.CountTemplatesForFirmware(ctx, id)
		if err != nil {
			return err
		}
		if vmCount > 0 || tplCount > 0 {
			blocking := map[string]int64{}
			if vmCount > 0 {
				blocking["vms"] = vmCount
			}
			if tplCount > 0 {
				blocking["templates"] = tplCount
			}
			return &ResourceInUseError{Resources: blocking}
		}
		return q.DeleteFirmware(ctx, id)
	})
}
