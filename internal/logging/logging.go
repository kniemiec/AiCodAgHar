// Package logging configures the process-wide log/slog default logger
// shared by control-plane and container-launcher.
//
// Correlation fields are carried on the context (WithTask/WithRound/WithStep)
// and appended to every record by contextHandler, so call sites use the
// *Context slog variants and never repeat task_id/round/step_id by hand
// (LG-2). internal/logging/callsites_test.go enforces that.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/kniemiec/AiCodAgHar/internal/buildinfo"
)

// AttrSensitive marks a record whose content may include task or agent
// content (SEC-2, LG-6). Such records go to the local stderr sink only and
// are never offered to additional (central) sinks.
const AttrSensitive = "sensitive"

// Sensitive is the attribute call sites attach to such records.
func Sensitive() slog.Attr { return slog.Bool(AttrSensitive, true) }

type ctxKey struct{}

// taskAttrs is the correlation state carried on a context. Zero fields are
// omitted from records.
type taskAttrs struct {
	TaskID       string
	Round        int
	StepID       string
	InvocationID string
}

func get(ctx context.Context) taskAttrs {
	a, _ := ctx.Value(ctxKey{}).(taskAttrs)
	return a
}

// WithTask returns ctx whose log records carry task_id.
func WithTask(ctx context.Context, taskID string) context.Context {
	a := get(ctx)
	a.TaskID = taskID
	return context.WithValue(ctx, ctxKey{}, a)
}

// WithRound returns ctx whose log records carry round.
func WithRound(ctx context.Context, round int) context.Context {
	a := get(ctx)
	a.Round = round
	return context.WithValue(ctx, ctxKey{}, a)
}

// WithStep returns ctx whose log records carry step_id and invocation_id.
// An empty argument leaves that field as ctx already has it.
func WithStep(ctx context.Context, stepID, invocationID string) context.Context {
	a := get(ctx)
	if stepID != "" {
		a.StepID = stepID
	}
	if invocationID != "" {
		a.InvocationID = invocationID
	}
	return context.WithValue(ctx, ctxKey{}, a)
}

// contextHandler appends the ctx correlation attributes to each record.
type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	a := get(ctx)
	if a.TaskID != "" {
		r.AddAttrs(slog.String("task_id", a.TaskID))
		if a.Round > 0 {
			r.AddAttrs(slog.Int("round", a.Round))
		}
	}
	if a.StepID != "" {
		r.AddAttrs(slog.String("step_id", a.StepID))
	}
	if a.InvocationID != "" {
		r.AddAttrs(slog.String("invocation_id", a.InvocationID))
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(as)}
}

func (h contextHandler) WithGroup(n string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(n)}
}

// fanout sends each record to the stderr handler and, unless the record is
// marked sensitive, to every additional sink (e.g. the OTLP log bridge).
type fanout struct {
	primary slog.Handler
	sinks   []slog.Handler
}

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	if f.primary.Enabled(ctx, l) {
		return true
	}
	for _, s := range f.sinks {
		if s.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if f.primary.Enabled(ctx, r.Level) {
		err = f.primary.Handle(ctx, r.Clone())
	}
	if isSensitive(r) {
		return err
	}
	for _, s := range f.sinks {
		if s.Enabled(ctx, r.Level) {
			if e := s.Handle(ctx, r.Clone()); e != nil && err == nil {
				err = e
			}
		}
	}
	return err
}

func (f fanout) WithAttrs(as []slog.Attr) slog.Handler {
	n := fanout{primary: f.primary.WithAttrs(as), sinks: make([]slog.Handler, len(f.sinks))}
	for i, s := range f.sinks {
		n.sinks[i] = s.WithAttrs(as)
	}
	return n
}

func (f fanout) WithGroup(name string) slog.Handler {
	n := fanout{primary: f.primary.WithGroup(name), sinks: make([]slog.Handler, len(f.sinks))}
	for i, s := range f.sinks {
		n.sinks[i] = s.WithGroup(name)
	}
	return n
}

func isSensitive(r slog.Record) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == AttrSensitive && a.Value.Kind() == slog.KindBool && a.Value.Bool() {
			found = true
			return false
		}
		return true
	})
	return found
}

// Setup installs a slog default logger tagged with component, host and
// build version, configured from LOG_LEVEL (debug|info|warn|error, default
// info) and LOG_FORMAT (text|json, default text). stderr is always on;
// extra sinks (added by later telemetry wiring) receive every
// non-sensitive record. slog.SetDefault also redirects the stdlib log
// package, so startup log.Fatalf lines go through the same handler.
func Setup(component string, extra ...slog.Handler) {
	slog.SetDefault(slog.New(NewHandler(os.Stderr, component, extra...)))
}

// NewHandler builds Setup's handler over w; exported so tests can capture
// exactly what the process would emit.
func NewHandler(w io.Writer, component string, extra ...slog.Handler) slog.Handler {
	opts := &slog.HandlerOptions{Level: parseLevel(os.Getenv("LOG_LEVEL"))}
	var base slog.Handler
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "json") {
		base = slog.NewJSONHandler(w, opts)
	} else {
		base = slog.NewTextHandler(w, opts)
	}
	host, _ := os.Hostname()
	attrs := []slog.Attr{
		slog.String("component", component),
		slog.String("host", host),
		slog.String("version", buildinfo.Version),
	}
	return contextHandler{fanout{primary: base, sinks: extra}.WithAttrs(attrs)}
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Truncate shortens s to at most n bytes for debug output, marking the cut.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// Keys returns m's keys, sorted — for logging an env map without its values,
// which carry credentials.
func Keys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
