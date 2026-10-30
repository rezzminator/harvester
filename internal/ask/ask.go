// Package ask renders evidence prompts and runs the configured local engine.
package ask

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/engine"
)

const engineTimeout = 60 * time.Second

type AskInput struct {
	ContentFiles []string
	SourceLabels []string
	Prompt       string
	Engine       engine.ID
	Model        string
	Effort       string
	Timeout      time.Duration
}

type TokenUsage struct {
	Input         int `json:"input_tokens"`
	CachedInput   int `json:"cached_input_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
	Output        int `json:"output_tokens"`
}

type AskResult struct {
	Answer   string
	Usage    *TokenUsage
	Duration time.Duration
}

type Engine interface {
	Run(context.Context, AskInput) (AskResult, error)
}

func ResolveInput(input AskInput, machine config.Config) (AskInput, error) {
	if len(input.ContentFiles) == 0 {
		return AskInput{}, fmt.Errorf("content files must not be empty")
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return AskInput{}, fmt.Errorf("prompt must not be empty")
	}
	for index, file := range input.ContentFiles {
		if strings.TrimSpace(file) == "" {
			return AskInput{}, fmt.Errorf("content file %d is empty", index+1)
		}
	}
	if len(input.SourceLabels) != 0 && len(input.SourceLabels) != len(input.ContentFiles) {
		return AskInput{}, fmt.Errorf(
			"source labels length %d does not match content files length %d",
			len(input.SourceLabels),
			len(input.ContentFiles),
		)
	}
	resolved := input
	if resolved.Engine == "" {
		var err error
		resolved.Engine, err = machine.DefaultEngine()
		if err != nil {
			return AskInput{}, err
		}
	}
	prefs := machine.Ask.PrefsFor(resolved.Engine)
	if resolved.Model == "" {
		resolved.Model = prefs.Model
	}
	if resolved.Effort == "" {
		resolved.Effort = prefs.Effort
	}
	if len(resolved.SourceLabels) == 0 {
		resolved.SourceLabels = append([]string(nil), resolved.ContentFiles...)
	}
	if resolved.Model != "" {
		var err error
		resolved.Model, err = CheckModel(resolved.Engine, resolved.Model)
		if err != nil {
			return AskInput{}, err
		}
	}
	return resolved, nil
}

func BuildPrompt(input AskInput) (string, error) {
	if len(input.ContentFiles) == 0 {
		return "", fmt.Errorf("content files must not be empty")
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return "", fmt.Errorf("prompt must not be empty")
	}
	if len(input.SourceLabels) != len(input.ContentFiles) {
		return "", fmt.Errorf(
			"source labels length %d does not match content files length %d",
			len(input.SourceLabels),
			len(input.ContentFiles),
		)
	}
	var builder strings.Builder
	builder.WriteString(
		"Read the prepared content files listed below. Work ONLY from them; no network access, no other files.\n",
	)
	for index, file := range input.ContentFiles {
		fmt.Fprintf(&builder, "%d. %s — source: %s\n", index+1, file, input.SourceLabels[index])
	}
	fmt.Fprintf(&builder, "TASK: %s\n", input.Prompt)
	builder.WriteString(
		"Rules: if a file is truncated or unusable, say so explicitly for that file instead of guessing.\n",
	)
	builder.WriteString(
		"After your answer, append a section titled exactly \"EVIDENCE\" listing one line per load-bearing claim:\n",
	)
	builder.WriteString("  [file N] <location: line range, turn number, or chunk id> — \"<short verbatim quote>\"")
	return builder.String(), nil
}

func ResolveEngine(id engine.ID, machine config.Config) (Engine, error) {
	runner, err := RunnerFor(id)
	if err != nil {
		return nil, err
	}
	return runner.Resolve(machine)
}
func ResolveClaude(machine config.Config) (Engine, error) {
	return resolveProcess(engine.Claude, machine)
}
func ResolveCodex(machine config.Config) (Engine, error) {
	return resolveProcess(engine.Codex, machine)
}

// claudeModelAliases are the model aliases Claude Code's --model takes for
// the latest model of a family (its --help names fable, opus and sonnet);
// claudeLongContext is the suffix it takes on an alias for the 1M-token
// context window (sonnet[1m]).
var claudeModelAliases = []string{"fable", "opus", "sonnet", "haiku"}

const claudeLongContext = "[1m]"

// CheckModel validates the engine family before any source is read. The engine
// remains the final judge of whether an accepted model id exists.
func CheckModel(id engine.ID, model string) (string, error) {
	descriptor, err := engine.Lookup(id)
	if err != nil {
		return "", err
	}
	model = strings.TrimSpace(model)
	if alias := strings.ToLower(model); id == engine.Claude && slices.Contains(claudeModelAliases, strings.TrimSuffix(alias, claudeLongContext)) {
		return alias, nil
	}
	if id == engine.Claude && strings.HasPrefix(model, "claude-") || id == engine.Codex && strings.HasPrefix(model, "gpt-") {
		return model, nil
	}
	if id == engine.Claude {
		return "", fmt.Errorf("model %q is not a claude model: name an alias (%s) or a model id starting claude-", model, strings.Join(claudeModelAliases, ", "))
	}
	return "", fmt.Errorf("model %q is not a %s model: name a model id starting gpt-", model, descriptor.LongName)
}
