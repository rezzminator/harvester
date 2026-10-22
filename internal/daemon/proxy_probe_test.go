package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestStdioProxyCancellationInterruptsAnUnresponsiveReplayProbe(t *testing.T) {
	proxy := newStdioProxy(t.Context(), "unused", io.Discard)
	var probes atomic.Int32
	probing := make(chan struct{})
	proxy.ensure = func(ctx context.Context) error {
		if probes.Add(1) == 1 {
			return nil
		}
		close(probing)
		<-ctx.Done()
		return ctx.Err()
	}
	proxy.client = &http.Client{Transport: proxyTestTransport(func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}
	})}
	proxy.retryDelay = time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := proxy.sendWithRetry(ctx, []byte(proxyTestToolCall(2, "read", `{}`)), false)
		done <- err
	}()
	<-probing
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("probe cancellation=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request stayed blocked on canceled health probe")
	}
}

func TestRunStdioCancelledDaemonProbe(t *testing.T) {
	proxy := newStdioProxy(t.Context(), "unused", io.Discard)
	probing := make(chan struct{})
	proxy.ensure = func(ctx context.Context) error { close(probing); <-ctx.Done(); return ctx.Err() }
	input, feed := io.Pipe()
	defer feed.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- proxy.run(ctx, input, io.Discard) }()
	if _, err := io.WriteString(feed, proxyTestInitialize+"\n"); err != nil {
		t.Fatal(err)
	}
	<-probing
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("proxy cancellation=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy stayed blocked on canceled daemon probe")
	}
}

func TestRunStdioCancellationAfterSuccessfulProbe(t *testing.T) {
	proxy := newStdioProxy(t.Context(), "unused", io.Discard)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	proxy.ensure = func(context.Context) error { cancel(); return nil }
	err := proxy.run(ctx, strings.NewReader(proxyTestInitialize+"\n"), io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("proxy after canceled successful probe=%v", err)
	}
}

func TestStdioProxyAllowsExistingMaximumCaptureResponse(t *testing.T) { testProxyLargeResponse(t, "x") }
func TestStdioProxyAllowsEscapedMaximumCaptureResponse(t *testing.T) {
	testProxyLargeResponse(t, `\u003c`)
}
func testProxyLargeResponse(t *testing.T, unit string) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":2,"result":{"text":"` + strings.Repeat(unit, 4<<20) + `"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	defer server.Close()
	proxy := newStdioProxy(t.Context(), proxyTestAddress(server), io.Discard)
	result, err := proxy.post(t.Context(), []byte(proxyTestToolCall(2, "read", `{}`)))
	if err != nil || string(result.response) != body {
		t.Fatalf("large response bytes=%d want=%d err=%v", len(result.response), len(body), err)
	}
}
