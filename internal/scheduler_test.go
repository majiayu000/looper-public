package internal

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerStartStop(t *testing.T) {
	jobs := []JobConfig{
		{Name: "fast_job", Schedule: "@every 1s", Command: "echo tick", Workdir: ".", Type: "script"},
	}

	var count atomic.Int32
	hook := func(name string, result *RunResult, err error) {
		count.Add(1)
	}

	s := NewScheduler(NewRunner(t.TempDir(), nil, nil), hook)
	if err := s.Load(jobs); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	s.Start()

	time.Sleep(2500 * time.Millisecond)
	s.Stop()

	c := count.Load()
	if c < 2 {
		t.Errorf("expected at least 2 ticks, got %d", c)
	}
}

func TestSchedulerReload(t *testing.T) {
	jobs1 := []JobConfig{
		{Name: "job_a", Schedule: "@every 1s", Command: "echo a", Workdir: ".", Type: "script"},
	}
	jobs2 := []JobConfig{
		{Name: "job_b", Schedule: "@every 1s", Command: "echo b", Workdir: ".", Type: "script"},
	}

	var mu sync.Mutex
	var names []string
	hook := func(name string, result *RunResult, err error) {
		mu.Lock()
		defer mu.Unlock()
		names = append(names, name)
	}

	s := NewScheduler(NewRunner(t.TempDir(), nil, nil), hook)
	if err := s.Load(jobs1); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	s.Start()
	time.Sleep(1500 * time.Millisecond)

	// Reload with different jobs
	if err := s.Reload(jobs2); err != nil {
		t.Fatalf("Reload failed: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	s.Stop()

	// Should have both job_a and job_b in names
	mu.Lock()
	defer mu.Unlock()
	hasA, hasB := false, false
	for _, n := range names {
		if n == "job_a" {
			hasA = true
		}
		if n == "job_b" {
			hasB = true
		}
	}
	if !hasA || !hasB {
		t.Errorf("expected both job_a and job_b, got %v", names)
	}
}

func TestSchedulerInvalidCron(t *testing.T) {
	jobs := []JobConfig{
		{Name: "bad", Schedule: "not a cron", Command: "echo", Workdir: ".", Type: "script"},
	}
	s := NewScheduler(NewRunner(t.TempDir(), nil, nil), nil)
	err := s.Load(jobs)
	if err == nil {
		t.Fatal("expected error for invalid cron expression")
	}
}

func TestSchedulerReloadPrunesRemovedStatus(t *testing.T) {
	jobs1 := []JobConfig{
		{Name: "job_a", Schedule: "@every 10s", Command: "echo a", Workdir: ".", Type: "script"},
	}
	jobs2 := []JobConfig{
		{Name: "job_b", Schedule: "@every 10s", Command: "echo b", Workdir: ".", Type: "script"},
	}

	s := NewScheduler(NewRunner(t.TempDir(), nil, nil), nil)
	if err := s.Load(jobs1); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := s.JobStatuses(); len(got) != 1 || got[0].Name != "job_a" {
		t.Fatalf("unexpected statuses after first load: %+v", got)
	}

	if err := s.Reload(jobs2); err != nil {
		t.Fatalf("Reload failed: %v", err)
	}
	got := s.JobStatuses()
	if len(got) != 1 || got[0].Name != "job_b" {
		t.Fatalf("expected only job_b status after reload, got %+v", got)
	}
}

func TestSchedulerSkipsBusyWorkdirWithoutMarkingRunning(t *testing.T) {
	workdir := t.TempDir()
	runner := NewRunner(t.TempDir(), nil, nil)
	s := NewScheduler(runner, nil)
	jobs := []JobConfig{
		{Name: "slow", Schedule: "@every 10s", Command: "sleep 1", Workdir: workdir, Type: "script"},
		{Name: "blocked", Schedule: "@every 10s", Command: "echo blocked", Workdir: workdir, Type: "script"},
	}
	if err := s.Load(jobs); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	go s.runJob(jobs[0])
	waitForStatus(t, s, "slow", func(st JobStatus) bool {
		return st.Running
	})

	s.runJob(jobs[1])

	for _, st := range s.JobStatuses() {
		if st.Name != "blocked" {
			continue
		}
		if st.Running {
			t.Fatalf("blocked job should not be marked running while workdir is busy: %+v", st)
		}
		if st.LastRunAt != "" {
			t.Fatalf("blocked job should not record a run when workdir is busy: %+v", st)
		}
		return
	}
	t.Fatal("blocked job status not found")
}

func TestSchedulerAllowsSameWorkdirWithDifferentLockKeys(t *testing.T) {
	workdir := t.TempDir()
	runner := NewRunner(t.TempDir(), nil, nil)
	s := NewScheduler(runner, nil)
	jobs := []JobConfig{
		{Name: "draft", Schedule: "@every 10s", Command: "sleep 1", Workdir: workdir, LockKey: "reply-draft", Type: "script"},
		{Name: "review", Schedule: "@every 10s", Command: "echo review", Workdir: workdir, LockKey: "reply-review", Type: "script"},
	}
	if err := s.Load(jobs); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	go s.runJob(jobs[0])
	waitForStatus(t, s, "draft", func(st JobStatus) bool {
		return st.Running
	})

	s.runJob(jobs[1])

	waitForStatus(t, s, "review", func(st JobStatus) bool {
		return st.LastRunAt != "" && !st.Running && st.LastExitCode == 0
	})
	waitForStatus(t, s, "draft", func(st JobStatus) bool {
		return !st.Running && st.LastExitCode == 0
	})
}

func TestSchedulerSkipsMatchingLockKeyWithoutMarkingRunning(t *testing.T) {
	workdir := t.TempDir()
	runner := NewRunner(t.TempDir(), nil, nil)
	s := NewScheduler(runner, nil)
	jobs := []JobConfig{
		{Name: "slow", Schedule: "@every 10s", Command: "sleep 1", Workdir: workdir, LockKey: "reply", Type: "script"},
		{Name: "blocked", Schedule: "@every 10s", Command: "echo blocked", Workdir: workdir, LockKey: "reply", Type: "script"},
	}
	if err := s.Load(jobs); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	go s.runJob(jobs[0])
	waitForStatus(t, s, "slow", func(st JobStatus) bool {
		return st.Running
	})

	s.runJob(jobs[1])

	for _, st := range s.JobStatuses() {
		if st.Name != "blocked" {
			continue
		}
		if st.Running {
			t.Fatalf("blocked job should not be marked running while lock key is busy: %+v", st)
		}
		if st.LastRunAt != "" {
			t.Fatalf("blocked job should not record a run when lock key is busy: %+v", st)
		}
		waitForStatus(t, s, "slow", func(st JobStatus) bool {
			return !st.Running && st.LastExitCode == 0
		})
		return
	}
	t.Fatal("blocked job status not found")
}

func TestSchedulerRunAfterSuccessTriggersAfterWorkdirUnlock(t *testing.T) {
	workdir := t.TempDir()
	runner := NewRunner(t.TempDir(), nil, nil)
	s := NewScheduler(runner, nil)
	jobs := []JobConfig{
		{
			Name:            "parent",
			Schedule:        "@every 10s",
			Command:         "echo parent",
			Workdir:         workdir,
			Type:            "script",
			RunAfterSuccess: []string{"child"},
		},
		{Name: "child", Schedule: "@every 10s", Command: "echo child", Workdir: workdir, Type: "script"},
	}
	if err := s.Load(jobs); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	s.runJob(jobs[0])

	waitForStatus(t, s, "child", func(st JobStatus) bool {
		return st.LastRunAt != "" && !st.Running && st.LastExitCode == 0
	})
}

func TestSchedulerRunAfterSuccessSkipsFailedJob(t *testing.T) {
	workdir := t.TempDir()
	runner := NewRunner(t.TempDir(), nil, nil)
	s := NewScheduler(runner, nil)
	jobs := []JobConfig{
		{
			Name:            "parent",
			Schedule:        "@every 10s",
			Command:         "exit 1",
			Workdir:         workdir,
			Type:            "script",
			RunAfterSuccess: []string{"child"},
		},
		{Name: "child", Schedule: "@every 10s", Command: "echo child", Workdir: workdir, Type: "script"},
	}
	if err := s.Load(jobs); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	s.runJob(jobs[0])

	for _, st := range s.JobStatuses() {
		if st.Name == "child" && st.LastRunAt != "" {
			t.Fatalf("child should not run after failed parent: %+v", st)
		}
	}
}

func waitForStatus(t *testing.T, s *Scheduler, name string, pred func(JobStatus) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, st := range s.JobStatuses() {
			if st.Name == name && pred(st) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for status %q", name)
}
