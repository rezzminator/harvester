//go:build darwin

package gather

import (
	"fmt"
	"golang.org/x/sys/unix"
	"sort"
)

type DarwinProcFS struct{}

func NewDarwinProcFS() *DarwinProcFS { return &DarwinProcFS{} }
func (proc *DarwinProcFS) PIDs() ([]int, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("list processes via kern.proc.all: %w", err)
	}
	pids := make([]int, 0, len(processes))
	for index := range processes {
		if pid := int(processes[index].Proc.P_pid); pid > 0 {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids, nil
}

func (proc *DarwinProcFS) ProcessIdentity(pid int) (ProcessIdentity, error) {
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf(
			"read process identity for pid %d via kern.proc.pid: %w",
			pid,
			processReadError(pid, err, unix.Kill(pid, 0)),
		)
	}
	command := make([]byte, 0, len(process.Proc.P_comm))
	for _, character := range process.Proc.P_comm {
		if character == 0 {
			break
		}
		command = append(command, byte(character))
	}
	return ProcessIdentity{
		EffectiveUID: process.Eproc.Ucred.Uid,
		Zombie:       process.Proc.P_stat == 5, // SZOMB in Darwin sys/proc.h.
		Command:      string(command),
	}, nil
}

func (proc *DarwinProcFS) Executable(pid int) (string, error) {
	buffer, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", fmt.Errorf("read executable via kern.procargs2 for %d: %w", pid, err)
	}
	path, err := procExecutablePath(buffer)
	if err != nil {
		return "", fmt.Errorf("decode executable for %d: %w", pid, err)
	}
	return path, nil
}
