package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStdioProxyReinitializesAfterDaemonReplacement(t *testing.T) {
	t.Parallel()
	var oldCalls, newCalls [][]string
	oldService := proxyTestService("old", nil, &oldCalls)
	newService := proxyTestService("new", nil, &newCalls)
	var mutex sync.RWMutex
	current := proxyTestDaemon(oldService)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.RLock()
		handler := current
		mutex.RUnlock()
		handler.ServeHTTP(writer, request)
	}))
	defer server.Close()

	proxy := newStdioProxy(context.Background(), proxyTestAddress(server), io.Discard)

	harness := startProxyTestHarness(t, proxy)
	harness.write(t, proxyTestInitialize)
	_ = harness.read(t)
	harness.write(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	harness.write(t, proxyTestToolCall(2, "new", `{"name":"before"}`))
	if got := proxyTestStructured(t, harness.read(t))["message"]; got != "old" {
		t.Fatalf("call before replacement = %v, want old", got)
	}
	mutex.Lock()
	current = proxyTestDaemon(newService)
	mutex.Unlock()
	harness.write(t, proxyTestToolCall(3, "new", `{"name":"after"}`))
	if got := proxyTestStructured(t, harness.read(t))["message"]; got != "new" {
		t.Fatalf("call after replacement = %v, want new", got)
	}
}

func TestStdioProxyConcurrentRecoveryReinitializesOnce(t *testing.T) {
	t.Parallel()
	const oldSession = "session-old"
	const recoveredSession = "session-recovered"
	firstCallsReady := make(chan struct{})
	releaseFirstCalls := make(chan struct{})
	var firstCalls atomic.Int32
	var initializeCalls atomic.Int32
	transport := proxyTestTransport(func(request *http.Request) (*http.Response, error) {
		var frame proxyFrame
		if err := json.NewDecoder(request.Body).Decode(&frame); err != nil {
			return nil, err
		}
		switch frame.Method {
		case proxyInitializeMethod:
			initializeCalls.Add(1)
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`,
				)),
			}
			response.Header.Set("Mcp-Session-Id", recoveredSession)
			return response, nil
		case "notifications/initialized":
			return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: http.NoBody}, nil
		case "tools/call":
			if request.Header.Get("Mcp-Session-Id") == oldSession {
				if firstCalls.Add(1) == 2 {
					close(firstCallsReady)
				}
				<-releaseFirstCalls
				return proxyTestSessionLost(), nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":2,"result":{}}`)),
			}, nil
		default:
			return nil, fmt.Errorf("unexpected proxy test method %q", frame.Method)
		}
	})
	proxy := newStdioProxy(context.Background(), "unused", io.Discard)
	proxy.endpoint = "http://proxy.test" + "/mcp"
	proxy.client = &http.Client{Transport: transport}
	proxy.retryDelay = time.Millisecond
	proxy.sessionID = oldSession
	proxy.protocol = "2025-06-18"
	proxy.storeHandshake(proxyInitializeMethod, []byte(proxyTestInitialize))
	proxy.storeHandshake("notifications/initialized", []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))

	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := proxy.sendWithRetry(
				context.Background(),
				[]byte(proxyTestToolCall(2, "status", `{"target":"seat"}`)),
				false,
			)
			results <- err
		}()
	}
	<-firstCallsReady
	close(releaseFirstCalls)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent recovery: %v", err)
		}
	}
	if calls := initializeCalls.Load(); calls != 1 {
		t.Fatalf("concurrent recovery replayed initialize %d times, want once", calls)
	}
}

func TestStdioProxySynchronizesHandshakeStorageAndReplay(t *testing.T) {
	t.Parallel()
	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"padding":"` +
		strings.Repeat("a", 64<<10) + `"}}`)
	initialized := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{"padding":"` +
		strings.Repeat("b", 64<<10) + `"}}`)
	alternates := [][]byte{
		bytes.ReplaceAll(initialize, []byte("a"), []byte("c")),
		bytes.ReplaceAll(initialized, []byte("b"), []byte("d")),
	}
	started := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	release := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	readDone := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	var bodies [2][]byte
	var calls atomic.Int32
	transport := proxyTestTransport(func(request *http.Request) (*http.Response, error) {
		if err := request.Context().Err(); err != nil {
			return nil, err
		}
		index := int(calls.Add(1)) - 1
		var content []byte
		if index < len(started) {
			close(started[index])
			<-release[index]
			var body bytes.Buffer
			chunk := make([]byte, 8)
			for {
				count, err := request.Body.Read(chunk)
				body.Write(chunk[:count])
				runtime.Gosched()
				if err == io.EOF {
					break
				}
				if err != nil {
					return nil, err
				}
			}
			content = body.Bytes()
			bodies[index] = content
			close(readDone[index])
		} else {
			var err error
			if content, err = io.ReadAll(request.Body); err != nil {
				return nil, err
			}
		}
		response := &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: http.NoBody}
		if bytes.Contains(content, []byte(`"method":"initialize"`)) {
			response.Header.Set("Content-Type", "application/json")
			response.Body = io.NopCloser(strings.NewReader(
				`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`,
			))
		}
		return response, nil
	})
	proxy := newStdioProxy(context.Background(), "unused", io.Discard)
	proxy.endpoint = "http://proxy.test" + "/mcp"
	proxy.client = &http.Client{Transport: transport}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	proxy.forward(cancelled, initialize, io.Discard)
	proxy.forward(cancelled, initialized, io.Discard)

	restored := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := proxy.restoreSession(context.Background())
			restored <- err
		}()
	}
	for index := range started {
		<-started[index]
		writerReady := make(chan struct{})
		writerDone := make(chan struct{})
		go func(index int) {
			close(writerReady)
			<-release[index]
			for {
				select {
				case <-readDone[index]:
					close(writerDone)
					return
				default:
				}
				proxy.forward(cancelled, alternates[index], io.Discard)
			}
		}(index)
		<-writerReady
		close(release[index])
		<-writerDone
	}
	for range 2 {
		if err := <-restored; err != nil {
			t.Fatalf("restoreSession: %v", err)
		}
	}
	for index, want := range [][]byte{initialize, initialized} {
		if !bytes.Equal(bodies[index], want) || !json.Valid(bodies[index]) {
			t.Fatalf("replayed handshake frame %d was incomplete", index+1)
		}
	}
	proxy.handshakeMutex.Lock()
	proxy.initialized = nil
	proxy.handshakeMutex.Unlock()
	session := proxy.session()
	if session.sessionID == "" && session.protocol == "" {
		t.Fatal("restoreSession replay left no held session")
	}
	before := calls.Load()
	generation, err := proxy.restoreSession(context.Background())
	if err != nil || calls.Load() != before || generation != session.generation {
		t.Fatalf(
			"restoreSession with held session: calls=%d generation=%d want=%d err=%v",
			calls.Load()-before, generation, session.generation, err,
		)
	}
	proxy.clearSession(proxy.session().generation)
	before = calls.Load()
	if _, err := proxy.restoreSession(context.Background()); err != nil || calls.Load() != before+1 {
		t.Fatalf("restoreSession initialize-only replay: calls=%d err=%v", calls.Load()-before, err)
	}
}

func TestStdioProxyBoundsUnavailableDaemonPerRequest(t *testing.T) {
	// Serial: expects its closed port to refuse; a parallel test's fork would keep the listener accepting until exec.
	var calls [][]string
	service := proxyTestService("initial", nil, &calls)
	server := httptest.NewServer(proxyTestDaemon(service))
	address := proxyTestAddress(server)
	proxy := newStdioProxy(context.Background(), address, io.Discard)
	proxy.retryWindow = 120 * time.Millisecond
	proxy.retryDelay = 10 * time.Millisecond
	harness := startProxyTestHarness(t, proxy)
	harness.write(t, proxyTestInitialize)
	_ = harness.read(t)
	server.Close()

	started := time.Now()
	harness.write(t, proxyTestToolCall(7, "new", `{"name":"down"}`))
	response := harness.read(t)
	if response["id"] != float64(7) {
		t.Fatalf("bounded error id = %#v, want 7", response["id"])
	}
	errorObject, ok := response["error"].(map[string]any)
	if !ok {
		t.Fatalf("bounded response = %+v, want JSON-RPC error", response)
	}
	message, _ := errorObject["message"].(string)
	for _, part := range []string{"harvester daemon", address, "120ms", "harvester serve"} {
		if !strings.Contains(message, part) {
			t.Errorf("bounded error %q does not name %q", message, part)
		}
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded call took %v, want under one second", elapsed)
	}
}

func TestStdioProxyDoesNotReplayMutationAfterResponseEOF(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		connection, _, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack response: %v", err)
			return
		}
		_ = connection.Close()
	}))
	defer server.Close()
	proxy := newStdioProxy(context.Background(), proxyTestAddress(server), io.Discard)
	proxy.retryWindow = 100 * time.Millisecond
	proxy.retryDelay = time.Millisecond

	_, err := proxy.sendWithRetry(
		context.Background(),
		[]byte(proxyTestToolCall(2, "new", `{"name":"child"}`)),
		false,
	)
	if err == nil || !strings.Contains(err.Error(), "was not replayed") {
		t.Fatalf("EOF mutation error = %v, want the no-replay verdict", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("EOF mutation reached the daemon %d times, want once", calls.Load())
	}
}

func TestStdioProxyDoesNotReplayMutationAfterInvalidSuccessResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "malformed JSON", contentType: "application/json", body: `{"jsonrpc":`},
		{name: "unsupported content type", contentType: "text/plain", body: `accepted`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writer.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			proxy := newStdioProxy(context.Background(), proxyTestAddress(server), io.Discard)
			proxy.retryWindow = 50 * time.Millisecond
			proxy.retryDelay = time.Millisecond

			_, err := proxy.sendWithRetry(
				context.Background(),
				[]byte(proxyTestToolCall(2, "new", `{"name":"child"}`)),
				false,
			)
			if err == nil || !strings.Contains(err.Error(), "was not replayed") {
				t.Fatalf("invalid success response error = %v, want the no-replay verdict", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("mutation reached daemon %d times, want once", calls.Load())
			}
		})
	}
}

func TestStdioProxyAllowsHealthyCallsPastRetryWindow(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		<-time.After(100 * time.Millisecond)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":2,"result":{}}`)
	}))
	defer server.Close()
	proxy := newStdioProxy(context.Background(), proxyTestAddress(server), io.Discard)
	if proxy.client.Timeout != 0 {
		t.Fatalf("proxy client timeout = %s, want tool execution governed by its request context", proxy.client.Timeout)
	}
	proxy.retryWindow = 30 * time.Millisecond
	if _, err := proxy.sendWithRetry(
		context.Background(),
		[]byte(proxyTestToolCall(2, "status", `{"target":"seat","ask":true}`)),
		false,
	); err != nil {
		t.Fatalf("healthy request beyond the retry window failed: %v", err)
	}
}

func TestStdioProxyDropsUndeliverableNotification(t *testing.T) {
	t.Parallel()
	var calls [][]string
	service := proxyTestService("initial", nil, &calls)
	server := httptest.NewServer(proxyTestDaemon(service))
	address := proxyTestAddress(server)
	var warnings proxyTestBuffer
	proxy := newStdioProxy(context.Background(), address, &warnings)
	proxy.retryWindow = 80 * time.Millisecond
	proxy.retryDelay = 10 * time.Millisecond
	harness := startProxyTestHarness(t, proxy)
	harness.write(t, proxyTestInitialize)
	_ = harness.read(t)
	server.Close()

	harness.write(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":44}}`)
	deadline := time.After(time.Second)
	for !strings.Contains(warnings.String(), "dropped notification") {
		select {
		case <-deadline:
			t.Fatalf("notification warning = %q", warnings.String())
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestStdioProxyClosesDaemonSessionAtEOF(t *testing.T) {
	t.Parallel()
	var calls [][]string
	daemon := proxyTestDaemon(proxyTestService("daemon", nil, &calls))
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			deletes.Add(1)
		}
		daemon.ServeHTTP(writer, request)
	}))
	defer server.Close()
	proxy := newStdioProxy(context.Background(), proxyTestAddress(server), io.Discard)
	var output bytes.Buffer
	if err := proxy.run(
		context.Background(),
		strings.NewReader(proxyTestInitialize+"\n"),
		&output,
	); err != nil {
		t.Fatal(err)
	}
	if deletes.Load() != 1 {
		t.Fatalf("daemon session DELETE count = %d, want 1", deletes.Load())
	}
}

func TestStdioProxyCancelsOutstandingCallAtEOF(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var frame proxyFrame
		if err := json.NewDecoder(request.Body).Decode(&frame); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		if frame.Method != "tools/call" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			return
		}
		close(started)
		<-request.Context().Done()
		close(cancelled)
	}))
	defer server.Close()
	proxy := newStdioProxy(context.Background(), proxyTestAddress(server), io.Discard)
	input, inputWriter := io.Pipe()
	returned := make(chan error, 1)
	go func() { returned <- proxy.run(context.Background(), input, io.Discard) }()
	if _, err := io.WriteString(inputWriter, proxyTestToolCall(2, "whoami", `{}`)+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("outstanding proxy call did not start")
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("proxy EOF returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy EOF did not cancel its outstanding call")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon request context survived proxy EOF")
	}
}
