// Package installping is Harvester's anonymous install count: on `harvester
// mcp` start it sends the install counter at most one ping per UTC day — a
// random id, the version, the OS and arch — in the background, never touching
// stdout, never delaying or failing the command it rides.
//
// Entry points:
//   - Start: the `harvester mcp` hook; one Send in a goroutine.
//   - Send: one synchronous attempt, returning what it decided.
//   - Refusal: why nothing may be sent here (opt-out, CI, test binary).
//
// Files:
//   - state.go: the install id file and the last-sent UTC day file.
package installping

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/clock"
	"github.com/rezzminator/harvester/internal/obs"
	"github.com/rezzminator/harvester/internal/paths"
)

const (
	// Product is the contract's product name for this binary.
	Product = "harvester"
	// EnvOffline turns the install count off.
	EnvOffline = "HARVESTER_INSTALL_PING_OFFLINE"
	// Notice is the one stderr line printed on the run that creates the id file.
	Notice             = "harvester: anonymous install count on (random id, version, OS, arch); set HARVESTER_INSTALL_PING_OFFLINE=1 to turn it off"
	productionEndpoint = "https://install-ping.rezzminator.workers.dev/v1/ping"
	timeout            = 5 * time.Second
	// Event values the contract accepts.
	EventInstall = "install"
	EventDaily   = "daily"
)

// endpoint is the one seam a test points at its own server; production never
// changes it, and Refusal refuses productionEndpoint inside any test binary.
var endpoint = productionEndpoint

// Payload is the contract's request body, in its field order.
type Payload struct {
	ID      string   `json:"id"`
	Product string   `json:"product"`
	Version string   `json:"version"`
	OS      string   `json:"os"`
	Arch    string   `json:"arch"`
	Plugins []string `json:"plugins"`
	Event   string   `json:"event"`
}

// Settings is what one Send needs; a zero field takes the process default
// (Getenv os.Getenv, Clock clock.Real, Home paths.Home, Stderr io.Discard).
type Settings struct {
	Home    string
	Version string
	Getenv  func(string) string
	Clock   clock.Clock
	Stderr  io.Writer
}

// Start runs one Send in a goroutine and returns at once: the ping never
// delays, blocks or fails the caller, and a panic in it is logged, never
// raised. Cancelling ctx does not cut the ping short; its own timeout does.
func Start(ctx context.Context, settings Settings) {
	ctx = context.WithoutCancel(ctx)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				obs.Logger(ctx).Warn("installping.failed", "step", "panic", obs.FieldErr, obs.Recovered("installping", recovered).Error())
			}
		}()
		_, _ = Send(ctx, settings)
	}()
}

// Send makes one attempt and returns what it decided: "refused: {why}",
// "skipped: sent {day}" or "sent {event}". A failure is logged as a WARN
// with its step, endpoint, event and path, and returned; it is never retried.
func Send(ctx context.Context, settings Settings) (string, error) {
	if settings.Getenv == nil {
		settings.Getenv = os.Getenv
	}
	if why := Refusal(settings.Getenv); why != "" {
		return "refused: " + why, nil
	}
	if settings.Clock == nil {
		settings.Clock = clock.Real
	}
	if settings.Stderr == nil {
		settings.Stderr = io.Discard
	}
	if settings.Home == "" {
		home, err := paths.Home()
		if err != nil {
			return "", warn(ctx, "home", "", "", fmt.Errorf("resolve harvester home: %w", err))
		}
		settings.Home = home
	}
	id, created, err := loadID(ctx, settings.Home)
	if err != nil {
		return "", warn(ctx, "id", "", paths.InstallID(settings.Home), err)
	}
	if created {
		fmt.Fprintln(settings.Stderr, Notice)
	}
	today := settings.Clock.Now().UTC().Format(time.DateOnly)
	last, err := lastDay(settings.Home)
	if err != nil {
		return "", warn(ctx, "day", "", paths.InstallPingDay(settings.Home), err)
	}
	if last == today {
		return "skipped: sent " + today, nil
	}
	event := EventDaily
	if created || last == "" {
		event = EventInstall
	}
	payload := Payload{ID: id, Product: Product, Version: settings.Version, OS: runtime.GOOS, Arch: runtime.GOARCH, Plugins: []string{}, Event: event}
	if err := post(ctx, payload); err != nil {
		return "", warn(ctx, "post", event, "", err)
	}
	if err := recordDay(settings.Home, today); err != nil {
		return "", warn(ctx, "record", event, paths.InstallPingDay(settings.Home), err)
	}
	return "sent " + event, nil
}

// Refusal names why nothing may be sent, or "" when a ping may go: an opt-out
// (EnvOffline or DO_NOT_TRACK set to anything but 0/false; the test jail sets
// EnvOffline for every process it starts), CI (CI likewise, or
// GITHUB_ACTIONS=true), or a test binary still aimed at the real endpoint — so
// no test anywhere can reach it.
func Refusal(getenv func(string) string) string {
	switch {
	case enabled(getenv(EnvOffline)):
		return EnvOffline
	case enabled(getenv("DO_NOT_TRACK")):
		return "DO_NOT_TRACK"
	case enabled(getenv("CI")):
		return "CI"
	case getenv("GITHUB_ACTIONS") == "true":
		return "GITHUB_ACTIONS"
	case testing.Testing() && endpoint == productionEndpoint:
		return "test binary"
	}
	return ""
}

// enabled is the contract's switch rule: any non-empty value but 0 or false.
func enabled(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && value != "0" && !strings.EqualFold(value, "false")
}

func post(ctx context.Context, payload Payload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode the ping: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build the ping request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	client := obs.WrapClient(&http.Client{Timeout: timeout})
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("send the ping: %w", err)
	}
	defer response.Body.Close()
	reason, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("the install counter answered %d: %s", response.StatusCode, strings.TrimSpace(string(reason)))
	}
	return nil
}

func warn(ctx context.Context, step, event, path string, err error) error {
	obs.Logger(ctx).Warn("installping.failed", "step", step, "target", endpoint, "kind", event, "path", path, obs.FieldErr, err.Error())
	return err
}
