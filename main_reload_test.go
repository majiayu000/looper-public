//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"looper/internal"
)

func TestApplyConfigReloadPreservesActiveProcessGroups(t *testing.T) {
	for _, trigger := range []string{"manual", "cron"} {
		for _, rollback := range []bool{false, true} {
			for _, finish := range []string{"complete", "stop"} {
				t.Run(fmt.Sprintf("%s/rollback=%v/%s", trigger, rollback, finish), func(t *testing.T) {
					workdir := t.TempDir()
					job := internal.JobConfig{
						Name: "active", Schedule: "@every 1s", Type: "script", Timeout: "1m", Workdir: workdir,
						Command: `echo $$ > parent.pid; sh -c 'echo $$ > child.pid; while [ ! -e release ]; do sleep 0.05; done; echo completed > completed' & wait`,
					}
					if trigger == "manual" {
						job.Schedule = "@every 1h"
					}
					oldCfg := &internal.Config{Scheduling: internal.SchedulingConfig{Jobs: []internal.JobConfig{job}}}
					runner := internal.NewRunner(t.TempDir(), nil, nil)
					type completion struct {
						result *internal.RunResult
						err    error
					}
					completed := make(chan completion, 4)
					scheduler := internal.NewScheduler(runner, func(_ string, result *internal.RunResult, err error) {
						completed <- completion{result, err}
					})
					if err := scheduler.Load(oldCfg.Scheduling.Jobs); err != nil {
						t.Fatal(err)
					}
					parent := 0
					t.Cleanup(func() {
						scheduler.Stop()
						if parent != 0 {
							_ = syscall.Kill(-parent, syscall.SIGKILL)
						}
					})
					if trigger == "cron" {
						scheduler.Start()
					} else if err := scheduler.RunNow(job.Name); err != nil {
						t.Fatal(err)
					}
					parent = waitForReloadPID(t, filepath.Join(workdir, "parent.pid"))
					child := waitForReloadPID(t, filepath.Join(workdir, "child.pid"))
					for _, pid := range []int{parent, child} {
						pgid, err := syscall.Getpgid(pid)
						if err != nil || pgid != parent {
							t.Fatalf("PID %d group=%d error=%v; want job group %d", pid, pgid, err, parent)
						}
					}

					updatedJob := job
					updatedJob.Schedule = "@every 1h"
					newCfg := &internal.Config{Scheduling: internal.SchedulingConfig{Jobs: []internal.JobConfig{updatedJob}}}
					if rollback {
						newCfg.Scheduling.Jobs[0].Schedule = "not a cron"
					} else if finish == "stop" {
						// Terminal shutdown must still drain jobs removed by a reload.
						newCfg.Scheduling.Jobs = nil
					}
					current := oldCfg
					err := applyConfigReload(newCfg, &current, scheduler, runner, nil, nil)
					if rollback {
						if err == nil || !strings.Contains(err.Error(), "reload scheduler failed: invalid schedule") || current != oldCfg {
							t.Fatalf("reload failure contract changed: current=%p old=%p error=%v", current, oldCfg, err)
						}
					} else if err != nil || current != newCfg {
						t.Fatalf("reload failed: current=%p new=%p error=%v", current, newCfg, err)
					}
					for _, pid := range []int{parent, child} {
						if !reloadProcessAlive(t, pid) {
							t.Fatalf("reload killed active PID %d", pid)
						}
					}
					select {
					case c := <-completed:
						t.Fatalf("active job completed during reload: result=%+v error=%v", c.result, c.err)
					default:
					}

					if finish == "stop" {
						scheduler.Stop()
					} else if err := os.WriteFile(filepath.Join(workdir, "release"), nil, 0o600); err != nil {
						t.Fatal(err)
					}
					select {
					case c := <-completed:
						if finish == "stop" {
							if !errors.Is(c.err, context.Canceled) {
								t.Fatalf("shutdown callback error=%v; want cancellation", c.err)
							}
						} else {
							if c.err != nil || c.result == nil || c.result.ExitCode != 0 {
								t.Fatalf("job did not finish normally: result=%+v error=%v", c.result, c.err)
							}
							if _, err := os.Stat(filepath.Join(workdir, "completed")); err != nil {
								t.Fatalf("child did not complete its work: %v", err)
							}
						}
					case <-time.After(3 * time.Second):
						t.Fatal("job callback did not complete")
					}
					for _, pid := range []int{parent, child} {
						if reloadProcessAlive(t, pid) {
							t.Errorf("PID %d still running after %s", pid, finish)
						}
					}
				})
			}
		}
	}
}

func waitForReloadPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				return pid
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job did not write PID to %s", path)
	return 0
}

func reloadProcessAlive(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false
		}
		t.Fatalf("inspect PID %d: %v", pid, err)
	}
	stat := strings.TrimSpace(string(out))
	return stat != "" && !strings.HasPrefix(stat, "Z")
}
