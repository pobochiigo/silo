package telemetry

import (
	"context"
	"errors"
	"log/slog"
)

// fanoutHandler dispatches every record to all wrapped handlers.
type fanoutHandler struct {
	handlers []slog.Handler
}

// NewFanoutHandler returns a slog.Handler that forwards each record to every
// handler in handlers that is enabled for the record's level. It is what
// InitLogs installs to keep logs visible locally while exporting them to the
// collector. Handle returns the errors of all failing handlers joined; one
// failing destination never prevents the others from receiving the record.
func NewFanoutHandler(handlers ...slog.Handler) slog.Handler {
	hs := make([]slog.Handler, 0, len(handlers))
	for _, h := range handlers {
		if h != nil {
			hs = append(hs, h)
		}
	}
	return &fanoutHandler{handlers: hs}
}

// Enabled reports whether any wrapped handler accepts records at level.
func (f *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle forwards r to every enabled handler. Each handler receives its own
// clone, as handlers may retain or mutate the record they are given.
func (f *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// WithAttrs returns a fan-out over the wrapped handlers with attrs applied.
func (f *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &fanoutHandler{handlers: hs}
}

// WithGroup returns a fan-out over the wrapped handlers with the group applied.
func (f *fanoutHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return f
	}
	hs := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &fanoutHandler{handlers: hs}
}
