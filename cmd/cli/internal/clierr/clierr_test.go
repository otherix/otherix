// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package clierr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func TestClassifyAPIErrorPassesThrough(t *testing.T) {
	apiErr := &cpclient.APIError{StatusCode: 404, Code: "not_found", Message: "vm missing"}
	got := Classify(apiErr)
	want := "api_error: not_found: vm missing"
	if got.Error() != want {
		t.Errorf("Classify(APIError) = %q, want %q", got.Error(), want)
	}
}

func TestClassifyDeadlineExceeded(t *testing.T) {
	err := fmt.Errorf("dial: %w", context.DeadlineExceeded)
	got := Classify(err)
	if !strings.HasPrefix(got.Error(), "request_timeout: ") {
		t.Errorf("Classify(deadline) = %q, want request_timeout: prefix", got.Error())
	}
}

func TestClassifyCanceled(t *testing.T) {
	// A user Ctrl-C surfaces as context.Canceled; it must not be mislabeled
	// as a transport fault (request_failed) that scripts grep for.
	err := fmt.Errorf("get: %w", context.Canceled)
	got := Classify(err)
	if !strings.HasPrefix(got.Error(), "cancelled: ") {
		t.Errorf("Classify(canceled) = %q, want cancelled: prefix", got.Error())
	}
}

func TestClassifyConnectionRefused(t *testing.T) {
	err := errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")
	got := Classify(err)
	if !strings.HasPrefix(got.Error(), "connection_refused: ") {
		t.Errorf("Classify(refused) = %q, want connection_refused: prefix", got.Error())
	}
}

func TestClassifyTLSHandshake(t *testing.T) {
	err := errors.New("x509: certificate signed by unknown authority")
	got := Classify(err)
	if !strings.HasPrefix(got.Error(), "tls_handshake_failed: ") {
		t.Errorf("Classify(x509) = %q, want tls_handshake_failed: prefix", got.Error())
	}
}

func TestClassifyDNSError(t *testing.T) {
	err := &net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true}
	got := Classify(err)
	if got == nil || !strings.HasPrefix(got.Error(), "host_not_found:") {
		t.Errorf("Classify(DNSError) = %v, want host_not_found: prefix", got)
	}
}

func TestClassifyGenericFallback(t *testing.T) {
	err := errors.New("something unexpected happened")
	got := Classify(err)
	if !strings.HasPrefix(got.Error(), "request_failed: ") {
		t.Errorf("Classify(generic) = %q, want request_failed: prefix", got.Error())
	}
}
