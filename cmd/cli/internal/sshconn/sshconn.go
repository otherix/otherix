// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package sshconn is the shared client-side SSH-ingress connector used by
// both the operator `otherix ssh` command and the thin external
// `otherix-ssh` binary. It owns three operations against a VM's SSH-ingress
// surface on the Control Plane:
//
//   - EnsureGuestCert generates a per-client ed25519 keypair (the private key
//     never leaves the machine), mints a short-lived guest certificate from
//     POST /v1/vms/{vm}/ssh-cert, and caches the cert for reuse until it nears
//     expiry.
//   - Proxy dials the GET /v1/vms/{vm}/ssh-stream WebSocket and splices it to
//     a supplied stdin/stdout pair. This is the body of an ssh ProxyCommand.
//   - WriteSSHConfigBlock writes a marker-delimited managed ssh_config block so
//     `ssh <name>.<cluster-suffix>` routes transparently through the connector.
//
// The connector is transport- and UX-agnostic: it logs nothing (never the
// bearer token, never key material) and leaves all terminal handling and
// argument parsing to its callers.
package sshconn

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// Config holds the connection parameters shared by every connector operation.
// A zero KnownDir resolves to ~/.otherix/ssh.
type Config struct {
	// ServerURL is the Control Plane root (e.g. https://cp.example:8443).
	ServerURL string

	// CAFingerprint, when set, pins the CP's presented TLS leaf by its hex
	// sha256(cert.Raw). An empty value trusts the system root store (public
	// certificate). A leading "sha256:" or "pin:" and any colons are
	// tolerated and stripped.
	CAFingerprint string

	// BearerToken authenticates to the CP: a CLI token (JWT or otx_ API
	// token) or an otx_sshgrant_ grant token. It is sent only in the
	// Authorization header and is never logged.
	BearerToken string

	// KnownDir is the directory holding the cached keypair, certificate, and
	// managed ssh_config fragment. Empty resolves to ~/.otherix/ssh.
	KnownDir string
}

// refreshWindow is how close to expiry a cached guest cert may be before
// EnsureGuestCert re-mints it. The guest cert TTL is single-digit minutes, so
// a 30s skew keeps a connect from racing an expiry.
const refreshWindow = 30 * time.Second

// keyFileName / certFileName are the cached identity filenames inside KnownDir.
// One keypair is reused across VMs (the guest cert, not the key, is the
// per-session credential); the cert is re-minted on login change or expiry.
const (
	keyFileName   = "id_ed25519"
	certFileName  = "id_ed25519-cert.pub"
	sshConfigName = "config"
)

// fetchTimeout bounds the cert-mint round-trip.
const fetchTimeout = 30 * time.Second

// EnsureGuestCert ensures a usable guest certificate exists on disk for login
// on vmName and returns the cached certificate and private-key paths. It
// generates an ed25519 keypair on first use (private key persisted 0600,
// never transmitted), reuses a cached cert while it is valid and matches
// login, and otherwise mints a fresh one via POST {ServerURL}/v1/vms/{vm}/ssh-cert.
func EnsureGuestCert(ctx context.Context, cfg Config, vmName, login string) (certPath, keyPath string, err error) {
	dir, err := cfg.resolveKnownDir()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("sshconn: create known dir: %v", err)
	}
	keyPath = filepath.Join(dir, keyFileName)
	certPath = filepath.Join(dir, certFileName)

	signer, err := loadOrGenerateKey(keyPath)
	if err != nil {
		return "", "", err
	}

	if cachedCertUsable(certPath, login, time.Now()) {
		return certPath, keyPath, nil
	}

	certLine, err := mintCert(ctx, cfg, vmName, login, signer.PublicKey())
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(certPath, certLine, 0o644); err != nil { //nolint:gosec // a public certificate is not secret.
		return "", "", fmt.Errorf("sshconn: write certificate: %v", err)
	}
	return certPath, keyPath, nil
}

// loadOrGenerateKey returns an ssh.Signer for the ed25519 key at path,
// generating and persisting a fresh one (0600) when absent or unparseable.
func loadOrGenerateKey(path string) (ssh.Signer, error) {
	if raw, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: path is the connector's own cache file under the operator-controlled KnownDir, not untrusted input.
		if signer, perr := ssh.ParsePrivateKey(raw); perr == nil {
			return signer, nil
		}
		// Fall through: a corrupt cached key is replaced rather than fatal.
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sshconn: generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, fmt.Errorf("sshconn: marshal key: %v", err)
	}
	if err := os.WriteFile(path, pemEncode(block), 0o600); err != nil {
		return nil, fmt.Errorf("sshconn: write key: %v", err)
	}
	signer, err := ssh.NewSignerFromSigner(priv)
	if err != nil {
		return nil, fmt.Errorf("sshconn: signer: %v", err)
	}
	return signer, nil
}

// cachedCertUsable reports whether the cert at path is a user certificate that
// certifies login and is not within refreshWindow of expiry at now.
func cachedCertUsable(path, login string, now time.Time) bool {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is the connector's own cached cert under the operator-controlled KnownDir.
	if err != nil {
		return false
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(raw)
	if err != nil {
		return false
	}
	cert, ok := pub.(*ssh.Certificate)
	if !ok {
		return false
	}
	wantLogin := login
	if wantLogin == "" {
		wantLogin = "root"
	}
	if !containsPrincipal(cert.ValidPrincipals, wantLogin) {
		return false
	}
	if cert.ValidBefore == ssh.CertTimeInfinity {
		return true
	}
	expiry := time.Unix(int64(cert.ValidBefore), 0) //nolint:gosec // ValidBefore is a server-set near-future Unix second.
	return now.Add(refreshWindow).Before(expiry)
}

// containsPrincipal reports whether login is among the cert principals.
func containsPrincipal(principals []string, login string) bool {
	for _, p := range principals {
		if p == login {
			return true
		}
	}
	return false
}

// certMintRequest / certMintResponse mirror the POST /v1/vms/{vm}/ssh-cert
// wire shapes (request: the public key to certify + desired login; response:
// the minted authorized-keys cert line + the certified login + expiry).
type certMintRequest struct {
	PublicKey string `json:"public_key"`
	Login     string `json:"login,omitempty"`
}

type certMintResponse struct {
	Certificate string `json:"certificate"`
	Login       string `json:"login"`
	ExpiresAt   string `json:"expires_at"`
}

// mintCert posts the public key (authorized-keys line) plus login to the CP's
// cert-mint endpoint and returns the minted certificate line. Only the public
// key crosses the wire; the private key stays on disk.
func mintCert(ctx context.Context, cfg Config, vmName, login string, pub ssh.PublicKey) ([]byte, error) {
	client, err := wsHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	client.Timeout = fetchTimeout

	reqBody, err := json.Marshal(certMintRequest{
		PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
		Login:     login,
	})
	if err != nil {
		return nil, fmt.Errorf("sshconn: marshal cert request: %v", err)
	}
	endpoint := strings.TrimRight(cfg.ServerURL, "/") + "/v1/vms/" + url.PathEscape(vmName) + "/ssh-cert"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(reqBody)))
	if err != nil {
		return nil, fmt.Errorf("sshconn: build cert request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sshconn: mint certificate: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sshconn: mint certificate: HTTP %d", resp.StatusCode)
	}
	var out certMintResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("sshconn: decode cert response: %v", err)
	}
	if strings.TrimSpace(out.Certificate) == "" {
		return nil, errors.New("sshconn: cert response carried an empty certificate")
	}
	if _, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(out.Certificate)); perr != nil {
		return nil, fmt.Errorf("sshconn: cert response is not a valid certificate: %v", perr)
	}
	return ensureTrailingNewline([]byte(out.Certificate)), nil
}

// Proxy dials wss://{ServerURL}/v1/vms/{vm}/ssh-stream with the configured
// bearer and TLS trust, then splices the WebSocket to stdin/stdout. It returns
// when either side closes (stdin EOF, server close, context cancel). This is
// the body of an ssh ProxyCommand: stdin/stdout are the ssh client's pipes and
// the spliced bytes are end-to-end SSH the CP never inspects. port is the
// guest port from the ProxyCommand `%p` token; the relay endpoint targets the
// guest sshd directly, so it is accepted for signature stability and not used
// to address the stream.
func Proxy(ctx context.Context, cfg Config, vmName string, port int, stdin io.Reader, stdout io.Writer) error {
	_ = port
	streamURL, err := streamURL(cfg.ServerURL, vmName)
	if err != nil {
		return err
	}
	client, err := wsHTTPClient(cfg)
	if err != nil {
		return err
	}
	hdr := http.Header{}
	if cfg.BearerToken != "" {
		hdr.Set("Authorization", "Bearer "+cfg.BearerToken)
	}
	conn, _, err := websocket.Dial(ctx, streamURL, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: hdr,
	})
	if err != nil {
		return fmt.Errorf("sshconn: dial ssh-stream: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	relayCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	nc := websocket.NetConn(relayCtx, conn, websocket.MessageBinary)

	// The guest->client direction is authoritative for session lifetime: it
	// ends when the guest sshd closes the stream (clean EOF) or when the local
	// stdout pipe breaks because the ssh client exited. The client->guest copy
	// runs in the background; when stdin reaches EOF it stops on its own and
	// the deferred Close tears the WebSocket down. We do not close from the
	// stdin goroutine so an in-flight guest->client frame is never dropped.
	go func() { _, _ = io.Copy(nc, stdin) }()

	_, copyErr := io.Copy(stdout, nc)
	cancel()
	if copyErr != nil && !errors.Is(copyErr, io.EOF) && !errors.Is(copyErr, context.Canceled) {
		return fmt.Errorf("sshconn: ssh-stream relay: %v", copyErr)
	}
	return nil
}

// WriteSSHConfigBlock writes (or replaces) a managed ssh_config block in
// {KnownDir}/config so `ssh <name>.<clusterSuffix>` routes through the
// connector. The block is delimited by `# >>> otherix-ssh <suffix>` /
// `# <<< otherix-ssh <suffix>` markers and carries a wildcard `Host
// *.<suffix>` entry with `ProxyCommand <connectorPath> proxy %h %p` plus the
// cached identity and a managed known_hosts. Re-writing replaces the existing
// block in place, so the operation is idempotent. The managed file is intended
// to be pulled into ~/.ssh/config via an `Include` line so the connector never
// edits the operator's own config.
func WriteSSHConfigBlock(cfg Config, clusterSuffix, connectorPath string) error {
	dir, err := cfg.resolveKnownDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sshconn: create known dir: %v", err)
	}
	path := filepath.Join(dir, sshConfigName)

	block := renderSSHConfigBlock(dir, clusterSuffix, connectorPath)

	existing, err := os.ReadFile(path) //nolint:gosec // G304: path is the connector's own managed ssh_config fragment under the operator-controlled KnownDir.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sshconn: read ssh config: %v", err)
	}
	merged := replaceManagedBlock(string(existing), clusterSuffix, block)
	if err := os.WriteFile(path, []byte(merged), 0o600); err != nil {
		return fmt.Errorf("sshconn: write ssh config: %v", err)
	}
	return nil
}

// renderSSHConfigBlock builds the marker-delimited managed block for suffix.
func renderSSHConfigBlock(dir, suffix, connectorPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# >>> otherix-ssh %s\n", suffix)
	fmt.Fprintf(&b, "Host *.%s\n", suffix)
	fmt.Fprintf(&b, "    ProxyCommand %s proxy %%h %%p\n", connectorPath)
	fmt.Fprintf(&b, "    IdentityFile %s\n", filepath.Join(dir, keyFileName))
	fmt.Fprintf(&b, "    CertificateFile %s\n", filepath.Join(dir, certFileName))
	fmt.Fprintf(&b, "    UserKnownHostsFile %s\n", filepath.Join(dir, "known_hosts"))
	fmt.Fprintf(&b, "    StrictHostKeyChecking accept-new\n")
	fmt.Fprintf(&b, "# <<< otherix-ssh %s\n", suffix)
	return b.String()
}

// replaceManagedBlock removes any existing managed block for suffix from
// existing and appends the new block, returning the merged file content. A
// missing block is simply appended.
func replaceManagedBlock(existing, suffix, block string) string {
	open := fmt.Sprintf("# >>> otherix-ssh %s", suffix)
	closeMarker := fmt.Sprintf("# <<< otherix-ssh %s", suffix)

	lines := strings.Split(existing, "\n")
	var kept []string
	inBlock := false
	for _, line := range lines {
		switch {
		case strings.TrimSpace(line) == open:
			inBlock = true
		case inBlock && strings.TrimSpace(line) == closeMarker:
			inBlock = false
		case !inBlock:
			kept = append(kept, line)
		}
	}
	head := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if head == "" {
		return block
	}
	return head + "\n\n" + block
}

// resolveKnownDir returns KnownDir or the ~/.otherix/ssh default.
func (c Config) resolveKnownDir() (string, error) {
	if c.KnownDir != "" {
		return c.KnownDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("sshconn: resolve home dir: %v", err)
	}
	return filepath.Join(home, ".otherix", "ssh"), nil
}

// wsHTTPClient builds an *http.Client whose TLS trust honours cfg: the system
// root store when CAFingerprint is empty, otherwise a leaf-pin that verifies
// the presented certificate's sha256(cert.Raw) against the pinned fingerprint
// in constant time and rejects any mismatch.
func wsHTTPClient(cfg Config) (*http.Client, error) {
	tlsCfg, err := pinnedTLSConfig(cfg.CAFingerprint)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	return &http.Client{Transport: tr}, nil
}

// pinnedTLSConfig returns a TLS config implementing the trust discriminator:
// an empty fingerprint uses the default root store; a set fingerprint pins the
// presented leaf by sha256(cert.Raw) (constant-time compare) with normal chain
// verification disabled, since the CP leaf is signed by the cluster CA and
// does not chain to a public root.
func pinnedTLSConfig(fingerprint string) (*tls.Config, error) {
	want := normalizeFingerprint(fingerprint)
	if want == "" {
		return &tls.Config{MinVersion: tls.VersionTLS12}, nil
	}
	if _, err := hex.DecodeString(want); err != nil || len(want) != 64 {
		return nil, fmt.Errorf("sshconn: invalid CA fingerprint (want 64 hex chars): %q", fingerprint)
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // leaf is pinned by fingerprint in VerifyConnection below.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("sshconn: server presented no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			got := hex.EncodeToString(sum[:])
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				return errors.New("sshconn: server certificate fingerprint does not match the pinned value")
			}
			return nil
		},
	}, nil
}

// normalizeFingerprint lowercases fingerprint and strips a "sha256:"/"pin:"
// prefix, whitespace, and any colons, yielding bare hex.
func normalizeFingerprint(fp string) string {
	s := strings.ToLower(strings.TrimSpace(fp))
	s = strings.TrimPrefix(s, "pin:")
	s = strings.TrimPrefix(s, "sha256:")
	s = strings.ReplaceAll(s, ":", "")
	return strings.TrimSpace(s)
}

// streamURL builds the ssh-stream WebSocket URL from the CP base URL, mapping
// http->ws and https->wss.
func streamURL(serverURL, vmName string) (string, error) {
	u, err := url.Parse(strings.TrimRight(serverURL, "/"))
	if err != nil {
		return "", fmt.Errorf("sshconn: parse server url: %v", err)
	}
	switch u.Scheme {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("sshconn: unsupported server url scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/vms/" + url.PathEscape(vmName) + "/ssh-stream"
	return u.String(), nil
}

// pemEncode renders a *pem.Block to its textual encoding.
func pemEncode(block *pem.Block) []byte { return pem.EncodeToMemory(block) }

// ensureTrailingNewline guarantees b ends with a single newline (the
// authorized-keys cert line ssh expects in a CertificateFile).
func ensureTrailingNewline(b []byte) []byte {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return append(b, '\n')
	}
	return b
}
