// Package daemon owns the persistent harvester and its stdio proxy.
package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/paths"
)

const serviceMarker = "harvester-daemon"
const startupTimeout = 15 * time.Second
const drainTimeout = 30 * time.Second
const cleanupTimeout = 2 * time.Second

var ErrDaemonAbsent = errors.New("harvester daemon is absent")
var ErrDaemonUnresponsive = errors.New("harvester daemon is unresponsive")

type StatusInfo struct {
	Service       string    `json:"service"`
	PID           int       `json:"pid"`
	Version       string    `json:"version"`
	Started       time.Time `json:"started"`
	UptimeSeconds float64   `json:"uptime_seconds"`
	Endpoint      string    `json:"endpoint"`
	Gateway       bool      `json:"gateway"`
	Draining      bool      `json:"draining"`
	CleanupError  string    `json:"cleanup_error,omitempty"`
	ConfigPath    string    `json:"config_path"`
	ConfigDigest  string    `json:"config_digest,omitempty"`
}

func identity(runtime config.Runtime) (string, error) {
	body, err := json.Marshal(struct {
		Config                     config.Config
		Home, Python, State, Cache string
	}{runtime.Config, runtime.Paths.Home, os.Getenv("HARVESTER_PYTHON_ROOT"), paths.StateRoot(runtime.Paths.Home), os.Getenv("XDG_CACHE_HOME")})
	if err != nil {
		return "", fmt.Errorf("encode effective daemon configuration: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(body)), nil
}

func unixClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
	}}}
}

func privateRoot(home string) error {
	root := paths.StateRoot(home)
	if !filepath.IsAbs(root) {
		return fmt.Errorf("daemon state root must be absolute: %s", root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create daemon state root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse daemon state root that is not a directory: %s", root)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("daemon state root is not owned by this user: %s", root)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("protect daemon state root: %w", err)
	}
	return nil
}

func checkPrivate(path string, kind os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode().Type() != kind || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("refuse unsafe daemon state %s mode=%s (need 0600)", path, info.Mode())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("refuse daemon state owned by another user: %s", path)
	}
	return nil
}

func Status(ctx context.Context, runtime config.Runtime) (StatusInfo, error) {
	socket := paths.DaemonSocket(runtime.Paths.Home)
	if err := checkPrivate(socket, os.ModeSocket); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StatusInfo{}, fmt.Errorf("%w at %s", ErrDaemonAbsent, socket)
		}
		return StatusInfo{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	client := unixClient(socket)
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://localhost/healthz", nil)
	if err != nil {
		return StatusInfo{}, err
	}
	response, err := client.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			return StatusInfo{}, fmt.Errorf("%w at %s: %w", ErrDaemonAbsent, socket, err)
		}
		return StatusInfo{}, fmt.Errorf("%w at %s: %w", ErrDaemonUnresponsive, socket, err)
	}
	defer response.Body.Close()
	var status StatusInfo
	if response.StatusCode != http.StatusOK {
		return status, fmt.Errorf("daemon health at %s returned HTTP %d", socket, response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&status); err != nil {
		return status, fmt.Errorf("decode daemon health at %s: %w", socket, err)
	}
	if status.Service != serviceMarker || status.PID <= 0 || status.Version == "" || status.Started.IsZero() || status.Endpoint != socket {
		return status, fmt.Errorf("foreign or malformed daemon health at %s", socket)
	}
	return status, nil
}

func loadCurrent(runtime config.Runtime) (config.Runtime, error) {
	if runtime.Config.Path == "" {
		return runtime, nil
	}
	current, err := config.LoadRuntime(runtime.Config.Path)
	if err != nil {
		return runtime, fmt.Errorf("reload daemon configuration: %w", err)
	}
	current.Version = runtime.Version
	return current, nil
}

func Ensure(ctx context.Context, runtime config.Runtime) (StatusInfo, error) {
	return ensure(ctx, runtime, false)
}
func Refresh(ctx context.Context, runtime config.Runtime) error {
	_, err := ensure(ctx, runtime, true)
	return err
}

func ensure(ctx context.Context, runtime config.Runtime, refresh bool) (StatusInfo, error) {
	runtime, err := loadCurrent(runtime)
	if err != nil {
		return StatusInfo{}, err
	}
	if runtime.Version == "" {
		runtime.Version = "dev"
	}
	if err := privateRoot(runtime.Paths.Home); err != nil {
		return StatusInfo{}, err
	}
	digest, err := identity(runtime)
	if err != nil {
		return StatusInfo{}, err
	}
	window := startupTimeout + drainTimeout
	bounded, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	started := false
	var versionBinary, version string
	var versionInfo os.FileInfo
	// Poll trust and file identity during startup/drain without spawning a version process each time.
	readVersion := func(current config.Runtime) (string, error) {
		binary, err := executable(current)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(binary)
		if err != nil {
			return "", err
		}
		if versionInfo != nil && binary == versionBinary && os.SameFile(info, versionInfo) && info.Size() == versionInfo.Size() && info.ModTime().Equal(versionInfo.ModTime()) {
			return version, nil
		}
		version, err = installedVersion(bounded, current)
		if err != nil {
			return "", err
		}
		versionBinary, versionInfo = binary, info
		return version, nil
	}
	expectedVersion, err := readVersion(runtime)
	if err != nil {
		return StatusInfo{}, fmt.Errorf("resolve current daemon build; see %s: %w", paths.DaemonLog(runtime.Paths.Home), err)
	}
	for {
		runtime, err = loadCurrent(runtime)
		if err != nil {
			return StatusInfo{}, err
		}
		digest, err = identity(runtime)
		if err != nil {
			return StatusInfo{}, err
		}
		expectedVersion, err = readVersion(runtime)
		if err != nil {
			return StatusInfo{}, err
		}
		status, probeErr := Status(bounded, runtime)
		if probeErr == nil && status.CleanupError != "" {
			return StatusInfo{}, fmt.Errorf("daemon unsafe cleanup; see %s: %s", paths.DaemonLog(runtime.Paths.Home), status.CleanupError)
		}
		if probeErr == nil {
			if status.ConfigPath != runtime.Config.Path {
				return StatusInfo{}, fmt.Errorf("daemon configuration mismatch at %s: running %s requested %s", status.Endpoint, status.ConfigPath, runtime.Config.Path)
			}
			if !status.Draining && !refresh && status.Version == expectedVersion && status.ConfigDigest == digest {
				return status, nil
			}
			if !status.Draining {
				latest, err := loadCurrent(runtime)
				if err != nil {
					return StatusInfo{}, err
				}
				latestDigest, err := identity(latest)
				if err != nil {
					return StatusInfo{}, err
				}
				latestVersion, err := readVersion(latest)
				if err != nil {
					return StatusInfo{}, err
				}
				if latestDigest != digest || latestVersion != expectedVersion {
					runtime = latest
					continue
				}
				if err := joinDrain(bounded, runtime, status, readVersion); err != nil {
					return StatusInfo{}, err
				}
				refresh = false
				started = false
			}
		} else if !errors.Is(probeErr, ErrDaemonAbsent) {
			return StatusInfo{}, probeErr
		} else if !started {
			released, err := lockReleased(runtime)
			if err != nil {
				return StatusInfo{}, err
			}
			if released {
				if err := startDetached(runtime); err != nil {
					return StatusInfo{}, fmt.Errorf("start harvester daemon; see %s: %w", paths.DaemonLog(runtime.Paths.Home), err)
				}
				started = true
				refresh = false
			}
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-bounded.Done():
			timer.Stop()
			return StatusInfo{}, fmt.Errorf("wait for harvester daemon; see %s: %w", paths.DaemonLog(runtime.Paths.Home), bounded.Err())
		case <-timer.C:
		}
	}
}

func executable(runtime config.Runtime) (string, error) {
	stable := filepath.Join(runtime.Paths.Home, ".local", "bin", "harvester")
	if _, err := os.Lstat(stable); err == nil {
		return trustedExecutable(stable)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect installed harvester %s: %w", stable, err)
	}
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve harvester executable: %w", err)
	}
	return trustedExecutable(path)
}

func trustedExecutable(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect harvester executable %s: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 || !ok || (int(stat.Uid) != os.Getuid() && stat.Uid != 0) {
		return "", fmt.Errorf("refuse untrusted or nonexecutable harvester binary: %s", path)
	}
	return path, nil
}

func installedVersion(ctx context.Context, runtime config.Runtime) (string, error) {
	binary, err := executable(runtime)
	if err != nil {
		return "", err
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	command := exec.CommandContext(bounded, binary, "version")
	output, err := command.CombinedOutput()
	fields := strings.Fields(string(output))
	if err != nil || len(fields) != 2 || fields[0] != "harvester" {
		return "", fmt.Errorf("read installed harvester version from %s: %s: %w", binary, output, errors.Join(err, errors.New("invalid version response")))
	}
	return fields[1], nil
}

func startDetached(runtime config.Runtime) (returnErr error) {
	binary, err := executable(runtime)
	if err != nil {
		return err
	}
	logPath := paths.DaemonLog(runtime.Paths.Home)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	fd, err := syscall.Open(logPath, syscall.O_WRONLY|syscall.O_APPEND|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	log := os.NewFile(uintptr(fd), logPath)
	defer func() { returnErr = errors.Join(returnErr, log.Close()) }()
	if err := log.Chmod(0o600); err != nil {
		return err
	}
	input, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, input.Close()) }()
	command := exec.Command(binary, "serve")
	command.Env = append(os.Environ(), "HARVESTER_HOME_DIR="+runtime.Paths.Home, "HARVESTER_CONFIG="+runtime.Config.Path)
	command.Stdin = input
	command.Stdout = log
	command.Stderr = log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func requestStop(ctx context.Context, runtime config.Runtime, expected StatusInfo) error {
	client := unixClient(paths.DaemonSocket(runtime.Paths.Home))
	defer client.CloseIdleConnections()
	body, err := json.Marshal(expected)
	if err != nil {
		return fmt.Errorf("encode expected daemon instance: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/stop", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request daemon drain: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("daemon refused drain: HTTP %d", response.StatusCode)
	}
	return nil
}

func sameInstance(first, second StatusInfo) bool {
	return first.PID == second.PID && first.Started.Equal(second.Started) && first.Version == second.Version && first.Endpoint == second.Endpoint && first.ConfigPath == second.ConfigPath && first.ConfigDigest == second.ConfigDigest
}

func joinDrain(ctx context.Context, runtime config.Runtime, expected StatusInfo, readVersion func(config.Runtime) (string, error)) error {
	stopErr := requestStop(ctx, runtime, expected)
	if stopErr == nil {
		return nil
	}
	current, probeErr := Status(ctx, runtime)
	if errors.Is(probeErr, ErrDaemonAbsent) {
		_, lockErr := lockReleased(runtime)
		return lockErr
	}
	if probeErr != nil {
		return errors.Join(stopErr, fmt.Errorf("confirm concurrent daemon drain: %w", probeErr))
	}
	if current.ConfigPath != runtime.Config.Path {
		return errors.Join(stopErr, errors.New("daemon configuration changed during drain"))
	}
	if sameInstance(current, expected) && current.Draining {
		return nil
	}
	latest, err := loadCurrent(runtime)
	if err != nil {
		return errors.Join(stopErr, err)
	}
	digest, err := identity(latest)
	if err != nil {
		return errors.Join(stopErr, err)
	}
	version, err := readVersion(latest)
	if err != nil {
		return errors.Join(stopErr, err)
	}
	if !sameInstance(current, expected) && !current.Draining && current.Version == version && current.ConfigDigest == digest {
		return nil
	}
	return stopErr
}

func Stop(ctx context.Context, runtime config.Runtime) error {
	status, err := Status(ctx, runtime)
	if err != nil && !errors.Is(err, ErrDaemonAbsent) {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, drainTimeout+cleanupTimeout+time.Second)
	defer cancel()
	if err == nil {
		if err := requestStop(bounded, runtime, status); err != nil {
			current, probeErr := Status(bounded, runtime)
			if !errors.Is(probeErr, ErrDaemonAbsent) && !(probeErr == nil && sameInstance(status, current) && current.Draining) {
				return errors.Join(err, probeErr)
			}
		}
	}
	for {
		current, err := Status(bounded, runtime)
		if err == nil && current.CleanupError != "" {
			return fmt.Errorf("daemon unsafe cleanup; see %s: %s", paths.DaemonLog(runtime.Paths.Home), current.CleanupError)
		}
		if errors.Is(err, ErrDaemonAbsent) {
			released, err := lockReleased(runtime)
			if err != nil {
				return err
			}
			if released {
				return nil
			}
		}
		if err != nil && !errors.Is(err, ErrDaemonAbsent) && !errors.Is(err, ErrDaemonUnresponsive) {
			return err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-bounded.Done():
			timer.Stop()
			return fmt.Errorf("wait for daemon drain: %w", bounded.Err())
		case <-timer.C:
		}
	}
}

func lockReleased(runtime config.Runtime) (bool, error) {
	path := paths.DaemonLock(runtime.Paths.Home)
	if err := checkPrivate(path, 0); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false, fmt.Errorf("probe daemon lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil
		}
		return false, fmt.Errorf("probe daemon lock ownership: %w", err)
	}
	return true, nil
}
