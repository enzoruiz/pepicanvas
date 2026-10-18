// Package logging provides the slog handler every process log must pass
// through: it redacts secrets and attaches the context correlation ID (TE-003).
package logging

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

// CorrelationKey is the attribute that carries the correlation ID.
const CorrelationKey = "correlation_id"

const redacted = "[REDACTED]"

// sensitiveKeyParts mark attribute keys whose whole value is redacted,
// compared case-insensitively against the leaf key.
var sensitiveKeyParts = []string{
	"authorization", "cookie", "credential", "password", "secret", "token",
}

type handler struct {
	inner slog.Handler
}

// NewHandler wraps inner so that every record is redacted before inner sees it.
func NewHandler(inner slog.Handler) slog.Handler {
	return handler{inner: inner}
}

func (h handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h handler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, secret.Scrub(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(scrubAttr(a))
		return true
	})
	if id, ok := correlation.FromContext(ctx); ok {
		out.AddAttrs(slog.String(CorrelationKey, id.String()))
	}
	return h.inner.Handle(ctx, out)
}

func (h handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = scrubAttr(a)
	}
	return handler{inner: h.inner.WithAttrs(scrubbed)}
}

func (h handler) WithGroup(name string) slog.Handler {
	return handler{inner: h.inner.WithGroup(name)}
}

func scrubAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	if sensitiveKey(a.Key) {
		return slog.String(a.Key, redacted)
	}
	if v.Kind() == slog.KindGroup {
		group := v.Group()
		scrubbed := make([]any, len(group))
		for i, member := range group {
			scrubbed[i] = scrubAttr(member)
		}
		return slog.Group(a.Key, scrubbed...)
	}
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, secret.Scrub(v.String()))
	case slog.KindAny:
		// Arbitrary values are flattened to scrubbed text: a struct or error
		// can hold secrets in fields the handler cannot inspect safely.
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, secret.Scrub(err.Error()))
		}
		return slog.String(a.Key, secret.Scrub(fmt.Sprintf("%+v", v.Any())))
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}

func sensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, part := range sensitiveKeyParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}
