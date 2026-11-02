package ask

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/rezzminator/harvester/internal/clock"
	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/deps"
	"github.com/rezzminator/harvester/internal/engine"
	"github.com/rezzminator/harvester/internal/obs"
)

// BinaryMissingError distinguishes lookup failure from an engine crash.
type BinaryMissingError struct {
	Engine, Binary string
	Err            error
}

func (err *BinaryMissingError) Error() string {
	return fmt.Sprintf("%s binary MISSING (%s)", err.Engine, err.Binary)
}
func (err *BinaryMissingError) Unwrap() error { return err.Err }

type processEngine struct {
	binary string
	engine engine.ID
}

func resolveProcess(id engine.ID, machine config.Config) (Engine, error) {
	descriptor, err := engine.Lookup(id)
	if err != nil {
		return nil, err
	}
	binary := descriptor.Binary
	switch id {
	case engine.Claude:
		if machine.Claude.Binary != "" {
			binary = machine.Claude.Binary
		}
	case engine.Codex:
		if machine.Codex.Binary != "" {
			binary = machine.Codex.Binary
		}
	default:
		return nil, fmt.Errorf("%s does not support ask", descriptor.Short)
	}
	path, err := obs.Runner(deps.RealRunner{}).LookPath(binary)
	if err != nil {
		return nil, &BinaryMissingError{Engine: descriptor.LongName, Binary: binary, Err: err}
	}
	return processEngine{binary: path, engine: id}, nil
}
func (runner processEngine) Run(parent context.Context, input AskInput) (AskResult, error) {
	prompt, err := BuildPrompt(input)
	if err != nil {
		return AskResult{}, err
	}
	if input.Model != "" {
		input.Model, err = CheckModel(runner.engine, input.Model)
		if err != nil {
			return AskResult{}, err
		}
	}
	timeout := input.Timeout
	if timeout == 0 {
		timeout = engineTimeout
	}
	if timeout < 0 {
		return AskResult{}, fmt.Errorf("timeout must be zero or positive")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	args := []string{runner.binary}
	if runner.engine == engine.Claude {
		args = append(args, "-p")
		if input.Model != "" {
			args = append(args, "--model", input.Model)
		}
		if input.Effort != "" {
			args = append(args, "--effort", input.Effort)
		}
		// A one-shot ask saves no session: it would stay listed as a resumable chat, as Codex's --ephemeral avoids.
		args = append(args, "--output-format", "json", "--no-session-persistence")
	} else {
		args = append(args, "exec")
		if input.Model != "" {
			args = append(args, "--model", input.Model)
		}
		if input.Effort != "" {
			args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(input.Effort))
		}
		args = append(args, "--json", "--ephemeral", "--skip-git-repo-check", "--color", "never")
	}
	streams := make([]*os.File, 0, 3)
	defer func() {
		for _, stream := range streams {
			_ = stream.Close() // unlinked: closing frees it, nothing is left to lose
		}
	}()
	for _, content := range []string{prompt, "", ""} {
		stream, err := engineStream(content)
		if err != nil {
			return AskResult{}, err
		}
		streams = append(streams, stream)
	}
	started := clock.Real.Now()
	runErr := runProcess(ctx, args, deps.StartOptions{Env: processEnvironment(), Stdin: streams[0], Stdout: streams[1], Stderr: streams[2], ProcessGroup: true})
	name := engine.MustLookup(runner.engine).LongName
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return AskResult{}, fmt.Errorf("%s ask timed out: %w", name, context.DeadlineExceeded)
		}
		return AskResult{}, fmt.Errorf("%s ask canceled: %w", name, ctx.Err())
	}
	stdout, stdoutTruncated, err := captured(streams[1])
	if err != nil {
		return AskResult{}, fmt.Errorf("%s ask failed: %w", name, err)
	}
	if stdoutTruncated {
		return AskResult{}, fmt.Errorf("%s ask failed: normalized headless output exceeded the %d-byte capture limit", name, maxCapturedOutput)
	}
	log := obs.Logger(ctx)
	// stderr is diagnostics only: neither its overflow nor its loss ever costs the answer on stdout.
	stderr, stderrTruncated, err := captured(streams[2])
	if err != nil {
		log.Warn("ask.stderr.unread", "reason", fmt.Sprintf("%s stderr: %v", name, err))
	}
	if stderrTruncated {
		log.Warn("ask.stderr.truncated", "reason", fmt.Sprintf("%s stderr exceeded the %d-byte capture limit; the rest was not kept", name, maxCapturedOutput))
	}
	if runErr != nil {
		return AskResult{}, fmt.Errorf("%s ask failed: %w; stderr tail %q; stdout tail %q", name, runErr, boundedTail(stderr, 1024), boundedTail(stdout, 1024))
	}
	output := processOutput{Stdout: stdout}
	err = parseOutput(&output, runner.engine)
	for _, warning := range output.Warnings {
		log.Warn("ask.output.partial", "reason", name+": "+warning)
	}
	if err != nil {
		return AskResult{}, fmt.Errorf("%s ask failed: %w", name, err)
	}
	return AskResult{Answer: output.Answer, Usage: output.Usage, Duration: clock.Real.Now().Sub(started)}, nil
}

type processOutput struct {
	Stdout, Answer string
	Usage          *TokenUsage
	IsError        bool
	Warnings       []string // what the parse could not read and skipped, the answer kept
}

func boundedTail(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[len(value)-limit:]
	}
	return value
}

func processEnvironment() []string {
	environment := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION", "CODEX_THREAD_ID", "TMUX", "TMUX_PANE":
			continue
		}
		if strings.HasPrefix(name, "HERDR_") {
			continue
		}
		environment = append(environment, entry)
	}
	return environment
}
func runProcess(ctx context.Context, argv []string, options deps.StartOptions) error {
	process, err := obs.Runner(deps.RealRunner{}).Start(ctx, argv, options)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		killErr := process.KillGroup()
		if killErr != nil {
			killErr = errors.Join(killErr, process.Kill())
		}
		waitErr := <-done
		if killErr != nil {
			return errors.Join(waitErr, fmt.Errorf("kill ask process group: %w", killErr))
		}
		return waitErr
	}
}

const maxCapturedOutput = 8 << 20

// engineStream is one of the engine's standard streams: an unlinked temporary
// file holding content, which the engine reads or writes directly. Its whole
// output is on hand the moment it exits: no pipe-copy goroutine that a loaded
// scheduler leaves unrun past exec's WaitDelay, which closed the pipe on an
// answer already written, and no pipe a lingering descendant holds open.
func engineStream(content string) (*os.File, error) {
	stream, err := os.CreateTemp("", "harvester-ask-")
	if err != nil {
		return nil, fmt.Errorf("create an engine stream: %w", err)
	}
	if err := os.Remove(stream.Name()); err != nil {
		return nil, errors.Join(fmt.Errorf("unlink engine stream %s: %w", stream.Name(), err), stream.Close())
	}
	if _, err := stream.WriteString(content); err != nil {
		return nil, errors.Join(fmt.Errorf("write engine stream %s: %w", stream.Name(), err), stream.Close())
	}
	if _, err := stream.Seek(0, io.SeekStart); err != nil {
		return nil, errors.Join(fmt.Errorf("rewind engine stream %s: %w", stream.Name(), err), stream.Close())
	}
	return stream, nil
}

// captured is what the engine wrote to stream, its first maxCapturedOutput
// bytes, and whether it wrote more.
func captured(stream *os.File) (string, bool, error) {
	raw, err := io.ReadAll(io.NewSectionReader(stream, 0, maxCapturedOutput+1))
	if err != nil {
		return "", false, fmt.Errorf("read engine stream %s: %w", stream.Name(), err)
	}
	if len(raw) > maxCapturedOutput {
		return string(raw[:maxCapturedOutput]), true, nil
	}
	return string(raw), false, nil
}
