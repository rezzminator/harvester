// Package gather exposes only process liveness needed by environment pruning.
package gather

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type ProcFS interface{ PIDs() ([]int, error) }
type ProcExecutable interface{ Executable(int) (string, error) }
type ProcessIdentity struct {
	EffectiveUID uint32
	Command      string
	Zombie       bool
}
type ProcIdentity interface {
	ProcessIdentity(int) (ProcessIdentity, error)
}
type RealProcFS struct{ Root string }

func NewProcFS(root string) ProcFS {
	if root != "" {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			if filepath.Clean(root) == "/proc" {
				return nativeProcFS()
			}
			return RealProcFS{Root: root}
		}
	}
	return nativeProcFS()
}

func (proc RealProcFS) root() string {
	if proc.Root == "" {
		return "/proc"
	}
	return proc.Root
}

func (proc RealProcFS) PIDs() ([]int, error) {
	entries, err := os.ReadDir(proc.root())
	if err != nil {
		return nil, fmt.Errorf("read proc root: %w", err)
	}
	pids := make([]int, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids, nil
}

func (proc RealProcFS) Executable(pid int) (string, error) {
	path, err := os.Readlink(proc.path(pid, "exe"))
	if err != nil {
		return "", fmt.Errorf("read process %d executable: %w", pid, err)
	}
	return strings.TrimSuffix(path, " (deleted)"), nil
}

func (proc RealProcFS) path(pid int, element string) string {
	return filepath.Join(proc.root(), strconv.Itoa(pid), element)
}

func procExecutablePath(buffer []byte) (string, error) {
	if len(buffer) < 5 {
		return "", fmt.Errorf("short kern.procargs2 executable buffer: %d bytes", len(buffer))
	}
	path, _, found := strings.Cut(string(buffer[4:]), "\x00")
	if !found || !filepath.IsAbs(path) {
		return "", fmt.Errorf("invalid kern.procargs2 executable path %q", path)
	}
	return filepath.Clean(path), nil
}

func processReadError(pid int, readErr, probeErr error) error {
	if pid > 0 && errors.Is(probeErr, syscall.ESRCH) {
		return fmt.Errorf("process %d disappeared: %w", pid, fs.ErrNotExist)
	}
	if probeErr != nil && !errors.Is(probeErr, syscall.EPERM) {
		return errors.Join(readErr, fmt.Errorf("probe process %d existence: %w", pid, probeErr))
	}
	return readErr
}
