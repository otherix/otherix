// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// TestZapToSlogBridge proves an etcd-style zap log line lands in the Otherix
// slog stream tagged source=etcd, carrying its structured field, at the mapped
// slog level - the zap-to-slog bridge.
func TestZapToSlogBridge(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	zl := newZapToSlog(slog.New(handler), zapcore.WarnLevel)

	zl.Warn("raft leader changed", zap.String("local-member-id", "abc123"))

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("decode slog record %q: %v", buf.String(), err)
	}
	if rec["msg"] != "raft leader changed" {
		t.Errorf("msg = %v, want %q", rec["msg"], "raft leader changed")
	}
	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", rec["level"])
	}
	if rec["source"] != "etcd" {
		t.Errorf("source = %v, want etcd", rec["source"])
	}
	if rec["local-member-id"] != "abc123" {
		t.Errorf("local-member-id = %v, want abc123", rec["local-member-id"])
	}
}

// TestZapToSlogBridgeDropsBelowLevel confirms entries under the configured
// minimum are not forwarded (so etcd debug/info noise stays out of the stream
// when the bridge is set to warn).
func TestZapToSlogBridgeDropsBelowLevel(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	zl := newZapToSlog(slog.New(handler), zapcore.WarnLevel)

	zl.Info("compaction completed")
	if buf.Len() != 0 {
		t.Errorf("info entry forwarded under warn-min bridge: %q", buf.String())
	}
}

func TestZapToSlogLevel(t *testing.T) {
	cases := []struct {
		in   zapcore.Level
		want slog.Level
	}{
		{zapcore.DebugLevel, slog.LevelDebug},
		{zapcore.InfoLevel, slog.LevelInfo},
		{zapcore.WarnLevel, slog.LevelWarn},
		{zapcore.ErrorLevel, slog.LevelError},
		{zapcore.DPanicLevel, slog.LevelError},
		{zapcore.FatalLevel, slog.LevelError},
	}
	for _, tc := range cases {
		if got := zapToSlogLevel(tc.in); got != tc.want {
			t.Errorf("zapToSlogLevel(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
