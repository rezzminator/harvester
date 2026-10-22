// Package obs is harvester's activity log: one structured destination for every
// corner of the engine, per harvester home.
//
// OpenLog installs the process logger once, at process entry, over a
// JSON-lines file under the resolved home — so a fence, a test jail, a lane
// container and the live host each write their own file and can never mix.
// Logger reads the logger a context carries, With scopes one into a context,
// and every record is scrubbed by Scrub before it
// reaches the file: a key nobody declared, or a value that looks like a
// credential or a body, never lands on disk.
package obs

import (
	"context"
	"log/slog"

	"github.com/rezzminator/harvester/internal/clock"
)

// Field names every record carries, and the scoped ones a caller adds. They
// are named here so every call site uses the same spelling.
const (
	FieldCmd     = "cmd"
	FieldPID     = "pid"
	FieldVersion = "version"
	FieldDur     = "dur_ms"
	FieldErr     = "err"
	FieldExit    = "exit"
	// FieldTime is the timestamp key: slog's "time" renamed, because every
	// other harvester surface spells it ts.
	FieldTime = "ts"
)

// contextKey is this package's private context key type.
type contextKey struct{}

// scope is what a context carries: the logger records are written through and
// the clock its middleware measures with.
type scope struct {
	logger *slog.Logger
	timing clock.Clock
}

// Logger returns the logger ctx carries, or the process logger OpenLog
// installed. Before OpenLog — a unit test that never opened one — it is a
// logger that discards, never nil.
func Logger(ctx context.Context) *slog.Logger {
	return current(ctx).logger
}

// With returns a context whose logger adds kv to its existing fields.
func With(ctx context.Context, kv ...any) context.Context {
	if len(kv) == 0 {
		return ctx
	}
	existing := current(ctx)
	return context.WithValue(ctx, contextKey{}, &scope{logger: existing.logger.With(kv...), timing: existing.timing})
}

// current resolves the scope in play: the context's, else the process one.
func current(ctx context.Context) *scope {
	if ctx != nil {
		if carried, ok := ctx.Value(contextKey{}).(*scope); ok && carried != nil {
			return carried
		}
	}
	return processScope()
}
