package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/daemon"
	"github.com/rezzminator/harvester/internal/paths"
	"os"
	"path/filepath"
)

type failingStdioWriter struct {
	err error
}

func (w failingStdioWriter) Write([]byte) (int, error) { return 0, w.err }

func TestStdioReturnsOutputFailureWithInputStillOpen(t *testing.T) {
	runtime := stdioRuntime(t)
	input, feed := io.Pipe()
	defer feed.Close()
	defer input.Close()
	want := errors.New("fixture output failed")
	done := make(chan error, 1)
	go func() { done <- serveStdio(context.Background(), runtime, input, failingStdioWriter{want}, io.Discard) }()
	if _, err := io.WriteString(feed, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-06-18\",\"capabilities\":{},\"clientInfo\":{\"name\":\"test\",\"version\":\"1\"}}}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatalf("output error lost: %v", err)
		}
	case <-time.After(time.Second):
		_ = feed.Close()
		<-done
		t.Fatal("stdio waited for external input after SDK output failed")
	}
}

func TestStdioRecoversMalformedFrame(t *testing.T) {
	runtime := stdioRuntime(t)
	input, feed := io.Pipe()
	output, receive := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer input.Close()
	defer output.Close()
	defer feed.Close()
	defer receive.Close()
	done := make(chan error, 1)
	go func() { done <- serveStdio(ctx, runtime, input, receive, io.Discard) }()
	if _, err := io.WriteString(feed, "not json\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(output)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, `"code":-32700`) {
		t.Fatalf("malformed response %q err=%v", line, err)
	}
	if _, err := io.WriteString(feed, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-06-18\",\"capabilities\":{},\"clientInfo\":{\"name\":\"test\",\"version\":\"1\"}}}\n"); err != nil {
		t.Fatal(err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || !strings.Contains(line, `"name":"harvester-professor"`) {
		t.Fatalf("initialize response %q err=%v", line, err)
	}
	cancel()
	_ = feed.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stdio did not stop")
	}
}

func stdioRuntime(t *testing.T) config.Runtime {
	t.Helper()
	home, err := os.MkdirTemp("", "hp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv(paths.EnvHome, home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("HARVESTER_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("HARVESTER_TEST_MAIN", "1")
	runtime, err := config.LoadRuntime("")
	if err != nil {
		t.Fatal(err)
	}
	runtime.Version = version
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Serve(ctx, runtime) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	ready := time.NewTimer(2 * time.Second)
	defer ready.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := daemon.Status(t.Context(), runtime); err == nil {
			return runtime
		}
		select {
		case err := <-done:
			t.Fatalf("daemon stopped: %v", err)
		case <-ready.C:
			t.Fatal("daemon not ready")
		case <-ticker.C:
		}
	}
}

func TestStopDrainsServeCleanly(t *testing.T) {
	_ = stdioRuntime(t)
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"stop"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("stop code=%d error=%s", code, &stderr)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(t.Context(), []string{"status", "--json"}, strings.NewReader(""), &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), `"running":false`) {
		t.Fatalf("status after stop code=%d output=%s error=%s", code, &stdout, &stderr)
	}
}
