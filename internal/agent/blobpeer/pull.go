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

// ErrBlobTooLarge is returned when the holder streams more bytes than the
// CP-known expected size, before the disk fills. (Integrity is also enforced by
// the digest verify; this bound stops a disk-fill DoS that would otherwise occur
// before the hash completes.)
var ErrBlobTooLarge = errors.New("blobpeer: pull body exceeds expected size")

// maxUnsizedPullBytes is the absolute fallback cap applied when the CP-known
// ExpectedSize is unknown (<= 0). It matches the source-download cap (64 GiB) so
// an unsized peer pull can never fill the consumer disk: the holder might be
// buggy or compromised, and without a size hint there is no per-blob bound, so
// this ceiling stands in. The end-of-stream digest verify remains the
// correctness backstop; this just bounds the bytes WRITTEN before the verify can
// reject them.
const maxUnsizedPullBytes = 64 << 30

// effectivePullCap returns the byte cap to bound a pull body with. A positive
// expectedSize bounds at expectedSize+1, so a correctly-sized blob reads in full
// and the first over-size byte trips the cap. An unknown size (<= 0) falls back
// to the absolute maxUnsizedPullBytes ceiling, so an unsized pull is bounded
// rather than unbounded.
func effectivePullCap(expectedSize int64) int64 {
	if expectedSize > 0 {
		return expectedSize + 1
	}
	return maxUnsizedPullBytes
}

// boundedReader fails closed once more than max bytes have been read. It records
// the overflow on a flag so the caller can still recognize the cause even though
// artifactstore.Put wraps the underlying copy error with %v (which severs the
// errors.Is chain).
type boundedReader struct {
	r          io.Reader
	max        int64
	read       int64
	overflowed bool
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.read >= b.max {
		b.overflowed = true
		return 0, ErrBlobTooLarge
	}
	if int64(len(p)) > b.max-b.read {
		p = p[:b.max-b.read]
	}
	n, err := b.r.Read(p)
	b.read += int64(n)
	return n, err
}

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
//
// ExpectedSize, when > 0, is the CP-known size in bytes of the blob (from the
// holder's reported inventory). Pull bounds the copied body to it and fails
// closed on overflow, so a misbehaving holder cannot fill the consumer disk
// before the digest check completes. Zero or negative means the size is unknown;
// Pull then bounds the body with an absolute fallback cap (maxUnsizedPullBytes)
// so even an unsized pull can never fill the disk, with the digest verify as the
// correctness backstop.
type PullArgs struct {
	Endpoint       string
	Digest         string
	Token          string
	Store          *artifactstore.Store
	TLSClient      *http.Client
	HolderIdentity string
	ExpectedSize   int64
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

	// Always bound the body the holder may stream into the content-addressed
	// store. When the CP knows the size the cap is ExpectedSize+1, so a
	// correctly-sized blob passes and the first over-size byte trips it. When the
	// size is unknown (an image-tier digest with no reported size, or a stale
	// inventory) the cap falls back to the absolute maxUnsizedPullBytes ceiling so
	// an unsized pull can never fill the disk. This stops a disk-fill DoS that
	// would otherwise occur before the sha256 verify completes. The cap is a flag
	// on the bounded reader (not the returned error) because artifactstore.Put
	// wraps the copy error with %v, severing the errors.Is chain - so overflow is
	// recognized by br.overflowed, not errors.Is(err, ...).
	br := &boundedReader{r: resp.Body, max: effectivePullCap(a.ExpectedSize)}
	if err := a.Store.Put(a.Digest, br); err != nil {
		if br.overflowed {
			return fmt.Errorf("%w: holder streamed past expected size for digest %s", ErrBlobTooLarge, a.Digest)
		}
		if errors.Is(err, artifactstore.ErrDigestMismatch) {
			return fmt.Errorf("%w: %v", ErrBlobVerifyFailed, err)
		}
		return fmt.Errorf("blobpeer: store pulled blob: %v", err)
	}
	return nil
}
