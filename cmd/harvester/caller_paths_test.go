package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/daemon"
	"github.com/rezzminator/harvester/internal/paths"
)

func TestMCPRelativeFilesUseEachCallerDirectory(t *testing.T) {
	home, err := os.MkdirTemp("", "hpaths-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv(paths.EnvHome, home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("HARVESTER_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("HARVESTER_TEST_MAIN", "1")
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"external":{"enabled":false},"search":{"enabled":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadRuntime("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Version = version
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := daemon.Stop(ctx, cfg); err != nil {
			t.Error(err)
		}
	}()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var firstPID int
	for _, caller := range []string{"A", "B"} {
		t.Run(caller, func(t *testing.T) {
			dir := filepath.Join(home, caller)
			if err := os.MkdirAll(filepath.Join(dir, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"same.txt", "second.txt"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("CALLER-"+caller+"-"+name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "mcp")
			command.Dir = dir
			var stderr bytes.Buffer
			command.Stderr = &stderr
			input, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				input.Close()
				if err := command.Wait(); err != nil {
					t.Errorf("proxy exit: %v: %s", err, &stderr)
				}
			}()
			reader := bufio.NewReader(output)
			call := func(frame string) string {
				t.Helper()
				if _, err := io.WriteString(input, frame+"\n"); err != nil {
					t.Fatal(err)
				}
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatalf("proxy response: %v", err)
				}
				return line
			}
			call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"caller-paths","version":"test"}}}`)
			io.WriteString(input, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
			args, _ := json.Marshal(map[string]any{"files": []string{"same.txt", "nested/../second.txt", "file://" + filepath.Join(dir, "same.txt"), filepath.Join(dir, "second.txt"), " same.txt ", " " + filepath.Join(dir, "second.txt") + " "}, "refresh": true})
			response := call(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read","arguments":%s}}`, args))
			for _, name := range []string{"same.txt", "second.txt"} {
				if strings.Count(response, "CALLER-"+caller+"-"+name) != 3 {
					t.Errorf("read did not use caller %s directory for %s: %s", caller, name, response)
				}
			}
			if caller == "B" && strings.Contains(response, "CALLER-A-") {
				t.Errorf("caller B read caller A's project: %s", response)
			}
			for index, malformed := range []string{`"same.txt"`, `[42]`, `[null]`, `["   "]`} {
				response := call(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"read","arguments":{"files":%s}}}`, index+3, malformed))
				if !strings.Contains(response, `"error"`) && !strings.Contains(response, `"isError":true`) {
					t.Errorf("malformed files accepted: %s", response)
				}
			}
			status, err := daemon.Status(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if caller == "A" {
				firstPID = status.PID
			} else if status.PID != firstPID {
				t.Errorf("callers used different daemons: %d != %d", status.PID, firstPID)
			}
		})
	}
}
