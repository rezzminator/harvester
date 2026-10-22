package deps

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// RunOptions selects a working directory; an empty Dir inherits the caller's.
type RunOptions struct{ Dir string }

// RunResult is one command's completed run: stdout and stderr split (never
// merged the way probeOne's CombinedOutput is, because a Runner caller — git,
// systemctl, security — routinely needs to tell them apart) plus the
// process exit code, or -1 when the process never reached one (the binary
// could not start, the context was cancelled first).
type RunResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner owns process execution and executable lookup.
type Runner interface {
	Run(ctx context.Context, argv []string, opts RunOptions) (RunResult, error)
	LookPath(name string) (string, error)
	// Start returns a process with owned streams and lifecycle.
	Start(ctx context.Context, argv []string, opts StartOptions) (Process, error)
}

// StartOptions carries the environment, streams, and process-group policy.
type StartOptions struct {
	Env []string
	Dir string
	// WaitDelay retains exec.Cmd's guarantee that
	// inherited descriptors cannot strand Wait or its copy goroutines.
	WaitDelay time.Duration
	// Stdin is ordinary input wired to the child. It cannot be combined with
	// StdinPipe; use the latter when the caller owns an interactive stream.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// StdinPipe and StdoutPipe keep the native process pipes inside deps. The
	// returned Process exposes the corresponding handles without leaking
	// exec.Cmd to callers.
	StdinPipe  bool
	StdoutPipe bool
	// ProcessGroup gives the child its own process group so KillGroup can stop
	// descendants as well as the direct child.
	ProcessGroup bool
}

// Process exposes a running child's streams and lifecycle.
type Process interface {
	Pid() int
	Wait() error
	StdinPipe() (io.WriteCloser, error)
	StdoutPipe() (io.ReadCloser, error)
	Kill() error
	KillGroup() error
}

// RealRunner runs argv through os/exec.
type RealRunner struct{}

// Run starts argv[0] with argv[1:] as its arguments. A nonzero exit is the
// command answering, not the Runner failing to run it — Run returns a nil
// error with ExitCode set, the same contract probeOne already holds for a
// version-probe subprocess; only a failure to start (lookup, permission,
// context already done) returns a non-nil error.
func (RealRunner) Run(ctx context.Context, argv []string, opts RunOptions) (RunResult, error) {
	if len(argv) == 0 {
		return RunResult{ExitCode: -1}, errors.New("deps: RealRunner.Run: empty argv")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = opts.Dir
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	// A nil err IS the zero exit code — ExitCode(nil) answers -1 ("never
	// reached one"), which is right for a process that never ran but wrong
	// here: this command ran and exited 0.
	exitCode := 0
	if err != nil {
		exitCode = ExitCode(err)
	}
	result := RunResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitCode}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return result, fmt.Errorf("run %q: %w", argv[0], err)
	}
	return result, nil
}

// LookPath resolves name on $PATH exactly as exec.LookPath does.
func (RealRunner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// Start owns the child until Wait returns; cancellation closes it and its pipes.
func (RealRunner) Start(ctx context.Context, argv []string, opts StartOptions) (Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("deps: RealRunner.Start: empty argv")
	}
	if opts.Stdin != nil && opts.StdinPipe {
		return nil, errors.New("deps: StartOptions cannot combine Stdin and StdinPipe")
	}
	if opts.Stdout != nil && opts.StdoutPipe {
		return nil, errors.New("deps: StartOptions cannot combine Stdout and StdoutPipe")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if opts.ProcessGroup {
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	command.Env = opts.Env
	command.Dir = opts.Dir
	command.WaitDelay = opts.WaitDelay
	if opts.Stdin != nil {
		command.Stdin = opts.Stdin
	}
	command.Stdout = opts.Stdout
	command.Stderr = opts.Stderr
	var stdin io.WriteCloser
	var stdout io.ReadCloser
	var err error
	if opts.StdinPipe {
		stdin, err = command.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("open %q stdin pipe: %w", argv[0], err)
		}
	}
	if opts.StdoutPipe {
		stdout, err = command.StdoutPipe()
		if err != nil {
			if stdin != nil {
				_ = stdin.Close()
			}
			return nil, fmt.Errorf("open %q stdout pipe: %w", argv[0], err)
		}
	}
	if err := command.Start(); err != nil {
		if stdin != nil {
			_ = stdin.Close()
		}
		if stdout != nil {
			_ = stdout.Close()
		}
		return nil, fmt.Errorf("start %q: %w", argv[0], err)
	}
	return &realProcess{
		command:      command,
		stdin:        stdin,
		stdout:       stdout,
		processGroup: opts.ProcessGroup,
	}, nil
}

// realProcess wraps a started *exec.Cmd owned by the runner.
type realProcess struct {
	command      *exec.Cmd
	stdin        io.WriteCloser
	stdout       io.ReadCloser
	processGroup bool
}

func (p *realProcess) Pid() int { return p.command.Process.Pid }

func (p *realProcess) Wait() error { return p.command.Wait() }

func (p *realProcess) StdinPipe() (io.WriteCloser, error) {
	if p.stdin == nil {
		return nil, errors.New("deps: process stdin pipe was not requested")
	}
	return p.stdin, nil
}

func (p *realProcess) StdoutPipe() (io.ReadCloser, error) {
	if p.stdout == nil {
		return nil, errors.New("deps: process stdout pipe was not requested")
	}
	return p.stdout, nil
}

func (p *realProcess) Kill() error {
	if p.command.Process == nil {
		return errors.New("deps: process has not started")
	}
	return p.command.Process.Kill()
}

func (p *realProcess) KillGroup() error {
	if !p.processGroup {
		return errors.New("deps: process group was not requested")
	}
	if p.command.Process == nil {
		return errors.New("deps: process has not started")
	}
	err := syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// FakeRunner scripts Runner responses by argv prefix and records every call
// on a ledger. Script order does not matter: the LONGEST matching prefix
// wins, so a caller can script a broad default ("git") alongside a
// narrower override ("git" "push") without the narrower one being shadowed
// by registration order. A call matching no registered prefix returns
// UnscriptedError — never a silent empty RunResult a caller could mistake
// for "ran and produced nothing". LookPath likewise names an unscripted lookup.
type FakeRunner struct {
	mu           sync.Mutex
	scripts      []fakeScript
	calls        []RunCall
	startScripts []fakeStartScript
	starts       []StartCall
	lifecycle    []LifecycleCall
}

type fakeScript struct {
	prefix []string
	result RunResult
	err    error
}

// RunCall is one Run invocation FakeRunner recorded on its ledger.
type RunCall struct {
	Argv []string
	Opts RunOptions
}

// StartCall is one Start invocation FakeRunner recorded on its ledger —
// Calls' Start counterpart.
type StartCall struct {
	Argv []string
	Opts StartOptions
}

// LifecycleCall records a FakeRunner Process lifecycle operation in order.
// It is deliberately separate from StartCall so tests can assert cleanup
// happened after cancellation without inspecting a real process table.
type LifecycleCall struct {
	Pid    int
	Action string
}

type fakeStartScript struct {
	prefix   []string
	pid      int
	waitErr  error
	err      error
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	killErr  error
	groupErr error
}

// fakeProcess is the scripted Process FakeRunner.Start hands back: a fixed
// pid and a fixed Wait error, never a real process.
type fakeProcess struct {
	pid      int
	waitErr  error
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	killErr  error
	groupErr error
	owner    *FakeRunner
}

func (p *fakeProcess) Pid() int { return p.pid }
func (p *fakeProcess) Wait() error {
	p.owner.recordLifecycle(p.pid, "wait")
	return p.waitErr
}

func (p *fakeProcess) StdinPipe() (io.WriteCloser, error) {
	if p.stdin == nil {
		return nil, errors.New("deps: fake process stdin pipe was not scripted")
	}
	return p.stdin, nil
}

func (p *fakeProcess) StdoutPipe() (io.ReadCloser, error) {
	if p.stdout == nil {
		return nil, errors.New("deps: fake process stdout pipe was not scripted")
	}
	return p.stdout, nil
}

func (p *fakeProcess) Kill() error {
	p.owner.recordLifecycle(p.pid, "kill")
	return p.killErr
}

func (p *fakeProcess) KillGroup() error {
	p.owner.recordLifecycle(p.pid, "kill-group")
	return p.groupErr
}

func (f *FakeRunner) recordLifecycle(pid int, action string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lifecycle = append(f.lifecycle, LifecycleCall{Pid: pid, Action: action})
}

// InteractiveScript supplies the owned streams and lifecycle answers for a
// FakeRunner.Start call. A stream is only returned when the corresponding
// StartOptions pipe was requested; unscripted streaming starts fail loudly.
type InteractiveScript struct {
	Pid      int
	WaitErr  error
	StartErr error
	Stdin    io.WriteCloser
	Stdout   io.ReadCloser
	KillErr  error
	GroupErr error
}

// Script registers result/err for every Run call whose argv starts with
// prefix.
func (f *FakeRunner) Script(prefix []string, result RunResult, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, fakeScript{
		prefix: append([]string(nil), prefix...),
		result: result,
		err:    err,
	})
}

// Run records the call on the ledger, then answers with the longest
// registered prefix match, or UnscriptedError when nothing matches.
func (f *FakeRunner) Run(_ context.Context, argv []string, opts RunOptions) (RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, RunCall{Argv: append([]string(nil), argv...), Opts: opts})
	var best *fakeScript
	for index := range f.scripts {
		script := &f.scripts[index]
		if !argvHasPrefix(argv, script.prefix) {
			continue
		}
		if best == nil || len(script.prefix) > len(best.prefix) {
			best = script
		}
	}
	if best == nil {
		return RunResult{ExitCode: -1}, UnscriptedError{Argv: append([]string(nil), argv...)}
	}
	return best.result, best.err
}

// ScriptStart registers the Process a Start call whose argv starts with
// prefix gets back: pid, the error Process.Wait answers, and the error
// Start itself returns (non-nil err short-circuits before a Process is
// built, the same shape Script's err does for Run).
func (f *FakeRunner) ScriptStart(prefix []string, pid int, waitErr, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startScripts = append(f.startScripts, fakeStartScript{
		prefix:  append([]string(nil), prefix...),
		pid:     pid,
		waitErr: waitErr,
		err:     err,
	})
}

// ScriptInteractive registers a streaming Start response. It is separate
// from ScriptStart so waited callers keep their small
// API while interactive consumers can own deterministic pipes and lifecycle
// failures.
func (f *FakeRunner) ScriptInteractive(prefix []string, script InteractiveScript) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startScripts = append(f.startScripts, fakeStartScript{
		prefix: append([]string(nil), prefix...), pid: script.Pid, waitErr: script.WaitErr, err: script.StartErr,
		stdin: script.Stdin, stdout: script.Stdout, killErr: script.KillErr,
		groupErr: script.GroupErr,
	})
}

// Start records the call on the Start ledger, then answers with the
// longest registered ScriptStart match, or a default Process (pid 4242,
// Wait nil) when nothing was scripted — a caller that never cares about the
// spawned process's identity does not have to script one.
func (f *FakeRunner) Start(_ context.Context, argv []string, opts StartOptions) (Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if opts.Stdin != nil && opts.StdinPipe {
		return nil, errors.New("deps: StartOptions cannot combine Stdin and StdinPipe")
	}
	if opts.Stdout != nil && opts.StdoutPipe {
		return nil, errors.New("deps: StartOptions cannot combine Stdout and StdoutPipe")
	}
	f.starts = append(f.starts, StartCall{Argv: append([]string(nil), argv...), Opts: opts})
	var best *fakeStartScript
	for index := range f.startScripts {
		script := &f.startScripts[index]
		if !argvHasPrefix(argv, script.prefix) {
			continue
		}
		if best == nil || len(script.prefix) > len(best.prefix) {
			best = script
		}
	}
	if best == nil {
		if opts.StdinPipe || opts.StdoutPipe {
			return nil, UnscriptedError{Argv: append([]string(nil), argv...)}
		}
		return &fakeProcess{pid: 4242, owner: f}, nil
	}
	if best.err != nil {
		return nil, best.err
	}
	if (opts.StdinPipe && best.stdin == nil) || (opts.StdoutPipe && best.stdout == nil) {
		return nil, fmt.Errorf("deps: FakeRunner: interactive script for %q did not provide requested pipes", argv)
	}
	return &fakeProcess{
		pid: best.pid, waitErr: best.waitErr, stdin: best.stdin, stdout: best.stdout,
		killErr: best.killErr, groupErr: best.groupErr, owner: f,
	}, nil
}

// LifecycleCalls returns a copy of every FakeRunner Process lifecycle call.
func (f *FakeRunner) LifecycleCalls() []LifecycleCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LifecycleCall(nil), f.lifecycle...)
}

// Starts returns every Start call FakeRunner has recorded, in call order —
// Calls' Start counterpart, with the same copy-on-read isolation.
func (f *FakeRunner) Starts() []StartCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	starts := make([]StartCall, len(f.starts))
	for index := range f.starts {
		call := &f.starts[index]
		starts[index] = StartCall{Argv: append([]string(nil), call.Argv...), Opts: call.Opts}
	}
	return starts
}

// LookPath names a lookup the fixture did not script.
func (f *FakeRunner) LookPath(name string) (string, error) {
	return "", UnscriptedError{Argv: []string{name}}
}

// Calls returns every Run call FakeRunner has recorded, in call order. Each
// entry's Argv is its own copy — mutating a returned call never reaches the
// ledger, the same isolation Run already gives the recorded argv against
// the caller's own slice.
func (f *FakeRunner) Calls() []RunCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := make([]RunCall, len(f.calls))
	for index, call := range f.calls {
		calls[index] = RunCall{Argv: append([]string(nil), call.Argv...), Opts: call.Opts}
	}
	return calls
}

func argvHasPrefix(argv, prefix []string) bool {
	if len(prefix) > len(argv) {
		return false
	}
	for index, want := range prefix {
		if argv[index] != want {
			return false
		}
	}
	return true
}

// UnscriptedError names the argv a FakeRunner received with nothing
// registered for it.
type UnscriptedError struct {
	Argv []string
}

func (err UnscriptedError) Error() string {
	return fmt.Sprintf("deps: FakeRunner: unscripted call %q", err.Argv)
}
