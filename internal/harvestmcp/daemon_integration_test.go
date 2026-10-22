package harvestmcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/deps"
	"github.com/rezzminator/harvester/internal/harvestpy"
)

type daemonConverterInput struct{ response *io.PipeWriter }

func (input daemonConverterInput) Write(body []byte) (int, error) {
	var request struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return 0, err
	}
	response, err := json.Marshal(map[string]any{"id": request.ID, "ok": true, "markdown": "shared service worker", "kind": "html"})
	if err != nil {
		return 0, err
	}
	go func() { _, _ = input.response.Write(append(response, '\n')) }()
	return len(body), nil
}
func (input daemonConverterInput) Close() error { return input.response.Close() }

func TestDaemonLocalAndRemoteServicesShareOneWorkerUntilLastClose(t *testing.T) {
	home := t.TempDir()
	script := filepath.Join(home, "converter.py")
	document := filepath.Join(home, "document.html")
	for path, body := range map[string]string{script: "fixture worker", document: "<p>fixture document</p>"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	runner := &deps.FakeRunner{}
	runner.ScriptInteractive([]string{"fixture-python", script}, deps.InteractiveScript{Pid: 4242, Stdin: daemonConverterInput{writer}, Stdout: reader})
	runtime := Runtime{Home: home, CacheDir: filepath.Join(home, "cache"), Python: "fixture-python", Script: script, Runner: runner, ConvertWorkers: 1, DisableSearch: true}
	local, err := NewConfiguredHarvester("test", runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	remote, err := NewRemote(RemoteOptions{Version: "test", Runtime: runtime, PublicURL: "http://127.0.0.1:18378", StaticToken: "fixture-token", StatePath: filepath.Join(home, "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if local.runtime.Remote || !remote.service.runtime.Remote {
		t.Fatal("local and remote service policies collapsed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	first, err := local.worker.Convert(ctx, harvestpy.Request{Path: document, Kind: "html"})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := remote.service.worker.Convert(ctx, harvestpy.Request{Path: document, Kind: "html"})
	if err != nil {
		t.Fatalf("remote lost worker when local holder closed: %v", err)
	}
	if first.Markdown != "shared service worker" || second.Markdown != first.Markdown || len(runner.Starts()) != 1 {
		t.Fatalf("services started independent workers: first=%+v second=%+v starts=%v", first, second, runner.Starts())
	}
	if err := remote.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.service.worker.Convert(ctx, harvestpy.Request{Path: document, Kind: "html"}); err == nil {
		t.Fatal("worker accepted conversion after last service closed")
	}
	kills := 0
	for _, call := range runner.LifecycleCalls() {
		if call.Action == "kill-group" {
			kills++
		}
	}
	if kills != 1 {
		t.Fatalf("shared worker group killed %d times, want once", kills)
	}
}
