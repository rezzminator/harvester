// Package doctor reports the standalone harvester's health.
package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/harvestpy"
	"github.com/rezzminator/harvester/internal/searxng"
)

// searchWait bounds the search rows: Docker's answers and one live JSON search.
const searchWait = 30 * time.Second

func Run(args []string, stdout, stderr io.Writer, runtime config.Runtime) int {
	flags := flag.NewFlagSet("harvester doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "print health rows as JSON")
	skipConversion := flags.Bool("skip-conversion", false, "exclude the optional conversion environment from health")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "harvester doctor: unexpected arguments")
		return 2
	}
	var rows bytes.Buffer
	fmt.Fprintf(&rows, "doctor: config harvester path=%s exists=%t\n", runtime.Config.Path, runtime.Config.Harvester.Exists)
	ctx, harvester, home := context.Background(), runtime.Config.Harvester, runtime.Paths.Home
	warnings := printHarvesterConfigDoctor(&rows, runtime)
	warnings += printBinaryDoctor(&rows, runtime)
	warnings += printDaemonDoctor(ctx, &rows, runtime)
	warnings += printHarvesterExternalDoctor(ctx, &rows, harvester)
	warnings += printHarvestCacheDoctor(&rows, harvester)
	backend := searxng.Resolve(home, harvester.Search)
	searchCtx, cancel := context.WithTimeout(ctx, searchWait)
	health := printHarvestSearchDoctor(searchCtx, &rows, harvester, backend.URL)
	warnings += health + printSearchDoctor(searchCtx, &rows, home, harvester.Search, backend, health != 0)
	cancel()
	printOutputDoctor(&rows, harvester)
	printToolsDoctor(&rows, home, harvester, backend)
	warnings += printLogsDoctor(&rows, home)
	if *skipConversion {
		fmt.Fprintln(&rows, "doctor: harvestpy skipped (--skip-conversion)")
	} else {
		warnings += printHarvestPythonDoctorWithRunner(ctx, &rows, home, harvestpy.Platform{}, configuredHarvestDoctor(), harvester.Fetch.Browser, nil)
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(struct {
			Healthy  bool     `json:"healthy"`
			Warnings int      `json:"warnings"`
			Rows     []string `json:"rows"`
		}{warnings == 0, warnings, strings.Split(strings.TrimSuffix(rows.String(), "\n"), "\n")}); err != nil {
			fmt.Fprintf(stderr, "harvester doctor: write JSON: %v\n", err)
			return 1
		}
	} else if _, err := io.Copy(stdout, &rows); err != nil {
		fmt.Fprintf(stderr, "harvester doctor: write report: %v\n", err)
		return 1
	}
	if warnings != 0 {
		return 1
	}
	return 0
}
