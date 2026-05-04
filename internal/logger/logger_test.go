// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package logger

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNewJSONFormatRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	log := newWithWriter(Config{Level: "warn", Format: "json"}, &buf)

	log.Debug("debug-msg")
	log.Info("info-msg")
	log.Warn("warn-msg")
	log.Error("error-msg")

	out := buf.String()
	for _, suppressed := range []string{"debug-msg", "info-msg"} {
		if strings.Contains(out, suppressed) {
			t.Errorf("output contains %q, want it suppressed at warn level", suppressed)
		}
	}
	for _, present := range []string{"warn-msg", "error-msg"} {
		if !strings.Contains(out, present) {
			t.Errorf("output missing %q, want present at warn level", present)
		}
	}
}

func TestNewTextFormat(t *testing.T) {
	var buf bytes.Buffer
	log := newWithWriter(Config{Level: "info", Format: "text"}, &buf)
	log.Info("hello")

	out := buf.String()
	for _, want := range []string{"hello", "level=INFO"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output = %q, want substring %q", out, want)
		}
	}
}

func TestNewInvalidLevelDefaultsToInfo(t *testing.T) {
	var buf bytes.Buffer
	log := newWithWriter(Config{Level: "bogus", Format: "json"}, &buf)
	log.Debug("nope")
	log.Info("yes")

	out := buf.String()
	if strings.Contains(out, "nope") {
		t.Errorf("output contains debug message %q, want default level info", "nope")
	}
	if !strings.Contains(out, "yes") {
		t.Errorf("output missing info message %q", "yes")
	}
}

func TestNewInvalidFormatDefaultsToJSON(t *testing.T) {
	var buf bytes.Buffer
	log := newWithWriter(Config{Level: "info", Format: "bogus"}, &buf)
	log.Info("hello")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("output not valid JSON: %v\nraw=%q", err, buf.String())
	}
	if got := rec["msg"]; got != "hello" {
		t.Errorf(`rec["msg"] = %v, want "hello"`, got)
	}
}

func TestWithComponent(t *testing.T) {
	var buf bytes.Buffer
	base := newWithWriter(Config{Level: "info", Format: "json"}, &buf)
	log := WithComponent(base, "scheduler")
	log.Info("hello")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("output not valid JSON: %v\nraw=%q", err, buf.String())
	}
	if got := rec["component"]; got != "scheduler" {
		t.Errorf(`rec["component"] = %v, want "scheduler"`, got)
	}
}

func TestSetDefaultInstallsLogger(t *testing.T) {
	// No t.Parallel(): mutates slog.Default.
	var buf bytes.Buffer
	custom := newWithWriter(Config{Level: "info", Format: "json"}, &buf)
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	SetDefault(custom)
	slog.Info("via-default")

	if !strings.Contains(buf.String(), "via-default") {
		t.Errorf("default slog output = %q, want to contain %q", buf.String(), "via-default")
	}
}

func TestKeysAreSnakeCase(t *testing.T) {
	var buf bytes.Buffer
	log := newWithWriter(Config{Level: "info", Format: "json"}, &buf)
	log.Info("msg", "request_id", "abc", "user_id", "u1")

	out := buf.String()
	for _, want := range []string{`"request_id":"abc"`, `"user_id":"u1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want substring %q", out, want)
		}
	}
	if strings.Contains(out, "requestId") {
		t.Errorf("output = %q, want snake_case keys (no camelCase)", out)
	}
}
