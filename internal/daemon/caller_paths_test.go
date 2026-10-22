package daemon

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rezzminator/harvester/internal/paths"
)

func TestProxyResolvesOnlyReadFilesAndPreservesTheEnvelope(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "caller")
	raw := []byte(`{"jsonrpc":"2.0","id":9007199254740993,"extra":{"keep":true},"method":"tools/call","params":{"name":"r\u0065ad","_meta":{"keep":"yes"},"arguments":{"files":["one.txt","../two.txt"," one.txt "," /absolute.txt ","/absolute.txt","file:///absolute.txt","https://example.test/doc","10.1038/nature14539","","   ",null,42],"refresh":true,"include_content":false,"headers":{"X-Test":"preserved"}}}}`)
	var frame proxyFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	resolved := resolveProxyReadFiles(raw, frame, directory)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(resolved, &got); err != nil {
		t.Fatal(err)
	}
	var original map[string]json.RawMessage
	json.Unmarshal(raw, &original)
	for _, key := range []string{"jsonrpc", "id", "extra", "method"} {
		if string(got[key]) != string(original[key]) {
			t.Errorf("envelope %s changed: %s", key, got[key])
		}
	}
	var params map[string]json.RawMessage
	json.Unmarshal(got["params"], &params)
	var args map[string]json.RawMessage
	json.Unmarshal(params["arguments"], &args)
	var files []any
	json.Unmarshal(args["files"], &files)
	want := []any{filepath.Join(directory, "one.txt"), filepath.Join(directory, "../two.txt"), filepath.Join(directory, "one.txt"), "/absolute.txt", "/absolute.txt", "file:///absolute.txt", "https://example.test/doc", "10.1038/nature14539", "", "", nil, float64(42)}
	for index := range want {
		if files[index] != want[index] {
			t.Errorf("files[%d] = %v, want %v", index, files[index], want[index])
		}
	}
	if string(params["_meta"]) != `{"keep":"yes"}` || string(args["refresh"]) != "true" || string(args["include_content"]) != "false" || string(args["headers"]) != `{"X-Test":"preserved"}` {
		t.Errorf("read options changed: %s", resolved)
	}
	for _, input := range []string{
		`{"method":"tools/call","params":{"name":"other","arguments":{"files":["one.txt"]}}}`,
		`{"method":"tools/call","params":{"name":"read","arguments":{"files":"one.txt"}}}`,
		`{"method":"tools/call","params":{"name":"read","arguments":null}}`,
		`{"method":"tools/call","params":{"name":"read","arguments":{"files":[null,42]}}}`,
		`{"method":"tools/list"}`,
	} {
		json.Unmarshal([]byte(input), &frame)
		if result := resolveProxyReadFiles([]byte(input), frame, directory); string(result) != input {
			t.Errorf("unrelated or malformed frame changed: %s", result)
		}
	}
}

func TestDaemonRejectsRelativeFilesWithoutReadingItsDirectory(t *testing.T) {
	cfg := daemonFixture(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "same.txt"), []byte("DAEMON-PRIVATE-PROJECT"), 0600); err != nil {
		t.Fatal(err)
	}
	foreground(t, cfg)
	proxy := newStdioProxy(t.Context(), paths.DaemonSocket(cfg.Paths.Home), io.Discard)
	proxy.endpoint = "http://localhost/mcp"
	proxy.client = unixClient(paths.DaemonSocket(cfg.Paths.Home))
	defer proxy.client.CloseIdleConnections()
	if _, err := proxy.sendWithRetry(context.Background(), []byte(proxyTestInitialize), true); err != nil {
		t.Fatal(err)
	}
	defer proxy.closeSession()
	for _, source := range []string{"same.txt", "./same.txt", "file:same.txt", "file://localhost"} {
		response, err := proxy.sendWithRetry(t.Context(), []byte(proxyTestToolCall(2, "read", `{"files":["`+source+`"]}`)), false)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(response), "DAEMON-PRIVATE-PROJECT") || !strings.Contains(string(response), "absolute") {
			t.Errorf("daemon did not reject relative file %q: %s", source, response)
		}
	}
	args, _ := json.Marshal(map[string]any{"files": []string{"same.txt", " " + filepath.Join(dir, "public.txt") + " "}})
	if err := os.WriteFile(filepath.Join(dir, "public.txt"), []byte("ABSOLUTE-PUBLIC-DOCUMENT"), 0600); err != nil {
		t.Fatal(err)
	}
	response, err := proxy.sendWithRetry(t.Context(), []byte(proxyTestToolCall(3, "read", string(args))), false)
	if err != nil || strings.Contains(string(response), "DAEMON-PRIVATE-PROJECT") || !strings.Contains(string(response), "ABSOLUTE-PUBLIC-DOCUMENT") || !strings.Contains(string(response), "absolute file paths") {
		t.Errorf("mixed batch boundary: %s: %v", response, err)
	}
}
