// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"context"
	"log/slog"

	"go.etcd.io/etcd/server/v3/embed"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// etcd logs through zap, Otherix logs through slog. Rather than switch Otherix
// to zap, we hand etcd a zap.Logger whose core forwards every entry into
// Otherix's slog handler, so etcd's internal logs join the one slog stream
// (same format, sink, and level control). ADR 0030 constraint #5.

// slogZapCore is a zapcore.Core that forwards entries to an slog.Logger. It
// accumulates With()-bound fields and converts zap fields to slog attrs via a
// MapObjectEncoder so every zap field type is handled without a hand-written
// type switch.
type slogZapCore struct {
	logger *slog.Logger
	level  zapcore.LevelEnabler
	fields []zapcore.Field
}

// newZapToSlog builds a zap.Logger that writes through the given slog.Logger at
// or above minLevel.
func newZapToSlog(logger *slog.Logger, minLevel zapcore.Level) *zap.Logger {
	core := &slogZapCore{
		logger: logger.With("source", "etcd"),
		level:  minLevel,
	}
	return zap.New(core)
}

// zapBuilderFromSlog returns an embed ZapLoggerBuilder that installs the
// zap-to-slog bridge as etcd's logger.
func zapBuilderFromSlog(logger *slog.Logger, minLevel zapcore.Level) func(*embed.Config) error {
	return embed.NewZapLoggerBuilder(newZapToSlog(logger, minLevel))
}

func (c *slogZapCore) Enabled(l zapcore.Level) bool {
	return c.level.Enabled(l)
}

func (c *slogZapCore) With(fields []zapcore.Field) zapcore.Core {
	combined := make([]zapcore.Field, 0, len(c.fields)+len(fields))
	combined = append(combined, c.fields...)
	combined = append(combined, fields...)
	return &slogZapCore{logger: c.logger, level: c.level, fields: combined}
}

func (c *slogZapCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c *slogZapCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range c.fields {
		f.AddTo(enc)
	}
	for _, f := range fields {
		f.AddTo(enc)
	}
	attrs := make([]slog.Attr, 0, len(enc.Fields)+1)
	for k, v := range enc.Fields {
		attrs = append(attrs, slog.Any(k, v))
	}
	if ent.LoggerName != "" {
		attrs = append(attrs, slog.String("logger", ent.LoggerName))
	}
	c.logger.LogAttrs(context.Background(), zapToSlogLevel(ent.Level), ent.Message, attrs...)
	return nil
}

func (c *slogZapCore) Sync() error { return nil }

// zapToSlogLevel maps zap levels onto slog levels. zap's DPanic/Panic/Fatal all
// surface as slog Error - we do not want etcd to take down the process through
// the log path.
func zapToSlogLevel(l zapcore.Level) slog.Level {
	switch {
	case l <= zapcore.DebugLevel:
		return slog.LevelDebug
	case l == zapcore.InfoLevel:
		return slog.LevelInfo
	case l == zapcore.WarnLevel:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}
