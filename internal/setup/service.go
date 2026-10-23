package setup

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rezzminator/harvester/internal/atomicfile"
	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/daemon"
	"github.com/rezzminator/harvester/internal/deps"
)

const serviceName = "harvester.service"
const launchLabel = "com.harvester.gateway"

func serviceCommand(ctx context.Context, runner deps.Runner, args ...string) error {
	result, err := runner.Run(ctx, args, deps.RunOptions{})
	if err != nil || result.ExitCode != 0 {
		return fmt.Errorf("%s failed (exit=%d stderr=%s): %w", strings.Join(args, " "), result.ExitCode, strings.TrimSpace(string(result.Stderr)), errors.Join(err, fmt.Errorf("service command failed")))
	}
	return nil
}

func unitQuote(value string) string {
	return strconv.Quote(strings.ReplaceAll(value, "%", "%%"))
}

func serviceContent(cfg config.Runtime, goos string) (string, []byte, error) {
	home := cfg.Paths.Home
	binary, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("resolve gateway service executable: %w", err)
	}
	// Native plugins keep a stable link beside their versioned executable.
	stable := filepath.Join(filepath.Dir(filepath.Dir(binary)), "harvester")
	if linked, err := os.Stat(stable); err == nil {
		invoked, err := os.Stat(binary)
		if err != nil {
			return "", nil, fmt.Errorf("inspect invoked gateway executable %s: %w", binary, err)
		}
		if os.SameFile(linked, invoked) {
			binary = stable
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, fmt.Errorf("inspect gateway executable link %s: %w", stable, err)
	}
	path := cfg.Config.Path
	if path == "" {
		path = config.Path(home)
	}
	for _, value := range []string{home, path, binary} {
		if strings.ContainsAny(value, "\x00\r\n") {
			return "", nil, fmt.Errorf("service paths must not contain NUL or newlines")
		}
	}
	servicePath := filepath.Join(home, ".local", "bin") + ":/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin"
	environment := [][2]string{{"HARVESTER_CONFIG", path}, {"PATH", servicePath}}
	for _, name := range []string{"XDG_STATE_HOME", "XDG_CACHE_HOME", "HARVESTER_PYTHON_ROOT"} {
		if value := os.Getenv(name); value != "" {
			if strings.ContainsAny(value, "\x00\r\n") {
				return "", nil, fmt.Errorf("service environment %s contains NUL or newlines", name)
			}
			environment = append(environment, [2]string{name, value})
		}
	}
	switch goos {
	case "linux":
		body := "[Unit]\nDescription=Harvester authenticated gateway\nAfter=network-online.target\n\n[Service]\nType=simple\nExecStart=" + unitQuote(binary) + " serve\n"
		for _, entry := range environment {
			body += "Environment=" + unitQuote(entry[0]+"="+entry[1]) + "\n"
		}
		body += "Restart=on-failure\nRestartSec=3\n\n[Install]\nWantedBy=default.target\n"
		return filepath.Join(home, ".config", "systemd", "user", serviceName), []byte(body), nil
	case "darwin":
		xmlValue := func(value string) string {
			var b bytes.Buffer
			_ = xml.EscapeText(&b, []byte(value))
			return b.String()
		}
		envXML := ""
		for _, entry := range environment {
			envXML += "<key>" + xmlValue(entry[0]) + "</key><string>" + xmlValue(entry[1]) + "</string>"
		}
		body := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>` + launchLabel + `</string><key>ProgramArguments</key><array><string>` + xmlValue(binary) + `</string><string>serve</string></array><key>EnvironmentVariables</key><dict>` + envXML + `</dict><key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict></dict></plist>
`
		return filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist"), []byte(body), nil
	default:
		return "", nil, fmt.Errorf("gateway user service is unsupported on %s", goos)
	}
}

func configureService(ctx context.Context, cfg config.Runtime, runner deps.Runner, goos string) error {
	path, wanted, err := serviceContent(cfg, goos)
	if err != nil {
		return err
	}
	old, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("read user service %s: %w", path, readErr)
	}
	if !cfg.Config.Harvester.External.Enabled {
		if errors.Is(readErr, os.ErrNotExist) {
			return nil
		}
		if goos == "linux" {
			if err := serviceCommand(ctx, runner, "systemctl", "--user", "disable", "--now", serviceName); err != nil {
				return err
			}
		} else {
			target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + launchLabel
			loaded, err := launchLoaded(ctx, runner, target)
			if err != nil {
				return err
			}
			if loaded {
				if err := serviceCommand(ctx, runner, "launchctl", "bootout", target); err != nil {
					return err
				}
			}
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove disabled user service %s: %w", path, err)
		}
		if goos == "linux" {
			return serviceCommand(ctx, runner, "systemctl", "--user", "daemon-reload")
		}
		return nil
	}
	changed := !bytes.Equal(old, wanted)
	if changed {
		if err := atomicfile.Write(path, wanted, 0o644); err != nil {
			return fmt.Errorf("write user service: %w", err)
		}
	}
	if goos == "linux" {
		if err := serviceCommand(ctx, runner, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		if readErr == nil {
			if err := serviceCommand(ctx, runner, "systemctl", "--user", "stop", serviceName); err != nil {
				return err
			}
		}
		if err := daemon.Stop(ctx, cfg); err != nil {
			return fmt.Errorf("handoff on-demand daemon to user service: %w", err)
		}
		if err := serviceCommand(ctx, runner, "systemctl", "--user", "enable", "--now", serviceName); err != nil {
			return err
		}
		if err := serviceCommand(ctx, runner, "systemctl", "--user", "restart", serviceName); err != nil {
			return err
		}
		return serviceCommand(ctx, runner, "systemctl", "--user", "is-active", "--quiet", serviceName)
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	loaded, err := launchLoaded(ctx, runner, domain+"/"+launchLabel)
	if err != nil {
		return err
	}
	if loaded {
		if err := serviceCommand(ctx, runner, "launchctl", "bootout", domain+"/"+launchLabel); err != nil {
			return err
		}
	}
	if err := daemon.Stop(ctx, cfg); err != nil {
		return fmt.Errorf("handoff on-demand daemon to LaunchAgent: %w", err)
	}
	var bootstrapErr error
	for attempt := 0; attempt < 5; attempt++ {
		bootstrapErr = serviceCommand(ctx, runner, "launchctl", "bootstrap", domain, path)
		if bootstrapErr == nil {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if bootstrapErr != nil {
		return fmt.Errorf("gateway launch agent is down; bootstrap %s %s failed: %w", domain, path, bootstrapErr)
	}
	return serviceCommand(ctx, runner, "launchctl", "print", domain+"/"+launchLabel)
}

func launchLoaded(ctx context.Context, runner deps.Runner, target string) (bool, error) {
	result, err := runner.Run(ctx, []string{"launchctl", "print", target}, deps.RunOptions{})
	if err == nil && result.ExitCode == 0 {
		return true, nil
	}
	if result.ExitCode > 0 && ctx.Err() == nil {
		label := target[strings.LastIndex(target, "/")+1:]
		missing := `could not find service "` + strings.ToLower(label) + `" in domain`
		for _, line := range strings.Split(string(result.Stdout)+"\n"+string(result.Stderr), "\n") {
			if label != "" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), missing) {
				return false, nil
			}
		}
	}
	return false, fmt.Errorf("inspect launch agent %s exit=%d stdout=%s stderr=%s: %w", target, result.ExitCode, result.Stdout, result.Stderr, errors.Join(err, ctx.Err(), fmt.Errorf("launchctl probe failed")))
}
