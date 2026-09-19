//go:build tinygo && esp32

package main

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// consoleHandler logs as slog text for people reading the serial console,
// or as JSON lines for totemctl mesh, which switches with "format json".
type consoleHandler struct {
	json        *atomic.Bool
	text, jsonH slog.Handler
}

func newConsoleHandler(text, json slog.Handler, useJSON *atomic.Bool) consoleHandler {
	return consoleHandler{json: useJSON, text: text, jsonH: json}
}

func (h consoleHandler) current() slog.Handler {
	if h.json.Load() {
		return h.jsonH
	}
	return h.text
}

func (h consoleHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.current().Enabled(ctx, l)
}

func (h consoleHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.current().Handle(ctx, r)
}

func (h consoleHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return consoleHandler{h.json, h.text.WithAttrs(as), h.jsonH.WithAttrs(as)}
}

func (h consoleHandler) WithGroup(name string) slog.Handler {
	return consoleHandler{h.json, h.text.WithGroup(name), h.jsonH.WithGroup(name)}
}
