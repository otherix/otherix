// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// TestValidateCreateRequest covers the field-level invariants the
// API edge enforces before any DB work. Each row exercises one
// rejection branch; happy-path vetting lives in the firmware /
// schedule tests that need a store fake.
func TestValidateCreateRequest(t *testing.T) {
	t.Parallel()

	validSHA := strings.Repeat("ab", 32) // 64 chars lowercase hex
	base := func() vmCreateRequest {
		return vmCreateRequest{
			Name:         "demo",
			ImageURL:     "https://example.test/img.qcow2",
			Architecture: "amd64",
			Pool:         "default",
			VCPUs:        2,
			MemoryMB:     2048,
		}
	}

	cases := []struct {
		name string
		req  vmCreateRequest
		ok   bool
	}{
		{name: "happy path", req: base(), ok: true},
		{
			name: "happy path with valid sha + format + disk",
			req: func() vmCreateRequest {
				r := base()
				r.ImageSHA256 = validSHA
				r.Format = "qcow2"
				r.DiskGiB = 20
				return r
			}(),
			ok: true,
		},
		{name: "empty name", req: func() vmCreateRequest { r := base(); r.Name = ""; return r }()},
		{
			name: "name too long",
			req:  func() vmCreateRequest { r := base(); r.Name = strings.Repeat("a", 64); return r }(),
		},
		{
			// Audit LOW: the name feeds the cidata local-hostname /
			// instance-id and the console-stream URL path, so it must be a
			// lowercase RFC 1123 DNS label.
			name: "name with uppercase and underscore",
			req:  func() vmCreateRequest { r := base(); r.Name = "Bad_Name"; return r }(),
		},
		{
			name: "name with slash",
			req:  func() vmCreateRequest { r := base(); r.Name = "a/b"; return r }(),
		},
		{
			name: "name with leading hyphen",
			req:  func() vmCreateRequest { r := base(); r.Name = "-vm"; return r }(),
		},
		{name: "empty image_url", req: func() vmCreateRequest { r := base(); r.ImageURL = ""; return r }()},
		{
			// SSRF guard: only absolute https URLs are admitted, so
			// a vm:create holder cannot point the agent at http://169.254.169.254
			// or any other plaintext/internal scheme.
			name: "http image_url",
			req:  func() vmCreateRequest { r := base(); r.ImageURL = "http://169.254.169.254/latest/meta-data/"; return r }(),
		},
		{name: "file image_url", req: func() vmCreateRequest { r := base(); r.ImageURL = "file:///etc/passwd"; return r }()},
		{name: "relative image_url", req: func() vmCreateRequest { r := base(); r.ImageURL = "/images/img.qcow2"; return r }()},
		{name: "missing arch", req: func() vmCreateRequest { r := base(); r.Architecture = ""; return r }()},
		{name: "bad arch", req: func() vmCreateRequest { r := base(); r.Architecture = "riscv"; return r }()},
		{name: "arm64 ok", req: func() vmCreateRequest { r := base(); r.Architecture = "arm64"; return r }(), ok: true},
		{name: "bad sha (short)", req: func() vmCreateRequest { r := base(); r.ImageSHA256 = "abcd"; return r }()},
		{name: "bad sha (uppercase)", req: func() vmCreateRequest { r := base(); r.ImageSHA256 = strings.ToUpper(validSHA); return r }()},
		{name: "bad format", req: func() vmCreateRequest { r := base(); r.Format = "vmdk"; return r }()},
		{name: "negative disk", req: func() vmCreateRequest { r := base(); r.DiskGiB = -1; return r }()},
		{
			// Empty pool is admitted - the handler substitutes the cluster
			// default at runtime, or returns 400 default_pool_not_set.
			name: "empty pool ok (cluster default fallback)",
			req:  func() vmCreateRequest { r := base(); r.Pool = ""; return r }(),
			ok:   true,
		},
		{name: "vcpus too small", req: func() vmCreateRequest { r := base(); r.VCPUs = 0; return r }()},
		{name: "vcpus too large", req: func() vmCreateRequest { r := base(); r.VCPUs = 129; return r }()},
		{name: "memory too small", req: func() vmCreateRequest { r := base(); r.MemoryMB = 64; return r }()},
		{name: "memory too large", req: func() vmCreateRequest { r := base(); r.MemoryMB = 1 << 30; return r }()},
		{
			// A node hint is a node name; a uuid literal is rejected at
			// admission so it never becomes a permanently-pending VM.
			name: "node hint is uuid",
			req:  func() vmCreateRequest { r := base(); s := uuid.New().String(); r.Node = &s; return r }(),
		},
		{
			name: "node hint is name ok",
			req:  func() vmCreateRequest { r := base(); s := "node-1"; r.Node = &s; return r }(),
			ok:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &captureWriter{}
			got := validateCreateRequest(rec, fakeRequest(), tc.req)
			if got != tc.ok {
				t.Errorf("validateCreateRequest(%s) = %v, want %v", tc.name, got, tc.ok)
			}
		})
	}
}

// TestValidateCreateRequestInvalidNameEnvelope pins the wire contract
// for the VM name rule: an invalid name yields 400 with the
// validation_failed code.
func TestValidateCreateRequestInvalidNameEnvelope(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := vmCreateRequest{
		Name:         "Bad_Name",
		ImageURL:     "https://example.test/img.qcow2",
		Architecture: "amd64",
		Pool:         "default",
		VCPUs:        2,
		MemoryMB:     2048,
	}
	if got := validateCreateRequest(rec, fakeRequest(), req); got {
		t.Fatal("validateCreateRequest(Bad_Name) = true, want false")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != "validation_failed" {
		t.Errorf("error.code = %q, want %q", body.Error.Code, "validation_failed")
	}
}

// firmwareStoreStub satisfies the handler's Store interface for the
// resolveFirmware unit tests. It embeds Store (nil) so only the two firmware
// methods exercised here have bodies; any other call panics.
type firmwareStoreStub struct {
	Store
	byID      map[uuid.UUID]store.Firmware
	byArch    map[store.FirmwareType]store.Firmware
	byIDErr   error
	byArchErr error
}

func (s *firmwareStoreStub) FirmwareByID(_ context.Context, id uuid.UUID) (store.Firmware, error) {
	if s.byIDErr != nil {
		return store.Firmware{}, s.byIDErr
	}
	fw, ok := s.byID[id]
	if !ok {
		return store.Firmware{}, store.ErrNotFound
	}
	return fw, nil
}

func (s *firmwareStoreStub) DefaultFirmwareForArchType(_ context.Context, _ store.CPUArch, ftype store.FirmwareType) (store.Firmware, error) {
	if s.byArchErr != nil {
		return store.Firmware{}, s.byArchErr
	}
	fw, ok := s.byArch[ftype]
	if !ok {
		return store.Firmware{}, store.ErrNotFound
	}
	return fw, nil
}

// networkStoreStub satisfies the handler's Store interface for the
// resolveNetworkName default-fallback test. It embeds Store (nil) so only
// ClusterSettings has a body; any other call panics.
type networkStoreStub struct {
	Store
	settings store.ClusterSetting
}

func (s *networkStoreStub) ClusterSettings(context.Context) (store.ClusterSetting, error) {
	return s.settings, nil
}

// TestResolveNetworkNameDefaultFallback covers the empty-network branch: with a
// cluster default network configured, an omitted `network` binds to that
// default; with none configured, the VM gets no NIC.
func TestResolveNetworkNameDefaultFallback(t *testing.T) {
	t.Parallel()

	defaultName := "qnet"
	cases := []struct {
		name     string
		settings store.ClusterSetting
		want     string
	}{
		{
			name:     "default network set falls through",
			settings: store.ClusterSetting{DefaultNetworkName: &defaultName},
			want:     defaultName,
		},
		{
			name:     "no default network yields no NIC",
			settings: store.ClusterSetting{DefaultNetworkName: nil},
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &Handler{store: &networkStoreStub{settings: tc.settings}, log: discardLog()}
			rec := httptest.NewRecorder()
			got, ok := h.resolveNetworkName(rec, fakeRequest(), "")
			if !ok {
				t.Fatalf("resolveNetworkName(\"\") ok = false, want true (body: %s)", rec.Body.String())
			}
			if got != tc.want {
				t.Errorf("resolveNetworkName(\"\") = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveFirmware covers the firmware-resolution truth table: an explicit
// firmware_id wins; otherwise the default for (arch, type) where type defaults
// to uefi unless firmware=="bios"; bad inputs surface typed sentinels.
func TestResolveFirmware(t *testing.T) {
	t.Parallel()

	explicitID := uuid.New()
	uefiID := uuid.New()
	biosID := uuid.New()
	st := &firmwareStoreStub{
		byID: map[uuid.UUID]store.Firmware{explicitID: {ID: explicitID}},
		byArch: map[store.FirmwareType]store.Firmware{
			store.FirmwareTypeUefi: {ID: uefiID},
			store.FirmwareTypeBios: {ID: biosID},
		},
	}
	h := &Handler{store: st, log: discardLog()}

	cases := []struct {
		name       string
		firmware   string
		firmwareID string
		wantID     uuid.UUID
		wantErr    error
	}{
		{name: "explicit id wins", firmwareID: explicitID.String(), wantID: explicitID},
		{name: "default uefi when firmware empty", wantID: uefiID},
		{name: "explicit uefi", firmware: "uefi", wantID: uefiID},
		{name: "explicit bios", firmware: "bios", wantID: biosID},
		{name: "bad uuid", firmwareID: "not-a-uuid", wantErr: errFirmwareBadID},
		{name: "bad firmware type", firmware: "coreboot", wantErr: errFirmwareBadType},
		{name: "explicit id missing", firmwareID: uuid.New().String(), wantErr: errFirmwareNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := h.resolveFirmware(context.Background(), store.CpuArchAmd64, tc.firmware, tc.firmwareID)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("resolveFirmware err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveFirmware err = %v, want nil", err)
			}
			if got == nil || *got != tc.wantID {
				t.Errorf("resolveFirmware id = %v, want %s", got, tc.wantID)
			}
		})
	}
}

// TestResolveFirmwareNoDefault pins the no-default-for-arch/type path: firmware
// is optional, so with no seeded default the resolver returns (nil, nil) and the
// VM is created without a firmware row (boots with the agent/qemu default).
func TestResolveFirmwareNoDefault(t *testing.T) {
	t.Parallel()
	st := &firmwareStoreStub{byArch: map[store.FirmwareType]store.Firmware{}}
	h := &Handler{store: st, log: discardLog()}
	got, err := h.resolveFirmware(context.Background(), store.CpuArchArm64, "", "")
	if err != nil {
		t.Errorf("resolveFirmware err = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("resolveFirmware id = %v, want nil (no firmware)", got)
	}
}

// TestGenerateLocalMAC asserts the minted MAC carries the QEMU 52:54:00 OUI,
// is locally administered + unicast, and varies across calls.
func TestGenerateLocalMAC(t *testing.T) {
	mac, err := generateLocalMAC()
	if err != nil {
		t.Fatalf("generateLocalMAC: %v", err)
	}
	if len(mac) != 6 {
		t.Fatalf("len(mac) = %d, want 6", len(mac))
	}
	if mac[0] != 0x52 || mac[1] != 0x54 || mac[2] != 0x00 {
		t.Errorf("mac OUI = %02x:%02x:%02x, want 52:54:00", mac[0], mac[1], mac[2])
	}
	// Locally administered (bit 1 set) + unicast (bit 0 clear) in the first octet.
	if mac[0]&0x02 == 0 {
		t.Errorf("mac first octet %02x is not locally administered", mac[0])
	}
	if mac[0]&0x01 != 0 {
		t.Errorf("mac first octet %02x is not unicast", mac[0])
	}
	other, err := generateLocalMAC()
	if err != nil {
		t.Fatalf("generateLocalMAC (2nd): %v", err)
	}
	if mac.String() == other.String() {
		t.Errorf("two generated MACs collided: %s", mac)
	}
}
