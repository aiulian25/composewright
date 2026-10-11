package redact

import (
	"context"
	"log/slog"
)

// Handler wraps a slog.Handler so that attributes named like secrets are
// masked and every string value passes through Text.
type Handler struct {
	next slog.Handler
}

func NewHandler(next slog.Handler) *Handler {
	return &Handler{next: next}
}

func (handler *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.next.Enabled(ctx, level)
}

func (handler *Handler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, Text(record.Message), record.PC)
	record.Attrs(func(attribute slog.Attr) bool {
		clean.AddAttrs(attr(attribute))
		return true
	})
	return handler.next.Handle(ctx, clean)
}

func (handler *Handler) WithAttrs(attributes []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, 0, len(attributes))
	for _, attribute := range attributes {
		clean = append(clean, attr(attribute))
	}
	return &Handler{next: handler.next.WithAttrs(clean)}
}

func (handler *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: handler.next.WithGroup(name)}
}

func attr(attribute slog.Attr) slog.Attr {
	attribute.Value = attribute.Value.Resolve()
	if IsSecretName(attribute.Key) {
		return slog.String(attribute.Key, Mask)
	}
	switch attribute.Value.Kind() {
	case slog.KindGroup:
		members := attribute.Value.Group()
		clean := make([]any, 0, len(members))
		for _, member := range members {
			clean = append(clean, attr(member))
		}
		return slog.Group(attribute.Key, clean...)
	case slog.KindString:
		return slog.String(attribute.Key, Text(attribute.Value.String()))
	case slog.KindAny:
		if err, ok := attribute.Value.Any().(error); ok {
			return slog.String(attribute.Key, Text(err.Error()))
		}
	}
	return attribute
}
