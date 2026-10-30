package obs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/rezzminator/harvester/internal/clock"
	"github.com/rezzminator/harvester/internal/paths"
)

// AlphaSuffix marks a pre-release build. A build carrying it logs at debug by
// default — an alpha tester's bug report is only as good as what harvester wrote
// while it happened — and a release build logs at info.
const AlphaSuffix = "-alpha"

// AlphaBuild reports whether version, the stamped build version, is a
// pre-release: the one predicate every build-dependent default reads.
func AlphaBuild(version string) bool {
	return strings.HasSuffix(strings.TrimSpace(version), AlphaSuffix)
}

// Settings is what a process entry tells the activity log: which verb is
// running, which build, and the machine's log policy from harvester.config.json
// (Level and Components as § Control spells them). Everything else has a
// default: Dir falls back to paths.LogDir, Env to the real process, Timing to
// the wall clock, Stderr to os.Stderr.
type Settings struct {
	Cmd        string
	Version    string
	Level      string
	Components map[string]string
	Dir        string
	Env        paths.Env
	Timing     clock.Clock
	Stderr     io.Writer
}

// process holds the logger OpenLog installed. Until then every Logger call
// answers with a discarding logger — never nil, so a package that logs before
// the process opened one (a unit test, a library caller) is silent rather
// than broken.
var (
	processMutex sync.Mutex
	process      = &scope{logger: slog.New(discardHandler{}), timing: clock.Real}
)

// processScope returns the installed process scope.
func processScope() *scope {
	processMutex.Lock()
	defer processMutex.Unlock()
	return process
}

// OpenLog installs the process logger and writes the cmd.start record. The
// returned context carries the same logger for every span below it; the
// returned function writes cmd.exit with the process's exit code and total
// duration — call it exactly once, at process exit.
//
// A destination that cannot be resolved or opened is NOT silent: the reason
// goes to Settings.Stderr and the process keeps running against a discarding
// logger, because losing the activity log is never a reason to fail a verb.
// A global level of `off` is the one quiet case: nothing is recorded and no
// file is created.
func OpenLog(ctx context.Context, settings Settings) (context.Context, func(exitCode int)) {
	env := settings.Env
	if env == nil {
		env = paths.OSEnv{}
	}
	timing := settings.Timing
	if timing == nil {
		timing = clock.Real
	}
	stderr := settings.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	levels, levelErr := ResolveLevels(
		Policy{Version: settings.Version, Level: settings.Level, Components: settings.Components}, env,
	)
	if levelErr != nil {
		fmt.Fprintf(stderr, "harvester: activity log: %v\n", levelErr)
	}
	destination := slog.Handler(discardHandler{})
	if levels.Global.Level != LevelOff {
		var openErr error
		destination, openErr = openDestination(settings, stderr)
		if openErr != nil {
			fmt.Fprintf(stderr, "harvester: activity log unavailable: %v\n", openErr)
		}
		destination = levelHandler{next: destination, levels: levels}
	}
	logger := slog.New(destination).With(
		FieldCmd, settings.Cmd,
		FieldPID, os.Getpid(),
		FieldVersion, settings.Version,
	)
	opened := &scope{logger: logger, timing: timing}
	processMutex.Lock()
	process = opened
	processMutex.Unlock()
	started := timing.Now()
	logger.LogAttrs(ctx, slog.LevelInfo, "cmd.start")
	scoped := context.WithValue(ctx, contextKey{}, opened)
	return scoped, func(exitCode int) {
		logger.LogAttrs(
			scoped,
			slog.LevelInfo,
			"cmd.exit",
			slog.Int(FieldExit, exitCode),
			slog.Int64(FieldDur, timing.Now().Sub(started).Milliseconds()),
		)
	}
}

// openDestination builds the handler records are written through: the level files of the plugin log
// directory (§ Logging). The destination takes every level — levelHandler in front of it owns the control
// — and applies Scrub last. A directory that cannot be resolved returns the discarding handler AND the
// reason — the caller reports it, so a log nobody can write never reads as a quiet one. A directory that
// cannot be written is reported by the plugin-log library itself, once per failing streak.
func openDestination(settings Settings, stderr io.Writer) (slog.Handler, error) {
	dir := settings.Dir
	if dir == "" {
		resolved, err := paths.Resolve()
		if err != nil {
			return discardHandler{}, fmt.Errorf("resolve log directory: %w", err)
		}
		dir = resolved.LogDir
	}
	options := &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: Scrub}
	return slog.NewJSONHandler(newLogRows(dir, stderr), options), nil
}

// discardHandler drops every record: the destination before OpenLog, and the
// one left when the file could not be opened.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }
