//go:build !windows

package internal

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func prepareCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}

	rootPID := cmd.Process.Pid
	descendants := descendantPIDs(rootPID)

	var errs []error
	if err := signalProcessGroup(rootPID, syscall.SIGTERM); err != nil {
		errs = append(errs, err)
	}
	signalProcesses(descendants, syscall.SIGTERM)

	time.Sleep(500 * time.Millisecond)

	if err := signalProcessGroup(rootPID, syscall.SIGKILL); err != nil {
		errs = append(errs, err)
	}
	signalProcesses(descendants, syscall.SIGKILL)
	return errors.Join(errs...)
}

func signalProcessGroup(pid int, sig syscall.Signal) error {
	err := syscall.Kill(-pid, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func signalProcesses(pids []int, sig syscall.Signal) {
	for i := len(pids) - 1; i >= 0; i-- {
		_ = syscall.Kill(pids[i], sig)
	}
}

func descendantPIDs(rootPID int) []int {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}

	children := make(map[int][]int)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}

	var pids []int
	var walk func(int)
	walk = func(pid int) {
		for _, child := range children[pid] {
			pids = append(pids, child)
			walk(child)
		}
	}
	walk(rootPID)
	return pids
}
