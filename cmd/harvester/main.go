// Command harvester serves the standalone document and web harvester.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/rezzminator/harvester/internal/ask"
	"github.com/rezzminator/harvester/internal/cli"
	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/daemon"
	"github.com/rezzminator/harvester/internal/doctor"
	"github.com/rezzminator/harvester/internal/engine"
	claudeengine "github.com/rezzminator/harvester/internal/engine/claude"
	codexengine "github.com/rezzminator/harvester/internal/engine/codex"
	"github.com/rezzminator/harvester/internal/harvestcli"
	"github.com/rezzminator/harvester/internal/obs"
	"github.com/rezzminator/harvester/internal/paths"
	"github.com/rezzminator/harvester/internal/setup"
)

var version = "0.1.0-dev.1"

func init() {
	ask.RegisterRunner(engine.Claude, claudeengine.AskRunner{})
	ask.RegisterRunner(engine.Codex, codexengine.AskRunner{})
}

func main() {
	ctx := context.Background()
	if len(os.Args) > 1 && (os.Args[1] == "mcp" || os.Args[1] == "serve" || os.Args[1] == "setup") {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
	}
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version") {
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: harvester version")
			return 2
		}
		fmt.Fprintln(stdout, "harvester", version)
		return 0
	}
	if len(args) > 0 && args[0] == "config" && len(args) == 2 && args[1] == "path" {
		home, err := paths.Home()
		if err != nil {
			fmt.Fprintln(stderr, "harvester config:", err)
			return 1
		}
		fmt.Fprintln(stdout, config.Path(home))
		return 0
	}
	runtime, err := config.LoadRuntime("")
	if err != nil {
		fmt.Fprintln(stderr, "harvester:", err)
		return 1
	}
	runtime.Version = version
	ctx, closeLog := obs.OpenLog(ctx, obs.Settings{Cmd: "harvester", Version: version, Stderr: stderr})
	defer func() { closeLog(code) }()
	if len(args) > 0 {
		switch args[0] {
		case "mcp":
			if len(args) != 1 {
				fmt.Fprintln(stderr, "usage: harvester mcp")
				return 2
			}
			err := serveStdio(ctx, runtime, stdin, stdout, stderr)
			if errors.Is(err, context.Canceled) {
				return 0
			}
			if err != nil {
				fmt.Fprintln(stderr, "harvester mcp:", err)
				return 1
			}
			return 0
		case "serve":
			if len(args) != 1 {
				fmt.Fprintln(stderr, "usage: harvester serve")
				return 2
			}
			return serveGateway(ctx, runtime, stdout, stderr)
		case "status":
			if len(args) > 2 || (len(args) == 2 && args[1] != "--json") {
				fmt.Fprintln(stderr, "usage: harvester status [--json]")
				return 2
			}
			status, err := daemon.Status(ctx, runtime)
			if err != nil {
				if len(args) == 2 {
					_ = json.NewEncoder(stdout).Encode(map[string]any{"running": false, "error": err.Error()})
				}
				fmt.Fprintln(stderr, "harvester status:", err)
				return 1
			}
			status.ConfigDigest = ""
			if len(args) == 2 {
				if err := json.NewEncoder(stdout).Encode(status); err != nil {
					fmt.Fprintln(stderr, "harvester status:", err)
					return 1
				}
			} else {
				fmt.Fprintf(stdout, "harvester pid=%d version=%s uptime=%.1fs endpoint=%s gateway=%t draining=%t\n", status.PID, status.Version, status.UptimeSeconds, status.Endpoint, status.Gateway, status.Draining)
			}
			if status.CleanupError != "" {
				fmt.Fprintln(stderr, "harvester status:", status.CleanupError)
				return 1
			}
			return 0
		case "stop":
			if len(args) != 1 {
				fmt.Fprintln(stderr, "usage: harvester stop")
				return 2
			}
			if err := daemon.Stop(ctx, runtime); err != nil {
				fmt.Fprintln(stderr, "harvester stop:", err)
				return 1
			}
			fmt.Fprintln(stdout, "harvester stopped")
			return 0
		case "setup":
			flags := cli.NewFlagSet("setup", "usage: harvester setup [--offline]", stderr)
			offline := flags.Bool("offline", false, "provision from local caches without downloads")
			rest, code, ok := cli.ParseFlagsAnywhere(flags, args[1:])
			if !ok {
				return code
			}
			if len(rest) != 0 {
				flags.Usage()
				return 2
			}
			return setup.Run(ctx, runtime, *offline, stdout, stderr)
		case "doctor":
			return doctor.Run(args[1:], stdout, stderr, runtime)
		case "config":
			if len(args) != 2 || args[1] != "show" {
				fmt.Fprintln(stderr, "usage: harvester config path|show")
				return 2
			}
			data, err := config.MarshalHarvester(runtime.Config.Harvester, true)
			if err != nil {
				fmt.Fprintln(stderr, "harvester config:", err)
				return 1
			}
			fmt.Fprintln(stdout, string(data))
			return 0
		}
	}
	return harvestcli.Harvest(args, stdout, stderr, runtime)
}

func serveGateway(ctx context.Context, runtime config.Runtime, stdout, stderr io.Writer) int {
	if err := daemon.Serve(ctx, runtime); err != nil {
		fmt.Fprintln(stderr, "harvester serve:", err)
		return 1
	}
	return 0
}
