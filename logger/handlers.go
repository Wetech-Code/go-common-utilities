package logger

import (
	"context"
	"log/slog"
)

// RedactAttr hides the value of an attribute whose key looks sensitive. It has
// the signature of slog.HandlerOptions.ReplaceAttr, so handlers that format
// records themselves (such as the Loki handler) can use it directly.
func RedactAttr(groups []string, a slog.Attr) slog.Attr { return redact(groups, a) }

// Redact returns a handler that hides sensitive attribute values (the same rule
// New applies to its own output) before passing each record to h. Use it around
// any handler that ships records somewhere else, so secrets never leave the
// process. Attributes inside groups are checked too, and so are group names.
func Redact(h slog.Handler) slog.Handler { return redactHandler{h: h} }

// redactHandler hides sensitive values. hidden is true once WithGroup has opened
// a group with a sensitive name: everything added under it is then hidden.
type redactHandler struct {
	h      slog.Handler
	hidden bool
}

// clean returns a with sensitive values hidden; if the handler is inside a
// sensitive group every value is hidden.
func (r redactHandler) clean(a slog.Attr) slog.Attr {
	if r.hidden {
		return hideAll(a)
	}
	return redactDeep(a)
}

// hideAll keeps the keys and group structure of a but hides every value.
func hideAll(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		members := a.Value.Group()
		hidden := make([]slog.Attr, len(members))
		for i, m := range members {
			hidden[i] = hideAll(m)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(hidden...)}
	}
	return slog.String(a.Key, redacted)
}

func (r redactHandler) Enabled(ctx context.Context, l slog.Level) bool { return r.h.Enabled(ctx, l) }

func (r redactHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, rec.Message, rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(r.clean(a))
		return true
	})
	return r.h.Handle(ctx, out)
}

func (r redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = r.clean(a)
	}
	return redactHandler{r.h.WithAttrs(clean), r.hidden}
}

func (r redactHandler) WithGroup(name string) slog.Handler {
	return redactHandler{r.h.WithGroup(name), r.hidden || isSensitive(name)}
}

// redactDeep applies the key rule to a and to everything inside groups.
func redactDeep(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		if isSensitive(a.Key) {
			return slog.String(a.Key, redacted)
		}
		members := a.Value.Group()
		clean := make([]slog.Attr, len(members))
		for i, m := range members {
			clean[i] = redactDeep(m)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(clean...)}
	}
	return redact(nil, a)
}

// WithContextAttrs returns a handler that adds the attributes fn returns for the
// record's context to every record, for example trace and span IDs. fn may
// return nil.
func WithContextAttrs(h slog.Handler, fn func(context.Context) []slog.Attr) slog.Handler {
	if fn == nil {
		return h
	}
	return contextHandler{h, fn}
}

type contextHandler struct {
	h  slog.Handler
	fn func(context.Context) []slog.Attr
}

func (c contextHandler) Enabled(ctx context.Context, l slog.Level) bool { return c.h.Enabled(ctx, l) }

func (c contextHandler) Handle(ctx context.Context, rec slog.Record) error {
	if attrs := c.fn(ctx); len(attrs) > 0 {
		rec = rec.Clone()
		rec.AddAttrs(attrs...)
	}
	return c.h.Handle(ctx, rec)
}

func (c contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{c.h.WithAttrs(attrs), c.fn}
}

func (c contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{c.h.WithGroup(name), c.fn}
}

// levelHandler drops records below min before they reach h.
type levelHandler struct {
	h   slog.Handler
	min slog.Level
}

func (l levelHandler) Enabled(ctx context.Context, lv slog.Level) bool {
	return lv >= l.min && l.h.Enabled(ctx, lv)
}

func (l levelHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level < l.min {
		return nil
	}
	return l.h.Handle(ctx, rec)
}

func (l levelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return levelHandler{l.h.WithAttrs(attrs), l.min}
}

func (l levelHandler) WithGroup(name string) slog.Handler {
	return levelHandler{l.h.WithGroup(name), l.min}
}
