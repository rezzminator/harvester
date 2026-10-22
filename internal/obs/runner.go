package obs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/rezzminator/harvester/internal/deps"
)

// compRunner is the component every process door records under.
const compRunner = "runner"

// Runner decorates next with the runner component's records (spec
// § Middleware, `runner`): one record per Run, LookPath and Start, and one
// per terminal of a started Process — Wait, Kill, KillGroup. It
// never changes what next does: the argv, options, result and error pass
// through untouched. What is recorded is the argument SHAPE — argv[0]'s
// base name and argc — never an argument, so a token on a command line
// cannot reach the file. nil next wraps deps.RealRunner.
func Runner(next deps.Runner) deps.Runner {
	if next == nil {
		next = deps.RealRunner{}
	}
	return loggedRunner{next: next}
}

type loggedRunner struct{ next deps.Runner }

// Run records runner.run: INFO with the exit code when the command ran
// (a non-zero exit is the command answering, per deps.Runner's contract),
// ERROR with exit -1 when it never started — INFO when its caller cancelled
// it (processLevel).
func (runner loggedRunner) Run(ctx context.Context, argv []string, opts deps.RunOptions) (deps.RunResult, error) {
	started := current(ctx).timing.Now()
	result, err := runner.next.Run(ctx, argv, opts)
	exit := result.ExitCode
	if err != nil {
		exit = -1
	}
	attrs := append(argvShape(argv), slog.Int(FieldExit, exit))
	record(ctx, compRunner, "runner.run", processLevel(err), started, err, attrs...)
	return result, err
}

// LookPath records a binary lookup against the process logger, with WARN on a miss.
func (runner loggedRunner) LookPath(name string) (string, error) {
	ctx := context.Background()
	started := current(ctx).timing.Now()
	path, err := runner.next.LookPath(name)
	record(ctx, compRunner, "runner.lookpath", errorLevel(err, slog.LevelWarn), started, err,
		slog.String("argv", name), slog.String("path", path))
	return path, err
}

// Start records runner.start with the pid, then hands back a Process whose
// terminals record their own result; a Start that fails is one ERROR record
// (INFO when its caller had cancelled it, processLevel).
func (runner loggedRunner) Start(ctx context.Context, argv []string, opts deps.StartOptions) (deps.Process, error) {
	started := current(ctx).timing.Now()
	process, err := runner.next.Start(ctx, argv, opts)
	attrs := argvShape(argv)
	if err != nil {
		record(ctx, compRunner, "runner.start", processLevel(err), started, err, attrs...)
		return nil, err
	}
	attrs = append(attrs, slog.Int(FieldPID, process.Pid()))
	record(ctx, compRunner, "runner.start", slog.LevelInfo, started, nil, attrs...)
	return &loggedProcess{ctx: ctx, next: process, started: started}, nil
}

// loggedProcess is the started child seen through the runner component:
// every terminal writes one record carrying the pid and dur_ms since start.
type loggedProcess struct {
	ctx     context.Context
	next    deps.Process
	started time.Time
}

func (process *loggedProcess) Pid() int { return process.next.Pid() }

// Wait records runner.exit: the child's exit code at INFO when it exited
// non-zero (an *exec.ExitError is the child answering), exit -1 for any other
// error — ERROR, or INFO when its caller cancelled the child (processLevel).
func (process *loggedProcess) Wait() error {
	err := process.next.Wait()
	exit, level := 0, slog.LevelInfo
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		exit = exitErr.ExitCode()
	default:
		exit, level = -1, processLevel(err)
	}
	process.terminal("runner.exit", level, err, slog.Int(FieldExit, exit))
	return err
}

// StdinPipe and StdoutPipe hand the child's pipes straight through: the
// bytes on them are never the log's business.
func (process *loggedProcess) StdinPipe() (io.WriteCloser, error) { return process.next.StdinPipe() }

func (process *loggedProcess) StdoutPipe() (io.ReadCloser, error) { return process.next.StdoutPipe() }

// Kill and KillGroup forward to the inner Process and record the terminal
// either way, so a kill that failed is an ERROR naming the pid.
func (process *loggedProcess) Kill() error {
	err := process.next.Kill()
	process.terminal("runner.kill", errorLevel(err), err)
	return err
}

func (process *loggedProcess) KillGroup() error {
	err := process.next.KillGroup()
	process.terminal("runner.killgroup", errorLevel(err), err)
	return err
}

func (process *loggedProcess) terminal(op string, level slog.Level, err error, attrs ...slog.Attr) {
	record(process.ctx, compRunner, op, level, process.started, err,
		append([]slog.Attr{slog.Int(FieldPID, process.Pid())}, attrs...)...)
}

// argvShape is the only thing a process door tells the log about its command
// line: the binary's base name and how many words followed it.
func argvShape(argv []string) []slog.Attr {
	name := ""
	if len(argv) != 0 {
		name = filepath.Base(argv[0])
	}
	return []slog.Attr{slog.String("argv", name), slog.Int("argc", len(argv))}
}

// processLevel is the level a process door records err at: INFO for nil and
// for a context.Canceled — the caller ended the child on purpose, and
// exec.Cmd.Wait reports ctx.Err() even for a child that then exited cleanly —
// ERROR for every other failure to start, run or wait, a deadline that cut
// the child off included.
func processLevel(err error) slog.Level {
	if errors.Is(err, context.Canceled) {
		return slog.LevelInfo
	}
	return errorLevel(err)
}

// errorLevel picks the level a record takes: INFO when err is nil, else the
// failure level (ERROR unless the caller names a softer one).
func errorLevel(err error, failure ...slog.Level) slog.Level {
	if err == nil {
		return slog.LevelInfo
	}
	if len(failure) != 0 {
		return failure[0]
	}
	return slog.LevelError
}

// record writes one middleware record for comp: op as the message, dur_ms
// since started on the scope's clock, err when there is one, plus attrs. It
// is the one shape every door in this package writes.
func record(ctx context.Context, comp, op string, level slog.Level, started time.Time, err error, attrs ...slog.Attr) {
	scoped := Component(ctx, comp)
	existing := current(scoped)
	attrs = append(attrs, slog.Int64(FieldDur, existing.timing.Now().Sub(started).Milliseconds()))
	if err != nil {
		attrs = append(attrs, slog.String(FieldErr, err.Error()))
	}
	existing.logger.LogAttrs(scoped, level, op, attrs...)
}
