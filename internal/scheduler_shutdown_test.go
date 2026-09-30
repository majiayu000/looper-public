//go:build !windows

package internal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSchedulerStopKillsProcessGroups(t *testing.T) {
	for _, trigger := range []string{"manual", "cron"} {
		for _, ignoreTerm := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ignore_term=%v", trigger, ignoreTerm), func(t *testing.T) {
				workdir := t.TempDir()
				child := `trap 'exit 0' TERM; echo $$ > child.pid; while :; do sleep 1; done`
				if ignoreTerm {
					child = `trap '' TERM; echo $$ > child.pid; while :; do sleep 1; done`
				}
				job := JobConfig{Name: "shutdown", Schedule: "@every 1s", Type: "script", Workdir: workdir,
					Timeout: "1m", Command: "echo $$ > parent.pid; sh -c " + shellQuote(child) + " & wait"}
				completed := make(chan error, 1)
				s := NewScheduler(NewRunner(t.TempDir(), nil, nil), func(_ string, _ *RunResult, err error) {
					completed <- err
				})
				if err := s.Load([]JobConfig{job}); err != nil {
					t.Fatal(err)
				}
				if trigger == "cron" {
					s.Start()
				} else if err := s.RunNow(job.Name); err != nil {
					t.Fatal(err)
				}
				parent := waitForShutdownPID(t, filepath.Join(workdir, "parent.pid"))
				t.Cleanup(func() {
					_ = syscall.Kill(-parent, syscall.SIGKILL)
					s.Stop()
				})
				childPID := waitForShutdownPID(t, filepath.Join(workdir, "child.pid"))
				for _, pid := range []int{parent, childPID} {
					pgid, err := syscall.Getpgid(pid)
					if err != nil || pgid != parent {
						t.Fatalf("PID %d group=%d error=%v; expected job group %d", pid, pgid, err, parent)
					}
				}

				start := time.Now()
				s.Stop()
				if elapsed := time.Since(start); elapsed > 3*time.Second {
					t.Errorf("Stop took %v for a canceled job", elapsed)
				}
				for _, pid := range []int{parent, childPID} {
					if shutdownProcessAlive(t, pid) {
						t.Errorf("PID %d still running after Stop", pid)
					}
				}
				select {
				case err := <-completed:
					if !errors.Is(err, context.Canceled) {
						t.Errorf("callback error=%v; expected cancellation", err)
					}
				default:
					t.Error("Stop returned before job callback completed")
				}
				for _, status := range s.JobStatuses() {
					if status.Running {
						t.Error("Stop returned with a running job status")
					}
				}
				if err := s.RunNow(job.Name); err == nil {
					t.Error("RunNow accepted work after Stop")
				}
			})
		}
	}
}

func TestSchedulerStopRejectsReload(t *testing.T) {
	s := NewScheduler(NewRunner(t.TempDir(), nil, nil), nil)
	job := JobConfig{Name: "reloaded", Schedule: "@every 1h", Type: "script", Workdir: t.TempDir(), Command: "echo reloaded"}
	if err := s.Load([]JobConfig{job}); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	s.Stop()
	s.Stop()
	for _, load := range []func([]JobConfig) error{s.Load, s.Reload} {
		if err := load([]JobConfig{job}); !errors.Is(err, context.Canceled) {
			t.Errorf("load after Stop error=%v; want cancellation", err)
		}
	}
	s.Start()
	if err := s.RunNow(job.Name); !errors.Is(err, context.Canceled) {
		t.Errorf("RunNow after Stop error=%v; want cancellation", err)
	}
}

func TestSchedulerStopRejectsConcurrentReload(t *testing.T) {
	for _, trigger := range []string{"manual", "cron"} {
		for _, load := range []string{"Load", "Reload"} {
			t.Run(trigger+"/"+load, func(t *testing.T) {
				entered := make(chan error, 1)
				release := make(chan struct{})
				stopped := make(chan struct{})
				s := NewScheduler(NewRunner(t.TempDir(), nil, nil), func(name string, _ *RunResult, err error) {
					if name == "active" {
						entered <- err
						<-release
					}
				})
				command := `echo $$ > parent.pid; sh -c 'trap "" TERM; echo $$ > child.pid; while :; do sleep 1; done' & wait`
				active := JobConfig{Name: "active", Schedule: "@every 1h", Type: "script", Timeout: "1m", Workdir: t.TempDir(), Command: command}
				fresh := active
				fresh.Name = "fresh"
				fresh.Workdir = t.TempDir()
				if trigger == "cron" {
					fresh.Schedule = "@every 1s"
				}
				if err := s.Load([]JobConfig{active}); err != nil {
					t.Fatal(err)
				}
				var parents []int
				released := false
				t.Cleanup(func() {
					if !released {
						close(release)
					}
					s.Stop()
					for _, pid := range parents {
						_ = syscall.Kill(-pid, syscall.SIGKILL)
					}
				})
				if err := s.RunNow(active.Name); err != nil {
					t.Fatal(err)
				}
				parent := waitForShutdownPID(t, filepath.Join(active.Workdir, "parent.pid"))
				parents = append(parents, parent)
				child := waitForShutdownPID(t, filepath.Join(active.Workdir, "child.pid"))
				for _, pid := range []int{parent, child} {
					if !shutdownProcessAlive(t, pid) {
						t.Fatalf("active PID %d did not start", pid)
					}
				}
				go func() { s.Stop(); close(stopped) }()
				select {
				case err := <-entered:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("active callback error=%v; want cancellation", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("Stop did not cancel the active job")
				}
				select {
				case <-stopped:
					t.Fatal("Stop returned before callback release")
				default:
				}
				// The blocked callback holds Stop in its unlocked drain phase.
				// Attempt the same pause/load/restart sequence as a watcher reload.
				s.Pause()
				var err error
				if load == "Reload" {
					err = s.Reload([]JobConfig{fresh})
				} else {
					err = s.Load([]JobConfig{fresh})
					s.Start()
				}
				if !errors.Is(err, context.Canceled) {
					t.Errorf("%s during Stop error=%v; want cancellation", load, err)
				}
				var freshPIDs []int
				if trigger == "manual" {
					observer := NewObserver(&Config{}, s, s.runner, time.Now())
					response := httptest.NewRecorder()
					runName := fresh.Name
					if err != nil {
						runName = active.Name
					}
					observer.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "http://127.0.0.1/run?job="+runName, nil))
					if response.Code != http.StatusNotFound {
						t.Errorf("/run during Stop status=%d body=%s; want existing rejection", response.Code, response.Body.String())
					}
					if err != nil && !strings.Contains(response.Body.String(), "scheduler stopped") {
						t.Errorf("/run cancellation error missing: %s", response.Body.String())
					}
				}
				if err == nil {
					// On the broken source, prove that actual new parent/child
					// processes start while Stop tracks only the old wait group.
					freshParent := waitForShutdownPID(t, filepath.Join(fresh.Workdir, "parent.pid"))
					parents = append(parents, freshParent)
					freshChild := waitForShutdownPID(t, filepath.Join(fresh.Workdir, "child.pid"))
					freshPIDs = []int{freshParent, freshChild}
					for _, pid := range freshPIDs {
						pgid, groupErr := syscall.Getpgid(pid)
						if groupErr != nil || pgid != freshParent {
							t.Fatalf("fresh PID %d group=%d error=%v; want %d", pid, pgid, groupErr, freshParent)
						}
					}
				}
				close(release)
				released = true
				select {
				case <-stopped:
				case <-time.After(3 * time.Second):
					t.Fatal("Stop did not drain the old job")
				}
				for _, pid := range append([]int{parent, child}, freshPIDs...) {
					if shutdownProcessAlive(t, pid) {
						t.Errorf("PID %d survived terminal Stop", pid)
					}
				}
				if err != nil {
					for _, name := range []string{"parent.pid", "child.pid"} {
						if _, statErr := os.Stat(filepath.Join(fresh.Workdir, name)); !errors.Is(statErr, os.ErrNotExist) {
							t.Errorf("fresh %s created after terminal Stop: %v", name, statErr)
						}
					}
				}
				if err := s.RunNow(active.Name); !errors.Is(err, context.Canceled) {
					t.Errorf("RunNow after drain error=%v; want cancellation", err)
				}
			})
		}
	}
}

func TestSchedulerStopWaitsForConcurrentJobs(t *testing.T) {
	completed := make(chan error, 2)
	s := NewScheduler(NewRunner(t.TempDir(), nil, nil), func(_ string, _ *RunResult, err error) { completed <- err })
	var jobs []JobConfig
	for i := 0; i < 2; i++ {
		jobs = append(jobs, JobConfig{Name: fmt.Sprintf("concurrent_%d", i), Schedule: "@every 1h", Type: "script",
			Workdir: t.TempDir(), Timeout: "1m", Command: "echo $$ > parent.pid; sleep 30 & wait"})
	}
	if err := s.Load(jobs); err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if err := s.RunNow(job.Name); err != nil {
			t.Fatal(err)
		}
		pid := waitForShutdownPID(t, filepath.Join(job.Workdir, "parent.pid"))
		t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); s.Stop() })
	}
	s.Stop()
	for i := 0; i < len(jobs); i++ {
		select {
		case err := <-completed:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("callback error=%v; expected cancellation", err)
			}
		default:
			t.Fatal("Stop returned before all concurrent jobs completed")
		}
	}
}

func TestSchedulerStopBoundsCallbackWait(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	s := NewScheduler(NewRunner(t.TempDir(), nil, nil), func(_ string, _ *RunResult, _ error) {
		close(entered)
		<-release
		close(finished)
	})
	job := JobConfig{Name: "callback", Schedule: "@every 1h", Type: "script", Workdir: t.TempDir(), Command: "echo done"}
	if err := s.Load([]JobConfig{job}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(release); <-finished; s.Stop() })
	if err := s.RunNow(job.Name); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not reach callback")
	}
	returned := make(chan struct{})
	start := time.Now()
	go func() { s.Stop(); close(returned) }()
	select {
	case <-returned:
		if time.Since(start) < schedulerShutdownTimeout {
			t.Fatal("Stop did not wait for the blocked callback")
		}
	case <-time.After(schedulerShutdownTimeout + 2*time.Second):
		t.Fatal("Stop did not bound the callback wait")
	}
}

func waitForShutdownPID(t *testing.T, path string) int {
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

func shutdownProcessAlive(t *testing.T, pid int) bool {
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
