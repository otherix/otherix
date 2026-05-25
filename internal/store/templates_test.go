// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/store"
)

// uniqueTemplateName returns a per-call unique name so tests in this
// package don't collide on uq_templates_name.
func uniqueTemplateName(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// seedUser inserts a user with a unique email and returns the id.
// Templates are owned (FK to users.id ON DELETE RESTRICT), so every
// templates test needs at least one principal to play the part.
func seedUser(t *testing.T, ctx context.Context, s *store.Store, role string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID:           id,
		Email:        uniqueEmail(),
		PasswordHash: "x",
		DisplayName:  "templates-test-" + role,
		Role:         role,
	}); err != nil {
		t.Fatalf("seedUser: %v", err)
	}
	return id
}

// defaultTemplateParams returns a minimal CreateTemplateParams that
// satisfies every NOT NULL column with a sensible default; tests
// override the fields they care about.
func defaultTemplateParams(id, owner uuid.UUID, name string) store.CreateTemplateParams {
	return store.CreateTemplateParams{
		ID:                     id,
		OwnerID:                owner,
		Name:                   name,
		Description:            "",
		Architecture:           store.CpuArchAmd64,
		OsFamily:               store.OsFamilyLinux,
		OsVariant:              "",
		ImageUrl:               "https://images.example.test/" + name + ".qcow2",
		ImageChecksumSha256:    bytes32(0xab),
		ImageFormat:            store.ImageFormatQcow2,
		ImageSizeBytes:         nil,
		FirmwareType:           store.FirmwareTypeUefi,
		FirmwareID:             nil,
		DefaultCpuCores:        2,
		DefaultMemoryMib:       2048,
		DefaultDiskGib:         20,
		CloudInitSupported:     true,
		QemuGuestAgentExpected: false,
		Metadata:               []byte(`{}`),
	}
}

// bytes32 returns 32 identical bytes — handy as a checksum stand-in
// without pulling in crypto/rand for purely structural tests.
func bytes32(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestTemplatesCreateGetSoftDelete(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	id := uuid.New()
	name := uniqueTemplateName("crud")

	created, err := s.Queries().CreateTemplate(ctx, defaultTemplateParams(id, owner, name))
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if created.Visibility != "private" {
		t.Errorf("created.Visibility = %q, want %q (templates always created private)", created.Visibility, "private")
	}
	if created.OwnerID != owner {
		t.Errorf("created.OwnerID = %v, want %v", created.OwnerID, owner)
	}

	got, err := s.Queries().GetTemplate(ctx, id)
	if err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
	if got.Name != name {
		t.Errorf("got.Name = %q, want %q", got.Name, name)
	}

	if err := s.Queries().SoftDeleteTemplate(ctx, id); err != nil {
		t.Fatalf("SoftDeleteTemplate: %v", err)
	}
	if _, err := s.Queries().GetTemplate(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetTemplate after soft-delete err = %v, want pgx.ErrNoRows", err)
	}
}

func TestTemplatesGlobalNameUniqueness(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	ownerA := seedUser(t, ctx, s, "developer")
	ownerB := seedUser(t, ctx, s, "developer")
	name := uniqueTemplateName("dup")

	if _, err := s.Queries().CreateTemplate(ctx, defaultTemplateParams(uuid.New(), ownerA, name)); err != nil {
		t.Fatalf("first CreateTemplate: %v", err)
	}
	_, err := s.Queries().CreateTemplate(ctx, defaultTemplateParams(uuid.New(), ownerB, name))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("duplicate-name across owners err = %v, want pg 23505", err)
	}
}

func TestTemplatesSoftDeleteFreesName(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	name := uniqueTemplateName("freed")
	firstID := uuid.New()

	if _, err := s.Queries().CreateTemplate(ctx, defaultTemplateParams(firstID, owner, name)); err != nil {
		t.Fatalf("first CreateTemplate: %v", err)
	}
	if err := s.Queries().SoftDeleteTemplate(ctx, firstID); err != nil {
		t.Fatalf("SoftDeleteTemplate: %v", err)
	}
	// Same name should now be re-usable thanks to the partial unique
	// index uq_templates_name (where deleted_at is null).
	if _, err := s.Queries().CreateTemplate(ctx, defaultTemplateParams(uuid.New(), owner, name)); err != nil {
		t.Errorf("re-create after soft-delete failed: %v", err)
	}
}

func TestTemplatesUpdate(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	id := uuid.New()
	original, err := s.Queries().CreateTemplate(ctx, defaultTemplateParams(id, owner, uniqueTemplateName("upd")))
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	updated, err := s.Queries().UpdateTemplate(ctx, store.UpdateTemplateParams{
		ID:                     id,
		Name:                   original.Name,
		Description:            "new description",
		OsVariant:              "ubuntu-2404",
		FirmwareType:           store.FirmwareTypeBios,
		FirmwareID:             nil,
		DefaultCpuCores:        4,
		DefaultMemoryMib:       4096,
		DefaultDiskGib:         40,
		CloudInitSupported:     false,
		QemuGuestAgentExpected: true,
		Metadata:               []byte(`{"k":"v"}`),
	})
	if err != nil {
		t.Fatalf("UpdateTemplate: %v", err)
	}
	if updated.Description != "new description" || updated.OsVariant != "ubuntu-2404" {
		t.Errorf("description / os_variant did not stick: %+v", updated)
	}
	if updated.DefaultCpuCores != 4 || updated.DefaultMemoryMib != 4096 || updated.DefaultDiskGib != 40 {
		t.Errorf("sizing did not stick: %+v", updated)
	}
	if updated.FirmwareType != store.FirmwareTypeBios {
		t.Errorf("firmware_type = %q, want %q", updated.FirmwareType, store.FirmwareTypeBios)
	}
	if updated.CloudInitSupported || !updated.QemuGuestAgentExpected {
		t.Errorf("capability bits did not stick: %+v", updated)
	}
	if !updated.UpdatedAt.After(updated.CreatedAt) {
		t.Errorf("UpdatedAt (%v) not after CreatedAt (%v)", updated.UpdatedAt, updated.CreatedAt)
	}
}

func TestTemplatesSetVisibilityRoundTrip(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	id := uuid.New()
	if _, err := s.Queries().CreateTemplate(ctx, defaultTemplateParams(id, owner, uniqueTemplateName("vis"))); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	pub, err := s.Queries().SetTemplateVisibility(ctx, store.SetTemplateVisibilityParams{
		ID: id, Visibility: "public",
	})
	if err != nil {
		t.Fatalf("SetTemplateVisibility public: %v", err)
	}
	if pub.Visibility != "public" {
		t.Errorf("Visibility = %q, want public", pub.Visibility)
	}
	if pub.OwnerID != owner {
		t.Errorf("publishing changed OwnerID: got %v want %v", pub.OwnerID, owner)
	}

	priv, err := s.Queries().SetTemplateVisibility(ctx, store.SetTemplateVisibilityParams{
		ID: id, Visibility: "private",
	})
	if err != nil {
		t.Fatalf("SetTemplateVisibility private: %v", err)
	}
	if priv.Visibility != "private" {
		t.Errorf("Visibility = %q, want private", priv.Visibility)
	}
}

func TestTemplatesListVisibilityClamp(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	alice := seedUser(t, ctx, s, "developer")
	bob := seedUser(t, ctx, s, "developer")

	// Tag rows with a per-test prefix so the shared harness's leftover
	// templates don't pollute the assertion.
	tag := uniqueTemplateName("clamp")
	createTagged := func(owner uuid.UUID, suffix string) uuid.UUID {
		id := uuid.New()
		params := defaultTemplateParams(id, owner, tag+"-"+suffix)
		if _, err := s.Queries().CreateTemplate(ctx, params); err != nil {
			t.Fatalf("seed %s: %v", suffix, err)
		}
		return id
	}
	alicePub := createTagged(alice, "alice-pub")
	if _, err := s.Queries().SetTemplateVisibility(ctx, store.SetTemplateVisibilityParams{ID: alicePub, Visibility: "public"}); err != nil {
		t.Fatalf("publish alice-pub: %v", err)
	}
	alicePriv := createTagged(alice, "alice-priv")
	bobPriv := createTagged(bob, "bob-priv")

	collect := func(rows []store.Template) map[uuid.UUID]struct{} {
		m := make(map[uuid.UUID]struct{}, len(rows))
		for _, r := range rows {
			m[r.ID] = struct{}{}
		}
		return m
	}

	// Admin / operator clamp: caller_can_see_any=true.
	rows, err := s.Queries().ListTemplates(ctx, store.ListTemplatesParams{
		CallerCanSeeAny: true, LimitCount: 1000,
	})
	if err != nil {
		t.Fatalf("admin ListTemplates: %v", err)
	}
	got := collect(rows)
	for _, id := range []uuid.UUID{alicePub, alicePriv, bobPriv} {
		if _, ok := got[id]; !ok {
			t.Errorf("admin clamp missed id %v", id)
		}
	}

	// Developer-Alice clamp: caller_can_see_any=false, caller_id=Alice.
	a := alice
	rows, err = s.Queries().ListTemplates(ctx, store.ListTemplatesParams{
		CallerID: &a, LimitCount: 1000,
	})
	if err != nil {
		t.Fatalf("developer-Alice ListTemplates: %v", err)
	}
	got = collect(rows)
	if _, ok := got[bobPriv]; ok {
		t.Errorf("developer-Alice saw bob's private template %v", bobPriv)
	}
	for _, id := range []uuid.UUID{alicePub, alicePriv} {
		if _, ok := got[id]; !ok {
			t.Errorf("developer-Alice missed own/public id %v", id)
		}
	}

	// Viewer clamp: caller_can_see_any=false, caller_id=NULL.
	rows, err = s.Queries().ListTemplates(ctx, store.ListTemplatesParams{LimitCount: 1000})
	if err != nil {
		t.Fatalf("viewer ListTemplates: %v", err)
	}
	got = collect(rows)
	for _, id := range []uuid.UUID{alicePriv, bobPriv} {
		if _, ok := got[id]; ok {
			t.Errorf("viewer clamp leaked private id %v", id)
		}
	}
	if _, ok := got[alicePub]; !ok {
		t.Errorf("viewer clamp missed public id %v", alicePub)
	}
}

func TestTemplatesListFilters(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	alice := seedUser(t, ctx, s, "developer")
	bob := seedUser(t, ctx, s, "developer")

	// Set up a small distinguishable fixture.
	mk := func(owner uuid.UUID, arch store.CPUArch, fam store.OsFamily, name string) uuid.UUID {
		id := uuid.New()
		params := defaultTemplateParams(id, owner, name)
		params.Architecture = arch
		params.OsFamily = fam
		if _, err := s.Queries().CreateTemplate(ctx, params); err != nil {
			t.Fatalf("CreateTemplate %s: %v", name, err)
		}
		return id
	}
	tag := uniqueTemplateName("filter")
	aliceLinux := mk(alice, store.CpuArchAmd64, store.OsFamilyLinux, tag+"-alice-linux")
	bobWin := mk(bob, store.CpuArchArm64, store.OsFamilyWindows, tag+"-bob-win")

	// Filter by owner=Alice.
	a := alice
	rows, err := s.Queries().ListTemplates(ctx, store.ListTemplatesParams{
		CallerCanSeeAny: true,
		OwnerIDFilter:   &a,
		LimitCount:      1000,
	})
	if err != nil {
		t.Fatalf("ListTemplates owner=alice: %v", err)
	}
	for _, r := range rows {
		if r.OwnerID != alice {
			t.Errorf("owner filter leaked: %v != %v", r.OwnerID, alice)
		}
	}
	if !contains(rows, aliceLinux) {
		t.Errorf("owner filter missed aliceLinux %v", aliceLinux)
	}

	// Filter by arch=arm64.
	arm := store.CpuArchArm64
	rows, err = s.Queries().ListTemplates(ctx, store.ListTemplatesParams{
		CallerCanSeeAny:    true,
		ArchitectureFilter: &arm,
		LimitCount:         1000,
	})
	if err != nil {
		t.Fatalf("ListTemplates arch=arm64: %v", err)
	}
	if !contains(rows, bobWin) {
		t.Errorf("arch filter missed bobWin %v", bobWin)
	}
	for _, r := range rows {
		if r.Architecture != store.CpuArchArm64 {
			t.Errorf("arch filter leaked: %v", r.Architecture)
		}
	}

	// Filter by os_family=windows.
	winFam := store.OsFamilyWindows
	rows, err = s.Queries().ListTemplates(ctx, store.ListTemplatesParams{
		CallerCanSeeAny: true,
		OsFamilyFilter:  &winFam,
		LimitCount:      1000,
	})
	if err != nil {
		t.Fatalf("ListTemplates os_family=windows: %v", err)
	}
	for _, r := range rows {
		if r.OsFamily != store.OsFamilyWindows {
			t.Errorf("os_family filter leaked: %v", r.OsFamily)
		}
	}
}

func TestTemplatesListPagination(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "operator")
	tag := uniqueTemplateName("page")
	const n = 5
	created := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		id := uuid.New()
		name := tag + "-" + uuid.NewString()[:6]
		if _, err := s.Queries().CreateTemplate(ctx, defaultTemplateParams(id, owner, name)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		created = append(created, id)
		// Spread created_at so the (created_at, id) cursor terminates correctly.
		time.Sleep(2 * time.Millisecond)
	}

	owners := []uuid.UUID{owner}
	got := make([]uuid.UUID, 0, n)
	var (
		curTS *time.Time
		curID *uuid.UUID
	)
	for page := 0; page < 1000; page++ {
		params := store.ListTemplatesParams{
			CallerCanSeeAny: true,
			OwnerIDFilter:   &owners[0],
			CursorCreatedAt: curTS,
			CursorID:        curID,
			LimitCount:      2,
		}
		rows, err := s.Queries().ListTemplates(ctx, params)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			got = append(got, r.ID)
		}
		last := rows[len(rows)-1]
		ts, id := last.CreatedAt, last.ID
		curTS, curID = &ts, &id
		if len(rows) < 2 {
			break
		}
	}
	if len(got) != n {
		t.Errorf("paginated through %d rows, want %d", len(got), n)
	}
	for i := 0; i < n; i++ {
		if got[i] != created[i] {
			t.Errorf("paginated order mismatch at %d: got %v want %v", i, got[i], created[i])
		}
	}
}

func TestTemplatesCloneCopiesEverythingExceptOwnerNameVisibility(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	source := seedUser(t, ctx, s, "operator")
	cloner := seedUser(t, ctx, s, "developer")

	srcID := uuid.New()
	srcParams := defaultTemplateParams(srcID, source, uniqueTemplateName("src"))
	srcParams.Description = "source desc"
	srcParams.OsVariant = "ubuntu-2404"
	srcParams.DefaultCpuCores = 8
	srcParams.DefaultMemoryMib = 16384
	srcParams.DefaultDiskGib = 100
	srcParams.Metadata = []byte(`{"label":"original"}`)
	if _, err := s.Queries().CreateTemplate(ctx, srcParams); err != nil {
		t.Fatalf("CreateTemplate src: %v", err)
	}
	if _, err := s.Queries().SetTemplateVisibility(ctx, store.SetTemplateVisibilityParams{ID: srcID, Visibility: "public"}); err != nil {
		t.Fatalf("publish src: %v", err)
	}

	cloneName := uniqueTemplateName("clone")
	cloned, err := s.Queries().CloneTemplate(ctx, store.CloneTemplateParams{
		NewID:          uuid.New(),
		NewOwnerID:     cloner,
		NewName:        cloneName,
		NewDescription: nil,
		SourceID:       srcID,
	})
	if err != nil {
		t.Fatalf("CloneTemplate: %v", err)
	}
	if cloned.OwnerID != cloner {
		t.Errorf("OwnerID = %v, want %v", cloned.OwnerID, cloner)
	}
	if cloned.Name != cloneName {
		t.Errorf("Name = %q, want %q", cloned.Name, cloneName)
	}
	if cloned.Visibility != "private" {
		t.Errorf("Visibility = %q, want private (clones always start private)", cloned.Visibility)
	}
	if cloned.Description != srcParams.Description {
		t.Errorf("Description (no override) = %q, want %q", cloned.Description, srcParams.Description)
	}
	if cloned.DefaultCpuCores != 8 || cloned.DefaultMemoryMib != 16384 || cloned.DefaultDiskGib != 100 {
		t.Errorf("sizing not copied verbatim: %+v", cloned)
	}
	if cloned.OsVariant != "ubuntu-2404" {
		t.Errorf("os_variant not copied: %q", cloned.OsVariant)
	}
	// Postgres re-formats jsonb on output (whitespace, key order). Compare
	// semantically rather than byte-for-byte.
	if !sameJSON(t, cloned.Metadata, []byte(`{"label":"original"}`)) {
		t.Errorf("metadata not copied: %s", string(cloned.Metadata))
	}

	// Description override
	overrideName := uniqueTemplateName("clone-override")
	override := "new desc"
	cloned2, err := s.Queries().CloneTemplate(ctx, store.CloneTemplateParams{
		NewID:          uuid.New(),
		NewOwnerID:     cloner,
		NewName:        overrideName,
		NewDescription: &override,
		SourceID:       srcID,
	})
	if err != nil {
		t.Fatalf("CloneTemplate override: %v", err)
	}
	if cloned2.Description != override {
		t.Errorf("Description override = %q, want %q", cloned2.Description, override)
	}
}

func TestTemplatesCloneMissingSourceReturnsNoRows(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	cloner := seedUser(t, ctx, s, "developer")

	_, err := s.Queries().CloneTemplate(ctx, store.CloneTemplateParams{
		NewID:      uuid.New(),
		NewOwnerID: cloner,
		NewName:    uniqueTemplateName("ghost"),
		SourceID:   uuid.New(),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("clone of nonexistent source err = %v, want pgx.ErrNoRows", err)
	}
}

func TestTemplatesCountActiveVMsForTemplate(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	tplID := uuid.New()
	if _, err := s.Queries().CreateTemplate(ctx, defaultTemplateParams(tplID, owner, uniqueTemplateName("vm-ref"))); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	// No VMs yet → count is zero.
	got, err := s.Queries().CountActiveVMsForTemplate(ctx, tplID)
	if err != nil {
		t.Fatalf("CountActiveVMsForTemplate (none): %v", err)
	}
	if got != 0 {
		t.Errorf("CountActiveVMsForTemplate = %d, want 0", got)
	}

	// Insert two active VMs and one soft-deleted VM via the pool
	// directly — vms has no first-class sqlc CRUD yet (VMs handler is
	// further down the roadmap). Soft-deleted rows must NOT contribute
	// to the precheck count.
	for i := 0; i < 2; i++ {
		if err := insertActiveVM(ctx, s, owner, &tplID, uniqueTemplateName("vm")); err != nil {
			t.Fatalf("seed active vm %d: %v", i, err)
		}
	}
	if err := insertSoftDeletedVM(ctx, s, owner, &tplID, uniqueTemplateName("vm-deleted")); err != nil {
		t.Fatalf("seed soft-deleted vm: %v", err)
	}

	got, err = s.Queries().CountActiveVMsForTemplate(ctx, tplID)
	if err != nil {
		t.Fatalf("CountActiveVMsForTemplate: %v", err)
	}
	if got != 2 {
		t.Errorf("CountActiveVMsForTemplate = %d, want 2 (soft-deleted excluded)", got)
	}
}

// insertActiveVM inserts a VM row directly via the pool; the project
// does not have a CreateVM sqlc query yet (VMs handler is on the
// roadmap). The columns map to chk_vms_cpu / chk_vms_memory.
func insertActiveVM(ctx context.Context, s *store.Store, owner uuid.UUID, tplID *uuid.UUID, name string) error {
	_, err := s.Pool().Exec(ctx, `
		insert into vms (id, owner_id, name, template_id, architecture,
		  cpu_cores, memory_mib, machine_type)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
	`, uuid.New(), owner, name, tplID, store.CpuArchAmd64, 1, 1024, "q35")
	return err
}

// insertSoftDeletedVM inserts a VM row already marked deleted_at, so
// the active-count precheck must skip it.
func insertSoftDeletedVM(ctx context.Context, s *store.Store, owner uuid.UUID, tplID *uuid.UUID, name string) error {
	_, err := s.Pool().Exec(ctx, `
		insert into vms (id, owner_id, name, template_id, architecture,
		  cpu_cores, memory_mib, machine_type, deleted_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, now())
	`, uuid.New(), owner, name, tplID, store.CpuArchAmd64, 1, 1024, "q35")
	return err
}

func contains(rows []store.Template, id uuid.UUID) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// sameJSON compares two JSON byte slices structurally — Postgres
// returns jsonb with normalised whitespace and key ordering, so a
// byte-for-byte compare against the input is fragile.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("sameJSON unmarshal a: %v", err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("sameJSON unmarshal b: %v", err)
	}
	return reflect.DeepEqual(x, y)
}
