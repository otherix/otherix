// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
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

// sshUserDataStoreStub satisfies the handler's Store interface for the
// SSH-ingress user-data resolution tests. It embeds Store (nil) so only
// ClusterSettings and ActiveSSHUserCA have bodies; any other call panics.
type sshUserDataStoreStub struct {
	Store
	settings store.ClusterSetting
	ca       store.SSHUserCA
	caErr    error
}

func (s *sshUserDataStoreStub) ClusterSettings(context.Context) (store.ClusterSetting, error) {
	return s.settings, nil
}

func (s *sshUserDataStoreStub) ActiveSSHUserCA(context.Context) (store.SSHUserCA, error) {
	return s.ca, s.caErr
}

// testCAAuthorizedKey is a sample CA public key in authorized_keys form, the
// shape ActiveSSHUserCA().PublicKeyAuthorized carries.
const testCAAuthorizedKey = "ecdsa-sha2-nistp384 AAAAE2VjZHNhLXNoYTItbmlzdHAzODQAAAAIbmlzdHAzODQAAABhBHcluster-ca otherix-cluster-ca"

// TestResolveCreateUserData_SSHCACoexistence is the M5 assertion: an operator
// `#cloud-config` with a `users:` block MUST coexist with the injected
// `TrustedUserCAKeys` CA-trust part. The result is valid multipart MIME and
// neither document clobbers the other.
func TestResolveCreateUserData_SSHCACoexistence(t *testing.T) {
	t.Parallel()

	operator := "#cloud-config\nusers:\n  - name: operator-user\n    sudo: ALL=(ALL) NOPASSWD:ALL\n"
	h := &Handler{store: &sshUserDataStoreStub{
		settings: store.ClusterSetting{SSHIngressEnabled: true},
		ca:       store.SSHUserCA{PublicKeyAuthorized: []byte(testCAAuthorizedKey)},
	}, log: discardLog()}

	req := vmCreateRequest{UserData: &operator, SSHIngressEnabled: true}
	rec := httptest.NewRecorder()
	got, ok := h.resolveCreateUserData(rec, fakeRequest(), req)
	if !ok {
		t.Fatalf("resolveCreateUserData ok = false, want true (body: %s)", rec.Body.String())
	}
	if got == nil {
		t.Fatal("resolveCreateUserData returned nil user-data, want multipart MIME")
	}

	parts := parseMultipartUserData(t, *got)
	if len(parts) != 2 {
		t.Fatalf("multipart parts = %d, want 2 (operator + CA-trust)", len(parts))
	}
	joined := strings.Join(parts, "\n----\n")
	if !strings.Contains(joined, "name: operator-user") {
		t.Errorf("operator users: block was clobbered; parts:\n%s", joined)
	}
	if !strings.Contains(joined, "TrustedUserCAKeys") {
		t.Errorf("injected TrustedUserCAKeys missing; parts:\n%s", joined)
	}
	if !strings.Contains(joined, testCAAuthorizedKey) {
		t.Errorf("CA public key missing from CA-trust part; parts:\n%s", joined)
	}
	// The CA-trust part must NOT add login users (the guest sshd owns login
	// policy): the only `users:` key present is the operator's own.
	if strings.Count(joined, "users:") != 1 {
		t.Errorf("expected exactly one users: block (operator's own), got %d; parts:\n%s",
			strings.Count(joined, "users:"), joined)
	}
}

// TestResolveCreateUserData_OperatorMIME is the B2 assertion: when the
// operator's OWN user-data is already a multipart-MIME archive, the SSH-ingress
// assembly must NOT mislabel it as an opaque text/cloud-config body (which makes
// cloud-init feed the operator's Content-Type/boundary header lines to the YAML
// parser, silently dropping the operator's payload). The operator's nested
// text/cloud-config `users:` leaf must remain reachable through a MIME walk, and
// the injected TrustedUserCAKeys must coexist.
func TestResolveCreateUserData_OperatorMIME(t *testing.T) {
	t.Parallel()

	// An operator-hand-crafted multipart-MIME user-data: one text/cloud-config
	// part with a users: block.
	operator := buildOperatorMultipart(t, "#cloud-config\nusers:\n  - name: operator-user\n")
	h := &Handler{store: &sshUserDataStoreStub{
		settings: store.ClusterSetting{SSHIngressEnabled: true},
		ca:       store.SSHUserCA{PublicKeyAuthorized: []byte(testCAAuthorizedKey)},
	}, log: discardLog()}

	req := vmCreateRequest{UserData: &operator, SSHIngressEnabled: true}
	rec := httptest.NewRecorder()
	got, ok := h.resolveCreateUserData(rec, fakeRequest(), req)
	if !ok {
		t.Fatalf("resolveCreateUserData ok = false, want true (body: %s)", rec.Body.String())
	}
	if got == nil {
		t.Fatal("resolveCreateUserData returned nil user-data, want multipart MIME")
	}

	leaves := walkMIMELeaves(t, *got)

	var sawOperatorUsers, sawCATrust bool
	for _, leaf := range leaves {
		if strings.Contains(leaf.body, "name: operator-user") {
			sawOperatorUsers = true
			// The operator's cloud-config must surface as a cloud-config leaf, not
			// be buried inside a body that still carries raw MIME header lines.
			if !strings.HasPrefix(leaf.mediaType, "text/cloud-config") {
				t.Errorf("operator users: leaf media type = %q, want text/cloud-config", leaf.mediaType)
			}
			if strings.Contains(leaf.body, "Content-Type:") || strings.Contains(leaf.body, "boundary=") {
				t.Errorf("operator MIME header lines leaked into a cloud-config body:\n%s", leaf.body)
			}
		}
		if strings.Contains(leaf.body, "TrustedUserCAKeys") {
			sawCATrust = true
		}
	}
	if !sawOperatorUsers {
		t.Errorf("operator users: block not reachable through MIME walk; leaves: %+v", leaves)
	}
	if !sawCATrust {
		t.Errorf("injected TrustedUserCAKeys not reachable through MIME walk; leaves: %+v", leaves)
	}
}

// buildOperatorMultipart wraps a single cloud-config document into a
// multipart/mixed MIME archive shaped the way a real operator user-data would
// arrive (leading Content-Type / MIME-Version headers).
func buildOperatorMultipart(t *testing.T, cloudConfig string) string {
	t.Helper()
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	pw, err := mw.CreatePart(map[string][]string{
		"Content-Type": {"text/cloud-config; charset=\"utf-8\""},
		"MIME-Version": {"1.0"},
	})
	if err != nil {
		t.Fatalf("create operator part: %v", err)
	}
	if _, err := io.WriteString(pw, cloudConfig); err != nil {
		t.Fatalf("write operator part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close operator writer: %v", err)
	}
	return "Content-Type: multipart/mixed; boundary=\"" + mw.Boundary() + "\"\n" +
		"MIME-Version: 1.0\n\n" + body.String()
}

// mimeLeaf is a non-multipart MIME part collected by walkMIMELeaves.
type mimeLeaf struct {
	mediaType string
	body      string
}

// walkMIMELeaves recursively descends a NoCloud MIME user-data blob and returns
// every non-multipart leaf part (media type + decoded body), so a test can
// assert what cloud-init would ultimately process.
func walkMIMELeaves(t *testing.T, userData string) []mimeLeaf {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(userData))
	if err != nil {
		t.Fatalf("parse user-data MIME headers: %v\n%s", err, userData)
	}
	return walkMIMEReader(t, msg.Header.Get("Content-Type"), msg.Body)
}

func walkMIMEReader(t *testing.T, contentType string, r io.Reader) []mimeLeaf {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("parse Content-Type %q: %v", contentType, err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read leaf body: %v", err)
		}
		return []mimeLeaf{{mediaType: mediaType, body: string(b)}}
	}
	var leaves []mimeLeaf
	mr := multipart.NewReader(r, params["boundary"])
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read multipart part: %v", err)
		}
		leaves = append(leaves, walkMIMEReader(t, p.Header.Get("Content-Type"), p)...)
	}
	return leaves
}

// TestResolveCreateUserData_Passthrough covers the fail-open paths: when SSH
// ingress is not requested, not enabled cluster-wide, or the CA is absent, the
// operator user-data is returned unchanged (no multipart wrapping).
func TestResolveCreateUserData_Passthrough(t *testing.T) {
	t.Parallel()

	operator := "#cloud-config\nusers:\n  - name: operator-user\n"
	cases := []struct {
		name string
		req  vmCreateRequest
		stub *sshUserDataStoreStub
	}{
		{
			name: "per-vm opt-in absent",
			req:  vmCreateRequest{UserData: &operator, SSHIngressEnabled: false},
			stub: &sshUserDataStoreStub{
				settings: store.ClusterSetting{SSHIngressEnabled: true},
				ca:       store.SSHUserCA{PublicKeyAuthorized: []byte(testCAAuthorizedKey)},
			},
		},
		{
			name: "cluster setting disabled",
			req:  vmCreateRequest{UserData: &operator, SSHIngressEnabled: true},
			stub: &sshUserDataStoreStub{
				settings: store.ClusterSetting{SSHIngressEnabled: false},
				ca:       store.SSHUserCA{PublicKeyAuthorized: []byte(testCAAuthorizedKey)},
			},
		},
		{
			name: "ca absent",
			req:  vmCreateRequest{UserData: &operator, SSHIngressEnabled: true},
			stub: &sshUserDataStoreStub{
				settings: store.ClusterSetting{SSHIngressEnabled: true},
				caErr:    store.ErrNotFound,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &Handler{store: tc.stub, log: discardLog()}
			rec := httptest.NewRecorder()
			got, ok := h.resolveCreateUserData(rec, fakeRequest(), tc.req)
			if !ok {
				t.Fatalf("resolveCreateUserData ok = false, want true (body: %s)", rec.Body.String())
			}
			if got == nil || *got != operator {
				t.Errorf("resolveCreateUserData = %v, want operator user-data unchanged", got)
			}
		})
	}
}

// TestBuildSSHCATrustUserDataNoOperatorDoc covers the no-operator-user-data
// branch: a single `#cloud-config` CA-trust document (no multipart wrapper
// needed) that still carries the CA public key and TrustedUserCAKeys drop-in.
func TestBuildSSHCATrustUserDataNoOperatorDoc(t *testing.T) {
	t.Parallel()

	got, err := buildSSHCATrustUserData("", []byte(testCAAuthorizedKey))
	if err != nil {
		t.Fatalf("buildSSHCATrustUserData: %v", err)
	}
	if !strings.HasPrefix(got, "#cloud-config") {
		t.Errorf("single-doc output should start with #cloud-config, got:\n%s", got)
	}
	if !strings.Contains(got, "TrustedUserCAKeys") || !strings.Contains(got, testCAAuthorizedKey) {
		t.Errorf("CA-trust doc missing TrustedUserCAKeys / CA key:\n%s", got)
	}
}

// parseMultipartUserData parses a NoCloud multipart-MIME user-data blob and
// returns each part's decoded body, failing the test on a malformed envelope.
func parseMultipartUserData(t *testing.T, userData string) []string {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(userData))
	if err != nil {
		t.Fatalf("parse user-data MIME headers: %v\n%s", err, userData)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse Content-Type: %v", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		t.Fatalf("top-level media type = %q, want multipart/*", mediaType)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var bodies []string
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read multipart part: %v", err)
		}
		b, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("read part body: %v", err)
		}
		bodies = append(bodies, string(b))
	}
	return bodies
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
