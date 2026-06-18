// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"testing"

	"github.com/google/uuid"
)

func TestSnapshotKeySchema(t *testing.T) {
	sid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	vid := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	oid := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	if got, want := snapshotKey(sid), "/otherix/snapshots/"+sid.String(); got != want {
		t.Errorf("snapshotKey = %q, want %q", got, want)
	}
	if got, want := snapshotPrefix(), "/otherix/snapshots/"; got != want {
		t.Errorf("snapshotPrefix = %q, want %q", got, want)
	}
	if got, want := snapshotVMNameGuard(vid, "Daily"), "/otherix/uniq/snapshots/vm_name/"+vid.String()+"/daily"; got != want {
		t.Errorf("snapshotVMNameGuard = %q, want %q", got, want)
	}
	if got, want := snapshotOwnerIndexKey(oid, sid), "/otherix/index/snapshots/owner/"+oid.String()+"/"+sid.String(); got != want {
		t.Errorf("snapshotOwnerIndexKey = %q, want %q", got, want)
	}
	if got, want := snapshotVMIndexKey(vid, sid), "/otherix/index/snapshots/vm/"+vid.String()+"/"+sid.String(); got != want {
		t.Errorf("snapshotVMIndexKey = %q, want %q", got, want)
	}
	if got, want := snapshotVMIndexPrefix(vid), "/otherix/index/snapshots/vm/"+vid.String()+"/"; got != want {
		t.Errorf("snapshotVMIndexPrefix = %q, want %q", got, want)
	}
	if got, want := blobRefKey("abc123", sid), "/otherix/index/blob_refs/abc123/"+sid.String(); got != want {
		t.Errorf("blobRefKey = %q, want %q", got, want)
	}
	if got, want := blobRefPrefix("abc123"), "/otherix/index/blob_refs/abc123/"; got != want {
		t.Errorf("blobRefPrefix = %q, want %q", got, want)
	}
	if got, want := placementKey("abc123", vid), "/otherix/placement/abc123/"+vid.String(); got != want {
		t.Errorf("placementKey = %q, want %q", got, want)
	}
	if got, want := placementPrefix("abc123"), "/otherix/placement/abc123/"; got != want {
		t.Errorf("placementPrefix = %q, want %q", got, want)
	}
}
