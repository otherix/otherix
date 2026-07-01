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
//   - DialIngress brokers an L4 ingress connection via POST /v1/vms/{vm}/ingress
//     and returns a spliceable net.Conn over the selected transport: a direct
//     TLS connection to a converged gateway (control plane out of the data path)
//     or the control-plane relay WebSocket.
//   - Proxy brokers via DialIngress and splices the resulting transport to a
//     supplied stdin/stdout pair. This is the body of an ssh ProxyCommand.
//   - WriteSSHConfigBlock writes a marker-delimited managed ssh_config block so
//     `ssh <name>.<cluster-suffix>` routes transparently through the connector.
//
// The connector is transport- and UX-agnostic: it logs nothing (never the
// bearer token, never key material) and leaves all terminal handling and
// argument parsing to its callers.
package sshconn

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	// sha256(cert.Raw). A leading "sha256:" or "pin:" and any colons are
	// tolerated and stripped. It takes precedence over CACertPEM: a leaf pin
	// is the stricter single-cert trust. See resolveTLSConfig for the full
	// trust precedence.
	CAFingerprint string

	// CACertPEM, when non-empty, is a PEM bundle (typically the cluster CA per
	// ADR 0026) used as the sole RootCAs pool to verify the CP's certificate
	// chain. It mirrors the trust the credentialed CLI client already uses, so
	// the connector reaches the same cluster-CA-signed CP. Ignored when
	// CAFingerprint or InsecureSkipTLSVerify is set.
	CACertPEM []byte

	// InsecureSkipTLSVerify, when true, disables all TLS verification (an
	// explicit operator opt-out mirroring the credentialed CLI client). It
	// takes precedence over every other trust input.
	InsecureSkipTLSVerify bool

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

// ingressResponse mirrors the POST /v1/vms/{vm}/ingress broker response. The
// control plane selects the transport; gateway-only fields stay empty on the
// relay path.
type ingressResponse struct {
	Transport   string `json:"transport"`
	VMID        string `json:"vm_id"`
	Port        int    `json:"port"`
	SplicerAddr string `json:"splicer_addr"`
	SessionCred string `json:"session_cred"`
	ExpiresAt   string `json:"expires_at"`
}

// DialIngress brokers an L4 ingress connection to vmName's guest port and
// returns a spliceable net.Conn carrying raw bytes to the guest. It POSTs to
// {ServerURL}/v1/vms/{vm}/ingress, then establishes the transport the control
// plane selected: a direct TLS connection to the converged gateway presenting
// the minted session credential ("gateway", control plane out of the data
// path), or the control-plane relay WebSocket ("relay"). The caller splices its
// own byte stream to the returned conn and closes it when done.
func DialIngress(ctx context.Context, cfg Config, vmName string, port int) (net.Conn, error) {
	resp, err := brokerIngress(ctx, cfg, vmName, port)
	if err != nil {
		return nil, err
	}
	switch resp.Transport {
	case "gateway":
		return dialGateway(ctx, cfg, resp)
	case "relay":
		return dialRelay(ctx, cfg, vmName, resp.Port)
	default:
		return nil, fmt.Errorf("sshconn: unknown ingress transport %q", resp.Transport)
	}
}

// brokerIngress posts the desired guest port to the CP ingress broker and
// returns the connect coordinates. Only the port crosses the wire; the bearer
// authorizes the call.
func brokerIngress(ctx context.Context, cfg Config, vmName string, port int) (ingressResponse, error) {
	client, err := wsHTTPClient(cfg)
	if err != nil {
		return ingressResponse{}, err
	}
	client.Timeout = fetchTimeout

	reqBody, err := json.Marshal(ingressRequest{Port: port})
	if err != nil {
		return ingressResponse{}, fmt.Errorf("sshconn: marshal ingress request: %v", err)
	}
	endpoint := strings.TrimRight(cfg.ServerURL, "/") + "/v1/vms/" + url.PathEscape(vmName) + "/ingress"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(reqBody)))
	if err != nil {
		return ingressResponse{}, fmt.Errorf("sshconn: build ingress request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return ingressResponse{}, fmt.Errorf("sshconn: broker ingress: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return ingressResponse{}, fmt.Errorf("sshconn: broker ingress: HTTP %d", resp.StatusCode)
	}
	var out ingressResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return ingressResponse{}, fmt.Errorf("sshconn: decode ingress response: %v", err)
	}
	if out.Transport == "" {
		return ingressResponse{}, errors.New("sshconn: ingress response carried no transport")
	}
	return out, nil
}

// ingressRequest is the broker request body: the guest TCP port to reach.
type ingressRequest struct {
	Port int `json:"port"`
}

// dialGateway TLS-dials the converged gateway's splicer address, performs the
// connect upgrade (POST /v1/connect with the session credential as the bearer),
// and returns the spliceable conn positioned at the raw guest byte stream.
func dialGateway(ctx context.Context, cfg Config, resp ingressResponse) (net.Conn, error) {
	if resp.SplicerAddr == "" || resp.SessionCred == "" {
		return nil, errors.New("sshconn: gateway broker response missing splicer address or credential")
	}
	// The broker reports the gateway's advertised endpoint, which is a full
	// https URL (validated as such at node join). Derive the host:port to dial
	// and the hostname to pin as the TLS ServerName from it.
	u, err := url.Parse(resp.SplicerAddr)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("sshconn: parse splicer address %q: %v", resp.SplicerAddr, err)
	}
	tlsCfg, err := gatewayTLSConfig(cfg, u.Hostname())
	if err != nil {
		return nil, err
	}
	conn, err := (&tls.Dialer{Config: tlsCfg}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("sshconn: dial gateway %s: %v", u.Host, err)
	}
	spliced, err := gatewayConnect(conn, u.Host, resp.SessionCred)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return spliced, nil
}

// gatewayConnect writes the minimal POST /v1/connect upgrade request carrying
// the session credential, consumes the gateway's status line + headers up to the
// blank line, and returns a conn whose reads continue from the buffered reader
// so no leading guest byte is lost. A non-200 status is surfaced as an error.
func gatewayConnect(conn net.Conn, host, cred string) (net.Conn, error) {
	req := "POST /v1/connect HTTP/1.1\r\nHost: " + host +
		"\r\nAuthorization: Bearer " + cred + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, fmt.Errorf("sshconn: send gateway connect: %v", err)
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("sshconn: read gateway response: %v", err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("sshconn: read gateway headers: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if code, ok := parseStatusCode(statusLine); !ok || code != http.StatusOK {
		return nil, fmt.Errorf("sshconn: gateway refused connect: %s", strings.TrimSpace(statusLine))
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// parseStatusCode extracts the numeric code from an "HTTP/1.1 <code> <reason>"
// status line. ok is false when the line is malformed.
func parseStatusCode(statusLine string) (int, bool) {
	fields := strings.Fields(statusLine)
	if len(fields) < 2 {
		return 0, false
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return code, true
}

// bufferedConn is a net.Conn whose reads draw from a bufio.Reader that already
// consumed the connect handshake headers, so any guest bytes the gateway buffered
// alongside the 200 response are not dropped. Writes, Close, and CloseWrite pass
// through to the underlying conn.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// CloseWrite half-closes the write direction when the underlying conn supports
// it (a *tls.Conn does), so a peer reading the spliced stream sees a clean EOF.
func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// gatewayTLSConfig builds the TLS trust for the gateway leg: it reuses the
// connector's configured trust (the cluster CA bundle per ADR 0026) and pins the
// ServerName to the splicer host, since the gateway leaf is cluster-CA-signed
// with its advertised endpoint in the SAN. It never disables verification on its
// own (only an explicit operator InsecureSkipTLSVerify does).
func gatewayTLSConfig(cfg Config, host string) (*tls.Config, error) {
	base, err := resolveTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	base.ServerName = host
	return base, nil
}

// dialRelay dials the control-plane ssh-stream relay WebSocket for vmName,
// threading the guest port, and returns the spliceable net.Conn.
func dialRelay(ctx context.Context, cfg Config, vmName string, port int) (net.Conn, error) {
	su, err := streamURL(cfg.ServerURL, vmName, port)
	if err != nil {
		return nil, err
	}
	client, err := wsHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	if cfg.BearerToken != "" {
		hdr.Set("Authorization", "Bearer "+cfg.BearerToken)
	}
	conn, _, err := websocket.Dial(ctx, su, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: hdr,
	})
	if err != nil {
		return nil, fmt.Errorf("sshconn: dial ssh-stream: %v", err)
	}
	return websocket.NetConn(ctx, conn, websocket.MessageBinary), nil
}

// Proxy brokers an ingress connection to vmName's guest port and splices the
// resulting transport (gateway or relay) to stdin/stdout. It returns when either
// side closes (stdin EOF, guest close, context cancel). This is the body of an
// ssh ProxyCommand: stdin/stdout are the ssh client's pipes and the spliced
// bytes are end-to-end SSH the control plane never inspects. port is the guest
// port from the ProxyCommand `%p` token.
func Proxy(ctx context.Context, cfg Config, vmName string, port int, stdin io.Reader, stdout io.Writer) error {
	conn, err := DialIngress(ctx, cfg, vmName, port)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// The guest->client direction is authoritative for session lifetime: it
	// ends when the guest closes the stream (clean EOF) or when the local stdout
	// pipe breaks because the ssh client exited. The client->guest copy runs in
	// the background; when stdin reaches EOF it stops on its own and the deferred
	// Close tears the transport down. We do not close from the stdin goroutine so
	// an in-flight guest->client frame is never dropped.
	go func() { _, _ = io.Copy(conn, stdin) }()

	_, copyErr := io.Copy(stdout, conn)
	if copyErr != nil &&
		!errors.Is(copyErr, io.EOF) &&
		!errors.Is(copyErr, context.Canceled) &&
		!errors.Is(copyErr, net.ErrClosed) {
		return fmt.Errorf("sshconn: ingress relay: %v", copyErr)
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

// wsHTTPClient builds an *http.Client whose TLS trust honours cfg per
// resolveTLSConfig. The same client backs both the cert-mint round-trip and
// the ssh-stream WebSocket dial, so both speak the operator's chosen trust.
func wsHTTPClient(cfg Config) (*http.Client, error) {
	tlsCfg, err := resolveTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	return &http.Client{Transport: tr}, nil
}

// resolveTLSConfig builds the connector's TLS trust config from cfg with this
// precedence:
//
//  1. InsecureSkipTLSVerify true -> verification disabled (explicit opt-out).
//  2. else CAFingerprint set -> pin the presented leaf by sha256(cert.Raw).
//  3. else CACertPEM non-empty -> that PEM bundle is the sole RootCAs pool.
//  4. else -> the system root store.
//
// Leaf-pin and CA-bundle are mutually exclusive: a set CAFingerprint wins, as
// it is the stricter single-cert trust. A CACertPEM that yields no usable
// certificate is a hard error rather than a silent fall-through to system roots.
func resolveTLSConfig(cfg Config) (*tls.Config, error) {
	if cfg.InsecureSkipTLSVerify {
		return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}, nil //nolint:gosec // explicit operator opt-out, mirrors the credentialed CLI client.
	}
	if normalizeFingerprint(cfg.CAFingerprint) != "" {
		return pinnedTLSConfig(cfg.CAFingerprint)
	}
	if len(cfg.CACertPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cfg.CACertPEM) {
			return nil, errors.New("sshconn: CA bundle contained no valid certificate")
		}
		return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, nil
	}
	return &tls.Config{MinVersion: tls.VersionTLS12}, nil
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
// http->ws and https->wss and threading the guest port as ?port=N so the relay
// targets the requested guest port.
func streamURL(serverURL, vmName string, port int) (string, error) {
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
	if port != 0 {
		q := u.Query()
		q.Set("port", strconv.Itoa(port))
		u.RawQuery = q.Encode()
	}
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
