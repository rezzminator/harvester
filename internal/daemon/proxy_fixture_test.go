package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rezzminator/harvester/internal/harvestmcp"
)

const proxyTestInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"proxy-test","version":"test"}}}`

type proxyTestTransport func(*http.Request) (*http.Response, error)

func (t proxyTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return t(r) }

type proxyTestBuffer struct {
	mutex sync.Mutex
	bytes.Buffer
}

func (b *proxyTestBuffer) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.Buffer.Write(p)
}
func (b *proxyTestBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.Buffer.String()
}
func proxyTestAddress(server *httptest.Server) string {
	return strings.TrimPrefix(server.URL, "http://")
}
func newMCPHTTPHandler(server *mcp.Server, _ string) http.Handler {
	return harvestmcp.LinkClientDisconnect(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true}))
}
func proxyTestDaemon(server *mcp.Server) http.Handler { return newMCPHTTPHandler(server, "fixture") }
func proxyTestService(marker string, _ any, _ *[][]string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "test"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "new"}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, map[string]any, error) {
		return nil, map[string]any{"message": marker}, nil
	})
	return server
}
func proxyTestToolCall(id int, name, args string) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
}
func proxyTestStructured(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	result, ok := frame["result"].(map[string]any)
	if !ok {
		body, _ := json.Marshal(frame)
		t.Fatalf("no result: %s", body)
	}
	structured, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("no structured content: %+v", result)
	}
	return structured
}
