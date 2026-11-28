// Package harvestcli owns the `harvester` verbs — the universal read, `ask`,
// `download-file` and `search` — as the command-line face of the same
// Harvester core the MCP tools serve; cmd/harvester keeps only the dispatch into
// Harvest.
package harvestcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/rezzminator/harvester/internal/cli"
	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/harvest"
	"github.com/rezzminator/harvester/internal/harvestmcp"
)

const (
	askVerb          = "ask"
	downloadFileVerb = "download-file"
	searchVerb       = "search"
	jsonFlag         = "json"
	maxSources       = 50
	readUsage        = "usage: harvester [--refresh] [--include-content=false] [--offset N] [--ocr-language latin|zh|ja|ar|ru|he] [--json] [--header 'Name: value']... <url|path|identifier>...\n" +
		"       harvester download-file [--json] [--header 'Name: value']... <url>...\n" +
		"       harvester search [--type any|paper|book] [--limit N] [--json] <query>...\n" +
		"       harvester ask -p <prompt> [--engine claude|codex] [--model MODEL] [--effort EFFORT] [--refresh] <url|path|identifier>..."
)

// newHarvester builds the configured harvester every verb reads through; a
// test swaps it for one whose transports reach a local fixture server.
var newHarvester = harvestmcp.NewHarvester

// Harvest dispatches `harvester`: `ask` (and its `--ask` alias),
// `download-file`, `search`, and otherwise the universal read of each source,
// printed in the order supplied. The retired `download` verb is a named usage
// error, never a source to read.
func Harvest(args []string, stdout, stderr io.Writer, runtime config.Runtime) int {
	if len(args) != 0 {
		switch args[0] {
		case askVerb:
			return runAsk(args[1:], stdout, stderr, runtime)
		case downloadFileVerb:
			return runDownload(args[1:], stdout, stderr, runtime)
		case searchVerb:
			return runSearch(args[1:], stdout, stderr, runtime)
		case "download":
			fmt.Fprintf(
				stderr,
				"harvester: unknown verb %q; a file's bytes come from `harvester download-file <url>...`\n",
				args[0],
			)
			return 2
		}
	}
	if askArgs, ok := askAlias(args); ok {
		return runAsk(askArgs, stdout, stderr, runtime)
	}
	return runRead(args, stdout, stderr, runtime)
}

// harvesterRuntime resolves this machine's harvester config into the MCP
// adapter's runtime (harvestmcp.RuntimeFromConfig).
func harvesterRuntime(runtime config.Runtime) harvestmcp.Runtime {
	return harvestmcp.RuntimeFromConfig(runtime.Paths.Home, runtime.Config.Harvester)
}

// runRead is the universal read: a URL, local path or identifier routed the
// way the read tool routes it.
func runRead(args []string, stdout, stderr io.Writer, runtime config.Runtime) int {
	flags := cli.NewFlagSet("harvest", readUsage, stderr)
	refresh := flags.Bool("refresh", false, "bypass the cache and fetch fresh content")
	includeContent := flags.Bool(
		"include-content",
		true,
		"print each source's content; false reads and caches it and prints only its size and cache path",
	)
	ocrLanguageFlag := flags.String(
		"ocr-language",
		"",
		"script to OCR a scanned document in (latin, zh, ja, ar, ru, he); a fresh read",
	)
	offset := flags.Int(
		"offset",
		0,
		"characters of the one source's content to skip: the offset a truncated: line names continues it",
	)
	jsonOutput := flags.Bool(jsonFlag, false, "print machine-readable result objects")
	headers := harvest.HeaderFlag(flags)
	sources, code, ok := cli.ParseFlagsAnywhere(flags, args)
	if !ok {
		return code
	}
	if len(sources) == 0 || len(sources) > maxSources {
		flags.Usage()
		return 2
	}
	switch {
	case *offset < 0:
		fmt.Fprintf(stderr, "harvester: --offset must be 0 or more, got %d\n", *offset)
		return 2
	case *offset > 0 && len(sources) > 1:
		fmt.Fprintf(stderr, "harvester: --offset takes exactly one source: it pages one item's content; got %d\n",
			len(sources))
		return 2
	}
	ocrLanguage, err := harvest.ParseOCRLang(*ocrLanguageFlag)
	if err != nil {
		fmt.Fprintf(stderr, "harvester: --ocr-language: %v\n", err)
		return 2
	}
	harvester, err := newHarvester(harvesterRuntime(runtime))
	if err != nil {
		fmt.Fprintf(stderr, "harvester: configure: %v\n", err)
		return 1
	}
	configured := harvesterRuntime(runtime)
	view := harvestmcp.ReadView{
		SizeOnly: !*includeContent, InlineCap: configured.MaxInlineChars, Budget: configured.MaxResultBytes,
	}
	results := make([]harvest.Result, 0, len(sources))
	for _, source := range sources {
		results = append(results, headers.Fetch(
			context.Background(),
			harvester,
			source,
			harvest.FetchOptions{
				Refresh: *refresh, SizeOnly: !*includeContent, OCRLang: ocrLanguage, Offset: *offset,
			},
		))
	}
	// The read tool's one rendering: every block within the answer budget.
	blocks, failed, pages := harvestmcp.RenderReadItems(sources, results, view)
	printedFailure := false // a block printed as an error line exits nonzero, whatever the result's Error
	for index, block := range blocks {
		printedFailure = printedFailure || failed[index]
		if !*jsonOutput {
			if index > 0 {
				fmt.Fprintln(stdout)
			}
			fmt.Fprintln(stdout, block)
		}
	}
	if *jsonOutput {
		encoded, err := json.MarshalIndent(jsonResults(results, failed, pages, view.SizeOnly), "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "harvester: encode results: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(encoded))
	}
	if printedFailure {
		return 1
	}
	for index := range results {
		result := &results[index]
		if result.Error != "" {
			return 1
		}
	}
	return 0
}

// jsonResults is results for --json, each content the page its text block
// shows: a cut page ends with harvest.InlineTruncationNote and names the
// offset that continues it (next_offset); a withheld one has neither.
func jsonResults(
	results []harvest.Result, failed []bool, pages []harvestmcp.ReadPage, sizeOnly bool,
) []harvest.JSONResult {
	out := harvest.JSONResults(results)
	for index := range out {
		if failed[index] || sizeOnly {
			continue
		}
		page := pages[index]
		if !page.Cut { // whole: the content as read
			continue
		}
		out[index].Content = ""
		if page.Withheld { // no page shown, so no offset continues one
			continue
		}
		next := page.End
		out[index].NextOffset = &next
		out[index].Content = page.Content + harvest.InlineTruncationNote
	}
	return out
}
