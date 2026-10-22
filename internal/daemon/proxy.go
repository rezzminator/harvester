package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rezzminator/harvester/internal/clock"
	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/obs"
	"github.com/rezzminator/harvester/internal/paths"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	proxyInitializeMethod = "initialize"
	proxyRetryWindow      = 45 * time.Second
	proxyRetryDelay       = 100 * time.Millisecond
	proxyCloseTimeout     = 300 * time.Millisecond
	proxyMaxResponse      = (4<<20)*18 + (1 << 20)
)

type stdioProxy struct {
	address, endpoint         string
	warnings                  io.Writer
	client                    *http.Client
	clock                     clock.Clock
	retryWindow, retryDelay   time.Duration
	initialize, initialized   []byte
	sessionID, protocol       string
	sessionGeneration         uint64
	handshakeMutex            sync.Mutex
	sessionMutex              sync.RWMutex
	reinitMutex, requestMutex sync.Mutex
	requests, requestTails    map[string]*proxyRequestState
	ensure                    func(context.Context) error
}

func Proxy(ctx context.Context, runtime config.Runtime, input io.Reader, output, warnings io.Writer) error {
	proxy := newStdioProxy(ctx, paths.DaemonSocket(runtime.Paths.Home), warnings)
	proxy.endpoint = "http://localhost/mcp"
	proxy.client = obs.WrapClient(unixClient(paths.DaemonSocket(runtime.Paths.Home)))
	defer proxy.client.CloseIdleConnections()
	proxy.ensure = func(ctx context.Context) error {
		_, err := Ensure(ctx, runtime)
		if err != nil {
			return fmt.Errorf("harvester daemon unavailable; see %s: %w", paths.DaemonLog(runtime.Paths.Home), err)
		}
		return nil
	}
	if err := proxy.ensure(ctx); err != nil {
		proxy.warn("startup failed: %v", err)
		return err
	}
	return proxy.run(ctx, input, output)
}

type proxyFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type proxySession struct {
	sessionID, protocol string
	generation          uint64
}

type proxyPostResult struct {
	response   []byte
	generation uint64
}

type uncertainProxyDeliveryError struct {
	cause error
}

func (failure uncertainProxyDeliveryError) Error() string { return failure.cause.Error() }

func (failure uncertainProxyDeliveryError) Unwrap() error { return failure.cause }

func newStdioProxy(_ context.Context, address string, warnings io.Writer) *stdioProxy {
	if warnings == nil {
		warnings = os.Stderr
	}
	return &stdioProxy{address: address, endpoint: "http://" + address + "/mcp", warnings: warnings, client: &http.Client{Transport: http.DefaultTransport}, clock: clock.Real, retryWindow: proxyRetryWindow, retryDelay: proxyRetryDelay, requests: make(map[string]*proxyRequestState), requestTails: make(map[string]*proxyRequestState)}
}
func (proxy *stdioProxy) run(ctx context.Context, input io.Reader, output io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	failed := make(chan error, 1)
	serialized := &lockedWriter{writer: output, failed: failed}
	returned := make(chan error, 1)
	go func() { returned <- proxy.read(ctx, input, serialized) }()
	defer func() {
		if closer, ok := input.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				proxy.warn("close proxy input: %v", err)
			}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-failed:
		return err
	case err := <-returned:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
}

type lockedWriter struct {
	sync.Mutex
	writer io.Writer
	failed chan error
}

func (w *lockedWriter) Write(body []byte) (int, error) {
	w.Lock()
	defer w.Unlock()
	n, err := w.writer.Write(body)
	if err == nil && n != len(body) {
		err = io.ErrShortWrite
	}
	if err != nil {
		select {
		case w.failed <- err:
		default:
		}
	}
	return n, err
}

func (proxy *stdioProxy) read(ctx context.Context, input io.Reader, output io.Writer) error {
	requestCtx, cancelRequests := context.WithCancel(ctx)
	var pending sync.WaitGroup
	var pendingAdmission *proxyRequestState
	defer func() {
		cancelRequests()
		pending.Wait()
		proxy.closeSession()
	}()
	buffered := bufio.NewReader(input)
	for {
		frame, err := buffered.ReadBytes('\n')
		if len(frame) > 0 {
			frame = bytes.TrimSpace(frame)
			if len(frame) > 0 {
				var envelope proxyFrame
				decodeErr := json.Unmarshal(frame, &envelope)
				var cancellationState *proxyRequestState
				if decodeErr == nil {
					proxy.resolvePendingAdmission(pendingAdmission, envelope)
					pendingAdmission = nil
				}
				switch {
				case decodeErr != nil:
					proxy.warn("decode client frame: %v", decodeErr)
					proxy.answerError(output, json.RawMessage("null"), -32700, "Parse error")
				case isProxyHandshake(envelope.Method):
					// Handshake stays ordered; later traffic may overlap.
					proxy.forward(requestCtx, frame, output)
				case envelope.Method == proxyDiscoverMethod && len(envelope.ID) != 0:
					proxy.answerError(output, envelope.ID, proxyMethodNotFound, `method "server/discover" not found`)
				case len(envelope.ID) != 0:
					state, registered := proxy.registerRequest(requestCtx, envelope.ID)
					if !registered {
						proxy.answerFailure(output, envelope.ID, fmt.Errorf("invalid JSON-RPC request id"))
						continue
					}
					if state.admissionTimer != nil {
						pendingAdmission = state
					}
					pending.Add(1)
					go func(content []byte, request *proxyRequestState) {
						defer pending.Done()
						proxy.forwardRequest(content, output, request)
					}(append([]byte(nil), frame...), state)
				case envelope.Method == "notifications/cancelled":
					var completed bool
					completed, cancellationState = proxy.cancelRequest(envelope, output)
					if completed {
						break
					}
					fallthrough
				default:
					pending.Add(1)
					go func(content []byte, cancellation *proxyRequestState) {
						defer pending.Done()
						proxy.forward(requestCtx, content, output)
						if cancellation != nil {
							proxy.completeCancellation(cancellation)
						}
					}(append([]byte(nil), frame...), cancellationState)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read stdio proxy frame: %w", err)
		}
	}
}

func (proxy *stdioProxy) forward(ctx context.Context, raw []byte, output io.Writer) {
	var frame proxyFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		proxy.warn("decode validated client frame: %v", err)
		return
	}
	if isProxyHandshake(frame.Method) {
		proxy.storeHandshake(frame.Method, raw)
	}
	response, err := proxy.sendWithRetry(ctx, raw, isProxyHandshake(frame.Method))
	if err != nil {
		if len(frame.ID) == 0 {
			proxy.warn("dropped notification %q: %v", frame.Method, err)
			return
		}
		proxy.answerForwardFailure(output, frame.ID, err)
		return
	}
	if len(response) != 0 {
		if _, err := output.Write(append(response, '\n')); err != nil {
			proxy.warn("write daemon response to stdio: %v", err)
		}
	}
}
func (proxy *stdioProxy) sendWithRetry(ctx context.Context, frame []byte, isHandshake bool) ([]byte, error) {
	retryCtx, cancel := context.WithTimeout(ctx, proxy.retryWindow)
	defer cancel()
	var lastErr error
	replayedLostSession := false
	for attempt := 0; ; attempt++ {
		var result proxyPostResult
		var err error
		if proxy.ensure != nil {
			err = proxy.ensure(retryCtx)
			if err != nil {
				return nil, err
			}
		}
		if err == nil && !isHandshake {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			result.generation, err = proxy.restoreSession(retryCtx)
		}
		if err == nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			result, err = proxy.post(ctx, frame)
			if err == nil {
				if !isHandshake && !replayedLostSession && uninitializedSessionResponse(result.response) {
					// The daemon answered from a session that never saw initialize:
					// the request never ran, so re-initialize and replay it once.
					replayedLostSession = true
					proxy.clearSession(result.generation)
					continue
				}
				return result.response, nil
			}
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		var uncertain uncertainProxyDeliveryError
		if errors.As(err, &uncertain) {
			return nil, proxy.uncertainDelivery(err)
		}
		var rejected proxyRejectedError
		if errors.As(err, &rejected) {
			return nil, err
		}
		lastErr = err
		proxy.clearSession(result.generation)
		timer := proxy.clock.NewTimer(proxy.retryDelay)
		select {
		case <-retryCtx.Done():
			timer.Stop()
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			return nil, proxy.retryExhausted(lastErr)
		case <-timer.C():
		}
	}
}

func (proxy *stdioProxy) uncertainDelivery(cause error) error {
	return fmt.Errorf(
		"lost the response from harvester daemon %s; the request may have arrived, so it was not replayed; check the daemon, then retry deliberately: %w",
		proxy.address,
		cause,
	)
}

func (proxy *stdioProxy) storeHandshake(method string, frame []byte) {
	proxy.handshakeMutex.Lock()
	defer proxy.handshakeMutex.Unlock()
	if method == proxyInitializeMethod {
		proxy.initialize = append(proxy.initialize[:0], frame...)
		return
	}
	proxy.initialized = append(proxy.initialized[:0], frame...)
}

func (proxy *stdioProxy) handshakeSnapshot() ([]byte, []byte) {
	proxy.handshakeMutex.Lock()
	defer proxy.handshakeMutex.Unlock()
	return append([]byte(nil), proxy.initialize...), append([]byte(nil), proxy.initialized...)
}

func (proxy *stdioProxy) reinitializeLocked(ctx context.Context) (uint64, error) {
	initialize, initialized := proxy.handshakeSnapshot()
	if len(initialize) == 0 {
		return proxy.session().generation, nil
	}
	result, err := proxy.post(ctx, initialize)
	if err != nil {
		return result.generation, fmt.Errorf("re-initialize daemon session: %w", err)
	}
	if len(initialized) != 0 {
		result, err = proxy.post(ctx, initialized)
		if err != nil {
			return result.generation, fmt.Errorf("restore initialized notification: %w", err)
		}
	}
	return result.generation, nil
}

func (proxy *stdioProxy) post(ctx context.Context, frame []byte) (proxyPostResult, error) {
	session := proxy.session()
	result := proxyPostResult{generation: session.generation}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, proxy.endpoint, bytes.NewReader(frame))
	if err != nil {
		return result, fmt.Errorf("build daemon request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if session.sessionID != "" {
		request.Header.Set("Mcp-Session-Id", session.sessionID)
	}
	if session.protocol != "" {
		request.Header.Set("Mcp-Protocol-Version", session.protocol)
	}
	response, err := proxy.client.Do(request)
	if err != nil {
		failure := fmt.Errorf("POST %s: %w", proxy.endpoint, err)
		if ctx.Err() != nil || retryableConnectionFailure(err) {
			return result, failure
		}
		return result, uncertainProxyDeliveryError{cause: failure}
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			proxy.warn("close daemon response: %v", closeErr)
		}
	}()
	body, err := io.ReadAll(io.LimitReader(response.Body, proxyMaxResponse+1))
	if err != nil {
		return result, uncertainProxyDeliveryError{cause: fmt.Errorf("read daemon response: %w", err)}
	}
	if len(body) > proxyMaxResponse {
		return result, uncertainProxyDeliveryError{
			cause: fmt.Errorf("daemon response exceeds %d bytes", proxyMaxResponse),
		}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return result, proxy.daemonStatusError(response.StatusCode, body)
	}
	responseSessionID := response.Header.Get("Mcp-Session-Id")
	if len(bytes.TrimSpace(body)) == 0 {
		proxy.updateSession(result.generation, responseSessionID, "")
		var outgoing proxyFrame
		if err := json.Unmarshal(frame, &outgoing); err != nil {
			return result, uncertainProxyDeliveryError{cause: fmt.Errorf("decode delivered request: %w", err)}
		}
		if len(outgoing.ID) != 0 {
			return result, uncertainProxyDeliveryError{cause: fmt.Errorf("daemon answered an empty response")}
		}
		return result, nil
	}
	contentType := strings.TrimSpace(strings.SplitN(response.Header.Get("Content-Type"), ";", 2)[0])
	if contentType != "application/json" {
		proxy.updateSession(result.generation, responseSessionID, "")
		return result, uncertainProxyDeliveryError{
			cause: fmt.Errorf("daemon answered unsupported content type %q", contentType),
		}
	}
	body = bytes.TrimSpace(body)
	if !json.Valid(body) {
		proxy.updateSession(result.generation, responseSessionID, "")
		return result, uncertainProxyDeliveryError{cause: fmt.Errorf("daemon answered malformed JSON")}
	}
	proxy.updateSession(result.generation, responseSessionID, protocolFromResponse(body))
	result.response = body
	return result, nil
}

// retryableConnectionFailure retries every dial error: no request byte was written.
// One call can overrun the retry window by one dial; a replay status probe
// classifies any timeout, including a dial timeout, as unresponsive.
func retryableConnectionFailure(err error) bool {
	var network *net.OpError
	return errors.As(err, &network) && network.Op == "dial"
}

func (proxy *stdioProxy) closeSession() {
	session := proxy.session()
	if session.sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), proxyCloseTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(obs.Presence(ctx), http.MethodDelete, proxy.endpoint, http.NoBody)
	if err != nil {
		proxy.warn("build daemon session close: %v", err)
		return
	}
	request.Header.Set("Mcp-Session-Id", session.sessionID)
	if session.protocol != "" {
		request.Header.Set("Mcp-Protocol-Version", session.protocol)
	}
	response, err := proxy.client.Do(request)
	if err != nil {
		proxy.warn("close daemon session %q: %v", session.sessionID, err)
		return
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			proxy.warn("close daemon session response: %v", closeErr)
		}
	}()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotFound {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, proxyMaxResponse+1))
		if readErr != nil {
			proxy.warn("read daemon session close response: %v", readErr)
			return
		}
		proxy.warn(
			"close daemon session %q: HTTP %d: %s",
			session.sessionID,
			response.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}
	proxy.clearSession(session.generation)
}

func protocolFromResponse(response []byte) string {
	var envelope struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return ""
	}
	return envelope.Result.ProtocolVersion
}

func (proxy *stdioProxy) session() proxySession {
	proxy.sessionMutex.RLock()
	defer proxy.sessionMutex.RUnlock()
	return proxySession{sessionID: proxy.sessionID, protocol: proxy.protocol, generation: proxy.sessionGeneration}
}

func (proxy *stdioProxy) updateSession(generation uint64, sessionID, protocol string) {
	proxy.sessionMutex.Lock()
	defer proxy.sessionMutex.Unlock()
	if generation != proxy.sessionGeneration {
		return
	}
	nextSessionID := proxy.sessionID
	if sessionID != "" {
		nextSessionID = sessionID
	}
	nextProtocol := proxy.protocol
	if protocol != "" {
		nextProtocol = protocol
	}
	if nextSessionID == proxy.sessionID && nextProtocol == proxy.protocol {
		return
	}
	proxy.sessionID = nextSessionID
	proxy.protocol = nextProtocol
	proxy.sessionGeneration++
}

// clearSession forgets the session only while generation is still current, so
// a late failure cannot wipe a session another request already recovered.
func (proxy *stdioProxy) clearSession(generation uint64) {
	proxy.sessionMutex.Lock()
	defer proxy.sessionMutex.Unlock()
	if generation != proxy.sessionGeneration {
		return
	}
	proxy.sessionID = ""
	proxy.protocol = ""
	proxy.sessionGeneration++
}

func (proxy *stdioProxy) answerFailure(output io.Writer, id json.RawMessage, cause error) {
	proxy.answerError(output, id, -32000, cause.Error())
}

func (proxy *stdioProxy) answerError(output io.Writer, id json.RawMessage, code int, message string) {
	response := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id}
	response.Error.Code = code
	response.Error.Message = message
	encoded, err := json.Marshal(response)
	if err != nil {
		proxy.warn("encode daemon failure response: %v", err)
		return
	}
	if _, err := output.Write(append(encoded, '\n')); err != nil {
		proxy.warn("write daemon failure response: %v", err)
	}
}

func (proxy *stdioProxy) warn(format string, arguments ...any) {
	fmt.Fprintf(proxy.warnings, "harvester mcp stdio proxy: "+format+"\n", arguments...)
}
