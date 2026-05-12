// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"net/http"
	"net/http/httptest"
)

// captureWriter is the minimal http.ResponseWriter the validation
// helpers need. The validation tests do not assert on body shape —
// only on the boolean return — so the recorder's default behaviour
// is sufficient.
type captureWriter = httptest.ResponseRecorder

// fakeRequest returns a minimal *http.Request the validation helpers
// can pass to response.WriteError without panicking.
func fakeRequest() *http.Request {
	req, _ := http.NewRequest(http.MethodPost, "/v1/vms", nil)
	return req
}
