// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package blobpeer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/otherix/otherix/internal/agent/artifactstore"
)

// ErrBlobVerifyFailed is returned when the pulled bytes do not hash to the
// requested digest. Fail-closed: the blob is never materialized into the store
// (artifactstore.Put re-verifies and discards a mismatch). The pull task fails.
var ErrBlobVerifyFailed = errors.New("blobpeer: pulled blob failed digest verification")

// PullArgs is the input to Pull. Endpoint is the holder's peer-listener base URL
// (https://host:port); Digest the requested content address; Token the per-op
// bearer token; Store the local artifact store to land the blob in; TLSClient an
// *http.Client carrying the node leaf cert (mTLS) and trusting the cluster CA.
//
// HolderIdentity, when non-empty, pins the TLS ServerName the consumer verifies
// the holder's cert against (the holder's node identity SAN,
// "node-<name>.agents.otherix.local"). The consumer dials the holder by its
// inter-node IP, but the holder's cluster-CA node leaf cert carries that
// identity SAN, NOT the dialed IP - so without this pin Go fails the handshake
// with "certificate is valid for <SAN>, not <IP>". This mirrors the live
// migration data path's tls-hostname pin (internal/agent/qemu/nbd.go). Empty
// preserves the default behavior (verify against the dialed host).
type PullArgs struct {
	Endpoint       string
	Digest         string
	Token          string
	Store          *artifactstore.Store
	TLSClient      *http.Client
	HolderIdentity string
}

// Pull streams the blob from the holder's peer listener over mTLS HTTPS and
// writes it into the local artifact store. artifactstore.Put computes the sha256
// on the way and rejects a mismatch (ErrDigestMismatch), so a wrong-bytes holder
// can never materialize a blob - Pull maps that to ErrBlobVerifyFailed. A non-2xx
// response (bad token -> 401/403, absent -> 404) returns an error without
// materializing anything.
func Pull(ctx context.Context, a PullArgs) error {
	target, err := url.JoinPath(a.Endpoint, "/blobs/"+a.Digest)
	if err != nil {
		return fmt.Errorf("blobpeer: build pull url: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("blobpeer: new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)

	client := a.TLSClient
	if a.HolderIdentity != "" {
		// Pin TLS verification to the holder's node identity SAN rather than the
		// dialed IP, without mutating the shared client: clone the transport and
		// set ServerName on a copy of its TLS config. InsecureSkipVerify stays
		// false and RootCAs (cluster CA) is preserved by the clone - only
		// ServerName is overridden. A non-*http.Transport client (a test double)
		// is used as-is.
		if tr, ok := a.TLSClient.Transport.(*http.Transport); ok {
			cloned := tr.Clone()
			if cloned.TLSClientConfig == nil {
				cloned.TLSClientConfig = &tls.Config{} //nolint:gosec // G402: MinVersion unset only when the source transport had no TLS config; the production puller always supplies one.
			}
			cloned.TLSClientConfig.ServerName = a.HolderIdentity
			client = &http.Client{Transport: cloned, Timeout: a.TLSClient.Timeout}
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("blobpeer: pull request: %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("blobpeer: holder returned %d for digest %s", resp.StatusCode, a.Digest)
	}

	if err := a.Store.Put(a.Digest, resp.Body); err != nil {
		if errors.Is(err, artifactstore.ErrDigestMismatch) {
			return fmt.Errorf("%w: %v", ErrBlobVerifyFailed, err)
		}
		return fmt.Errorf("blobpeer: store pulled blob: %v", err)
	}
	return nil
}
