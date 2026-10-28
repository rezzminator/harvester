package harvestcli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/rezzminator/harvester/internal/ask"
	"github.com/rezzminator/harvester/internal/cli"
	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/engine"
	"github.com/rezzminator/harvester/internal/harvest"
	"github.com/rezzminator/harvester/internal/harvestmcp"
)

// askAlias keeps the originally attempted `harvester --ask -p ...`
// spelling useful while `harvester ask -p ...` remains the canonical form.
func askAlias(args []string) ([]string, bool) {
	for index, argument := range args {
		if argument != "--ask" {
			continue
		}
		result := make([]string, 0, len(args)-1)
		result = append(result, args[:index]...)
		result = append(result, args[index+1:]...)
		return result, true
	}
	return nil, false
}

func runAsk(args []string, stdout, stderr io.Writer, runtime config.Runtime) int {
	flags := cli.NewFlagSet(
		"harvest ask",
		"usage: harvester ask -p <prompt> [--engine claude|codex] [--model MODEL] [--effort EFFORT] [--refresh] <url|doi|path>...",
		stderr,
	)
	prompt := flags.String("prompt", "", "question answered only from the harvested sources")
	flags.StringVar(prompt, "p", "", "question answered only from the harvested sources")
	engineName := flags.String("engine", "", "ask engine (claude or codex; default from config)")
	model := flags.String("model", "", "override the configured ask model")
	effort := flags.String("effort", "", "override the configured reasoning effort")
	refresh := flags.Bool("refresh", false, "bypass the harvest cache")
	sources, code, ok := cli.ParseFlagsAnywhere(flags, args)
	if !ok {
		return code
	}
	if strings.TrimSpace(*prompt) == "" || len(sources) == 0 || len(sources) > maxSources {
		flags.Usage()
		return 2
	}

	engineID := engine.ID("")
	var err error
	if strings.TrimSpace(*engineName) != "" {
		engineID, err = engine.Parse(*engineName)
	} else {
		engineID, err = runtime.Config.DefaultEngine()
	}
	if err != nil {
		fmt.Fprintf(stderr, "harvester ask: resolve engine: %v\n", err)
		return 2
	}
	if strings.TrimSpace(*model) == "" {
		*model = runtime.Config.Ask.PrefsFor(engineID).Model
	}
	if strings.TrimSpace(*model) != "" {
		*model, err = ask.CheckModel(engineID, *model)
		if err != nil {
			fmt.Fprintf(stderr, "harvester ask: %v\n", err)
			return 2
		}
	}

	runner, err := ask.ResolveEngine(engineID, runtime.Config)
	if err != nil {
		fmt.Fprintf(stderr, "harvester ask: resolve %s engine: %v\n", engineID, err)
		return 1
	}

	harvester, err := newHarvester(harvesterRuntime(runtime))
	if err != nil {
		fmt.Fprintf(stderr, "harvester ask: configure harvester: %v\n", err)
		return 1
	}
	results := make([]harvest.Result, 0, len(sources))
	for _, source := range sources {
		results = append(results, harvester.FetchPublic(
			context.Background(),
			source,
			harvest.FetchOptions{Refresh: *refresh, SizeOnly: true},
		))
	}
	answer, err := harvestmcp.AskOver(context.Background(), runtime.Config, runtime.Paths.Home, runner, ask.AskInput{
		Prompt: *prompt, Engine: engineID, Model: *model, Effort: *effort,
	}, sources, results)
	if err != nil {
		fmt.Fprintf(stderr, "harvester ask: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, answer.Answer)
	if answer.Usage != nil {
		fmt.Fprintf(
			stderr,
			"harvester ask: usage input=%d cached_input=%d cache_creation=%d output=%d\n",
			answer.Usage.Input,
			answer.Usage.CachedInput,
			answer.Usage.CacheCreation,
			answer.Usage.Output,
		)
	} else {
		fmt.Fprintln(stderr, "harvester ask: usage unknown: the engine reported no token counts")
	}
	return 0
}
