package ask

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

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
		args = append(args, "--output-format", "json")
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
	started := clock.Real.Now()
	var stdout, stderr boundedBuffer
	runErr := runProcess(ctx, args, deps.StartOptions{Env: processEnvironment(), Stdin: strings.NewReader(prompt), Stdout: &stdout, Stderr: &stderr, ProcessGroup: true, WaitDelay: 500 * time.Millisecond})
	name := engine.MustLookup(runner.engine).LongName
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return AskResult{}, fmt.Errorf("%s ask timed out: %w", name, context.DeadlineExceeded)
		}
		return AskResult{}, fmt.Errorf("%s ask canceled: %w", name, ctx.Err())
	}
	if stdout.truncated || stderr.truncated {
		return AskResult{}, fmt.Errorf("%s ask failed: normalized headless output exceeded the %d-byte capture limit", name, maxCapturedOutput)
	}
	if runErr != nil {
		return AskResult{}, fmt.Errorf("%s ask failed: %w; stderr tail %q; stdout tail %q", name, runErr, boundedTail(stderr.String(), 1024), boundedTail(stdout.String(), 1024))
	}
	output := processOutput{Stdout: stdout.String()}
	if err := parseOutput(&output, runner.engine); err != nil {
		return AskResult{}, fmt.Errorf("%s ask failed: %w", name, err)
	}
	return AskResult{Answer: output.Answer, Usage: output.Usage, Duration: clock.Real.Now().Sub(started)}, nil
}

type processOutput struct {
	Stdout, Answer string
	Usage          *TokenUsage
	IsError        bool
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

type boundedBuffer struct {
	bytes.Buffer
	truncated bool
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	remaining := maxCapturedOutput - buffer.Len()
	if remaining <= 0 {
		buffer.truncated = true
		return len(value), nil
	}
	if len(value) > remaining {
		_, _ = buffer.Buffer.Write(value[:remaining])
		buffer.truncated = true
		return len(value), nil
	}
	return buffer.Buffer.Write(value)
}
