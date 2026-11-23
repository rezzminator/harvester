package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rezzminator/harvester/internal/obs"
)

type proxyTestHarness struct {
	input  *io.PipeWriter
	output *bufio.Reader
	cancel context.CancelFunc
	// stopped closes once the proxy under test returned; err holds what it returned.
	stopped chan struct{}
	err     error
}

// errProxyTestStopped fails a write the stopped proxy will never read; the failure names the proxy's error.
var errProxyTestStopped = errors.New("stdio proxy stopped")

// proxyTestWriteLimit bounds a write the proxy has not read: a live proxy reads its next frame once
// the daemon it ensures has started, within startupTimeout.
const proxyTestWriteLimit = startupTimeout

// newProxyTestHarness runs one proxy over two pipes. When the proxy returns, its error is recorded
// and both pipes close, so a pending write or read fails at once and names that error.
func newProxyTestHarness(run func(ctx context.Context, input io.Reader, output io.Writer) error) (*proxyTestHarness, *io.PipeReader) {
	ctx, cancel := context.WithCancel(context.Background())
	input, inputWriter := io.Pipe()
	output, outputWriter := io.Pipe()
	harness := &proxyTestHarness{
		input: inputWriter, output: bufio.NewReader(output), cancel: cancel, stopped: make(chan struct{}),
	}
	go func() {
		harness.err = run(ctx, input, outputWriter)
		close(harness.stopped)
		_ = input.CloseWithError(errProxyTestStopped)
		_ = outputWriter.Close()
	}()
	return harness, output
}

// outcome names the proxy's state for a failure message, waiting briefly for a proxy that is stopping.
func (harness *proxyTestHarness) outcome() string {
	select {
	case <-harness.stopped:
		return fmt.Sprintf("stdio proxy returned: %v", harness.err)
	case <-time.After(2 * time.Second):
		return "stdio proxy still running"
	}
}

func startProxyTestHarness(t *testing.T, proxy *stdioProxy) *proxyTestHarness {
	t.Helper()
	harness, output := newProxyTestHarness(proxy.run)
	t.Cleanup(func() {
		_ = harness.input.Close()
		go func() { _, _ = io.Copy(io.Discard, output) }()
		select {
		case <-harness.stopped:
		case <-time.After(2 * time.Second):
			t.Error("stdio proxy did not stop after cancellation")
		}
		harness.cancel()
	})
	return harness
}

func (harness *proxyTestHarness) write(t *testing.T, frame string) {
	t.Helper()
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(harness.input, frame+"\n")
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("write proxy frame %s: %v; %s", frame, err, harness.outcome())
		}
	case <-time.After(proxyTestWriteLimit):
		t.Fatalf("proxy did not read frame %s within %s; %s", frame, proxyTestWriteLimit, harness.outcome())
	}
}

func (harness *proxyTestHarness) read(t *testing.T) map[string]any {
	t.Helper()
	result := make(chan []byte, 1)
	errors := make(chan error, 1)
	go func() {
		line, err := harness.output.ReadBytes('\n')
		if err != nil {
			errors <- err
			return
		}
		result <- line
	}()
	select {
	case line := <-result:
		var frame map[string]any
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatalf("decode proxy response %s: %v", line, err)
		}
		return frame
	case err := <-errors:
		t.Fatalf("read proxy response: %v; %s", err, harness.outcome())
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for proxy response; %s", harness.outcome())
	}
	return nil
}

type proxyReadSignal struct {
	io.Reader
	started chan struct{}
	once    sync.Once
}

func (reader *proxyReadSignal) Read(data []byte) (int, error) {
	reader.once.Do(func() { close(reader.started) })
	return reader.Reader.Read(data)
}

func TestProxyHarnessCleanupUnblocksPendingRead(t *testing.T) {
	finished := make(chan error, 1)
	t.Run("pending read", func(t *testing.T) {
		proxy := newStdioProxy(context.Background(), "", io.Discard)
		harness := startProxyTestHarness(t, proxy)
		started := make(chan struct{})
		harness.output = bufio.NewReader(&proxyReadSignal{Reader: harness.output, started: started})
		go func() {
			_, err := harness.output.ReadBytes('\n')
			finished <- err
		}()
		<-started
	})
	select {
	case err := <-finished:
		if err != io.EOF {
			t.Fatalf("pending read returned %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending read survived harness cleanup")
	}
}

func TestProxyHarnessClosesDaemonSessionBeforeCleanupReturns(t *testing.T) {
	t.Parallel()
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodDelete {
			t.Errorf("method=%s, want DELETE", request.Method)
		}
		time.Sleep(30 * time.Millisecond)
		writer.WriteHeader(http.StatusNoContent)
		close(closed)
	}))
	defer server.Close()

	t.Run("proxy", func(t *testing.T) {
		proxy := &stdioProxy{endpoint: server.URL, client: obs.WrapClient(&http.Client{}), sessionID: "session"}
		startProxyTestHarness(t, proxy)
	})
	select {
	case <-closed:
	default:
		t.Fatal("daemon session DELETE had not completed when harness cleanup returned")
	}
}
