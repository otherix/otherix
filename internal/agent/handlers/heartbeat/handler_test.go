// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type spyNudger struct{ n int }

func (s *spyNudger) Nudge() { s.n++ }

func TestNudgeHandlerTriggersAndReturns204(t *testing.T) {
	sp := &spyNudger{}
	h := New(sp)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/heartbeat/nudge", nil)

	h.Nudge(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if sp.n != 1 {
		t.Errorf("nudge calls = %d, want 1", sp.n)
	}
}
