// Package logging carries log attributes, such as a request ID or a VM ID,
// in a context, so every log line written for that request or VM includes
// them, however deep in tined it's written.
//
// Wrap tined's slog handler with NewHandler, attach attributes with With, and
// log with the *Context methods (log.InfoContext(ctx, ...)).
package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

type ctxKey struct{}

// With returns a context whose log lines also carry attrs. Later attributes
// with the same key replace earlier ones.
func With(ctx context.Context, attrs ...slog.Attr) context.Context {
	existing, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	for _, a := range existing {
		replaced := false
		for _, b := range attrs {
			if a.Key == b.Key {
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, a)
		}
	}
	merged = append(merged, attrs...)
	return context.WithValue(ctx, ctxKey{}, merged)
}

// WithVM returns a context whose log lines carry the VM's ID.
func WithVM(ctx context.Context, id string) context.Context {
	return With(ctx, slog.String("vm", id))
}

// Attrs returns the attributes ctx carries.
func Attrs(ctx context.Context) []slog.Attr {
	attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	return attrs
}

// NewRequestID makes a short random ID for a request.
func NewRequestID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewHandler wraps h so records also get the attributes their context carries.
func NewHandler(h slog.Handler) slog.Handler { return &handler{inner: h} }

type handler struct{ inner slog.Handler }

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	if attrs := Attrs(ctx); len(attrs) > 0 {
		// Skip attributes the record already has, so a key never appears twice.
		have := map[string]bool{}
		r.Attrs(func(a slog.Attr) bool {
			have[a.Key] = true
			return true
		})
		for _, a := range attrs {
			if !have[a.Key] {
				r.AddAttrs(a)
			}
		}
	}
	return h.inner.Handle(ctx, r)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{inner: h.inner.WithAttrs(attrs)}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{inner: h.inner.WithGroup(name)}
}
