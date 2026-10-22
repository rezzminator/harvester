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

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/harvestpy"
)

func Run(args []string, stdout, stderr io.Writer, runtime config.Runtime) int {
	flags := flag.NewFlagSet("harvester doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "print health rows as JSON")
	skipConversion := flags.Bool("skip-conversion", false, "exclude the optional conversion environment from health")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "harvester doctor: unexpected arguments")
		return 2
	}
	var rows bytes.Buffer
	fmt.Fprintf(&rows, "doctor: config harvester path=%s exists=%t\n", runtime.Config.Path, runtime.Config.Harvester.Exists)
	warnings := printHarvesterConfigDoctor(&rows, runtime)
	warnings += printHarvesterExternalDoctor(context.Background(), &rows, runtime.Config.Harvester)
	warnings += printHarvestCacheDoctor(&rows, runtime.Config.Harvester)
	warnings += printHarvestSearchDoctor(context.Background(), &rows, runtime.Config.Harvester)
	if *skipConversion {
		fmt.Fprintln(&rows, "doctor: harvestpy skipped (--skip-conversion)")
	} else {
		warnings += printHarvestPythonDoctorWithRunner(context.Background(), &rows, runtime.Paths.Home, harvestpy.Platform{}, configuredHarvestDoctor(), runtime.Config.Harvester.Fetch.Browser, nil)
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
