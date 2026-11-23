package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/deps"
	"github.com/rezzminator/harvester/internal/harvestmcp"
	"github.com/rezzminator/harvester/internal/obs"
	"github.com/rezzminator/harvester/internal/paths"
)

func runtimeForService(cfg config.Runtime) harvestmcp.Runtime {
	runtime := harvestmcp.RuntimeFromConfig(cfg.Paths.Home, cfg.Config.Harvester)
	runtime.Machine = &cfg.Config
	runtime.RequireAbsoluteFiles = true
	// Identical runner identity lets both security views hold the same converter pool.
	runtime.Runner = obs.Runner(deps.RealRunner{})
	return runtime
}

func NewGateway(cfg config.Runtime) (*harvestmcp.RemoteServer, *http.Server, error) {
	return newGateway(cfg, runtimeForService(cfg))
}

func newGateway(cfg config.Runtime, runtime harvestmcp.Runtime) (*harvestmcp.RemoteServer, *http.Server, error) {
	settings := cfg.Config.Harvester.External
	if !settings.Enabled {
		return nil, nil, errors.New("external.enabled is false")
	}
	statePath := ""
	if settings.StateDir != "" {
		statePath = filepath.Join(settings.StateDir, "auth.json")
	}
	remote, err := harvestmcp.NewRemote(harvestmcp.RemoteOptions{Runtime: runtime, Version: cfg.Version, PublicURL: settings.PublicURL, Passphrase: settings.Passphrase, StaticToken: settings.StaticToken, StatePath: statePath})
	if err != nil {
		return nil, nil, fmt.Errorf("configure external gateway: %w", err)
	}
	server := &http.Server{Addr: net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port)), Handler: remote.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute}
	return remote, server, nil
}

type admission struct {
	sync.Mutex
	draining bool
	active   sync.WaitGroup
}

func (g *admission) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.Lock()
		if g.draining {
			g.Unlock()
			http.Error(w, "harvester daemon is draining", http.StatusServiceUnavailable)
			return
		}
		g.active.Add(1)
		g.Unlock()
		defer g.active.Done()
		next.ServeHTTP(w, r)
	})
}
func (g *admission) drain() { g.Lock(); g.draining = true; g.Unlock() }

func (g *admission) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { g.active.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("daemon drain did not finish active work: %w", ctx.Err())
	}
}

func acquire(runtime config.Runtime) (*os.File, bool, error) {
	if err := privateRoot(runtime.Paths.Home); err != nil {
		return nil, false, err
	}
	path := paths.DaemonLock(runtime.Paths.Home)
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open daemon lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := checkPrivate(path, 0); err != nil {
		file.Close()
		return nil, false, err
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lock daemon: %w", err)
	}
	return file, true, nil
}

// checkSocketPath refuses a socket path the kernel cannot bind (sun_path holds 104 bytes on macOS,
// 108 on Linux, its NUL included) by name, where bind alone says only "invalid argument".
func checkSocketPath(socket string) error {
	if limit := len(syscall.RawSockaddrUnix{}.Path); len(socket) >= limit {
		return fmt.Errorf("daemon socket path %s is %d bytes, over the %d-byte unix socket path limit; set XDG_STATE_HOME to a shorter directory", socket, len(socket), limit-1)
	}
	return nil
}

func Serve(ctx context.Context, runtime config.Runtime) error {
	return serve(ctx, runtime, drainTimeout, handoffTimeout, nil)
}

// serve runs the daemon until ctx ends or a /stop asks it to. A stop in mode shutdown (harvester stop)
// drains admitted calls for drainWindow while keeping the socket and the lock. Any other stop is a
// replacement's handoff: the socket and the lock pass to the successor at once, and the admitted calls
// finish here within handoffWindow, so a replacement cuts no call that ends within its own limits.
func serve(ctx context.Context, runtime config.Runtime, drainWindow, handoffWindow time.Duration, admittedHandler http.Handler) (returnErr error) {
	if runtime.Version == "" {
		runtime.Version = "dev"
	}
	lock, held, err := acquire(runtime)
	if err != nil {
		return err
	}
	if !held {
		bounded, cancel := context.WithTimeout(ctx, startupTimeout)
		defer cancel()
		for {
			status, err := Status(bounded, runtime)
			if err == nil && status.CleanupError != "" {
				return fmt.Errorf("daemon unsafe cleanup; see %s: %s", paths.DaemonLog(runtime.Paths.Home), status.CleanupError)
			}
			if err == nil && !status.Draining {
				digest, digestErr := identity(runtime)
				if digestErr != nil {
					return digestErr
				}
				if status.ConfigPath != runtime.Config.Path || status.ConfigDigest != digest || status.Version != runtime.Version {
					return fmt.Errorf("existing daemon configuration/version differs; run harvester setup to refresh it")
				}
				return nil
			}
			if err != nil && !errors.Is(err, ErrDaemonAbsent) {
				return fmt.Errorf("daemon lock held but health failed: %w", err)
			}
			lock, held, err = acquire(runtime)
			if err != nil {
				return err
			}
			if held {
				break
			}
			timer := time.NewTimer(25 * time.Millisecond)
			select {
			case <-bounded.Done():
				timer.Stop()
				return fmt.Errorf("daemon lock held without healthy daemon; see %s: %w", paths.DaemonLog(runtime.Paths.Home), bounded.Err())
			case <-timer.C:
			}
		}
	}
	releaseLock := sync.OnceValue(lock.Close)
	defer func() { returnErr = errors.Join(returnErr, releaseLock()) }()
	socket := paths.DaemonSocket(runtime.Paths.Home)
	if err := checkPrivate(socket, os.ModeSocket); err == nil {
		if err := os.Remove(socket); err != nil {
			return fmt.Errorf("remove stale daemon socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := checkSocketPath(socket); err != nil {
		return err
	}
	// The socket appears at its path already private: bound under another name, made 0600, then renamed
	// in. A proxy probing while this daemon starts reads it absent, never as an unsafe socket.
	bind := paths.DaemonSocketBind(runtime.Paths.Home)
	if err := os.Remove(bind); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove leftover daemon socket bind: %w", err)
	}
	listener, err := net.Listen("unix", bind)
	if err != nil {
		return fmt.Errorf("listen daemon socket: %w", err)
	}
	unixListener := listener.(*net.UnixListener)
	unixListener.SetUnlinkOnClose(false)
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	if err := os.Chmod(bind, 0o600); err != nil {
		return errors.Join(fmt.Errorf("protect daemon socket: %w", err), os.Remove(bind))
	}
	if err := os.Rename(bind, socket); err != nil {
		return errors.Join(fmt.Errorf("publish daemon socket: %w", err), os.Remove(bind))
	}
	socketInfo, err := os.Lstat(socket)
	if err != nil {
		return err
	}
	// removeSocket leaves a successor's socket at the same path in place.
	removeSocket := sync.OnceValue(func() error {
		current, err := os.Lstat(socket)
		if err == nil && os.SameFile(socketInfo, current) {
			return os.Remove(socket)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
	defer func() { returnErr = errors.Join(returnErr, removeSocket()) }()
	runtimeService := runtimeForService(runtime)
	service, err := harvestmcp.NewConfiguredHarvester(runtime.Version, runtimeService)
	if err != nil {
		return fmt.Errorf("configure local MCP service: %w", err)
	}
	resources := []func() error{service.Close}
	var resourceOnce sync.Once
	var resourceErr error
	closeResources := func() error {
		resourceOnce.Do(func() {
			for _, closeResource := range resources {
				resourceErr = errors.Join(resourceErr, closeResource())
			}
		})
		return resourceErr
	}
	defer func() { returnErr = errors.Join(returnErr, closeResources()) }()
	gate := &admission{}
	handoff := false // set with the first accepted /stop, under the gate's lock
	stop := make(chan struct{})
	var stopOnce sync.Once
	requestDrain := func() { gate.drain(); stopOnce.Do(func() { close(stop) }) }
	digest, err := identity(runtime)
	if err != nil {
		return err
	}
	status := StatusInfo{Service: serviceMarker, PID: os.Getpid(), Version: runtime.Version, Started: time.Now().UTC(), Endpoint: socket, Gateway: runtime.Config.Harvester.External.Enabled, ConfigPath: runtime.Config.Path, ConfigDigest: digest}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		gate.Lock()
		snapshot := status
		snapshot.UptimeSeconds = time.Since(status.Started).Seconds()
		snapshot.Draining = gate.draining
		gate.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(snapshot); err != nil {
			obs.Logger(r.Context()).Warn("write daemon health", obs.FieldErr, err.Error())
		}
	})
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var expected StatusInfo
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&expected); err != nil {
			http.Error(w, "expected daemon instance required", http.StatusBadRequest)
			return
		}
		gate.Lock()
		if !sameInstance(expected, status) {
			gate.Unlock()
			http.Error(w, "daemon instance changed before drain", http.StatusConflict)
			return
		}
		if !gate.draining {
			handoff = r.URL.Query().Get("mode") != stopShutdown
		}
		gate.draining = true
		gate.Unlock()
		stopOnce.Do(func() { close(stop) })
		w.WriteHeader(http.StatusAccepted)
	})
	var mcpHandler http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return service.Server() }, &mcp.StreamableHTTPOptions{JSONResponse: true, Stateless: false, DisableLocalhostProtection: false})
	if admittedHandler != nil {
		mcpHandler = admittedHandler
	}
	mux.Handle("/mcp", gate.handler(harvestmcp.LinkClientDisconnect(mcpHandler)))
	requests, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	baseContext := func(net.Listener) context.Context { return requests }
	local := &http.Server{Handler: mux, BaseContext: baseContext, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute}
	servers := []*http.Server{local}
	listeners := []net.Listener{listener}
	var gateway *harvestmcp.RemoteServer
	if status.Gateway {
		remote, server, err := newGateway(runtime, runtimeService)
		if err != nil {
			return err
		}
		gateway = remote
		resources = append(resources, remote.Close)
		external, err := net.Listen("tcp", server.Addr)
		if err != nil {
			return fmt.Errorf("listen external gateway %s: %w", server.Addr, err)
		}
		server.BaseContext = baseContext
		server.Handler = gate.handler(server.Handler)
		servers = append(servers, server)
		listeners = append(listeners, external)
	}
	done := make(chan error, len(servers))
	for index, server := range servers {
		go func(server *http.Server, listener net.Listener) { done <- server.Serve(listener) }(server, listeners[index])
	}
	var serveErr error
	handingOff := false
	select {
	case <-ctx.Done():
	case <-stop:
		gate.Lock()
		handingOff = handoff
		gate.Unlock()
	case serveErr = <-done:
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	requestDrain()
	window := drainWindow
	if handingOff {
		window = handoffWindow
		// Stop accepting, then give the endpoint up: the successor takes the lock and binds the socket
		// while the admitted calls finish on their open connections, each closed once it answers.
		for _, server := range servers {
			server.SetKeepAlivesEnabled(false)
		}
		for _, listener := range listeners {
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				serveErr = errors.Join(serveErr, fmt.Errorf("close daemon listener for handoff: %w", err))
			}
		}
		for range servers {
			if err := <-done; err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
				serveErr = errors.Join(serveErr, err)
			}
		}
		// The successor loads the gateway's credential state when it takes the lock: no write of this
		// daemon's may land after that, or one would erase the other's clients and refresh tokens.
		gateway.HandOff()
		serveErr = errors.Join(serveErr, removeSocket(), releaseLock())
		obs.Logger(ctx).Info("harvester daemon handed off to its successor; finishing admitted calls", "window", window.String())
	}
	markUnsafe := func(cleanupErr error) {
		ownership := "retaining daemon ownership"
		if handingOff {
			ownership = "the successor already owns the daemon"
		}
		unsafe := fmt.Errorf("unsafe cleanup: owned work remains after cancellation; %s: %w", ownership, cleanupErr)
		gate.Lock()
		status.CleanupError = unsafe.Error()
		gate.Unlock()
		obs.Logger(ctx).Error("harvester daemon unsafe cleanup", obs.FieldErr, unsafe.Error(), "path", paths.DaemonLog(runtime.Paths.Home))
		serveErr = errors.Join(serveErr, unsafe)
	}
	grace, cancelGrace := context.WithTimeout(context.Background(), window)
	drainErr := gate.wait(grace)
	cancelGrace()
	cleanup, cancelCleanup := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancelCleanup()
	if drainErr != nil {
		// Keep health reachable while cancellation reaps per-request browser workers.
		cancelRequests()
		if cleanupErr := gate.wait(cleanup); cleanupErr != nil {
			markUnsafe(cleanupErr)
			// Bounded callers fail visibly; the owner must not exit and unlock live work.
			_ = gate.wait(context.Background())
		}
		serveErr = errors.Join(serveErr, drainErr)
	}
	resourcesDone := make(chan error, 1)
	go func() { resourcesDone <- closeResources() }()
	select {
	case err := <-resourcesDone:
		serveErr = errors.Join(serveErr, err)
	case <-cleanup.Done():
		markUnsafe(fmt.Errorf("close owned worker resources: %w", cleanup.Err()))
		serveErr = errors.Join(serveErr, <-resourcesDone)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), drainWindow)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdown); err != nil {
			serveErr = errors.Join(serveErr, fmt.Errorf("drain daemon: %w", err), server.Close())
		}
	}
	return serveErr
}
