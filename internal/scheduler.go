package internal

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

type JobCallback func(name string, result *RunResult, err error)

type JobStatus struct {
	Name         string `json:"name"`
	Schedule     string `json:"schedule"`
	Type         string `json:"type"`
	Running      bool   `json:"running"`
	LastRunAt    string `json:"last_run_at,omitempty"`
	LastDuration string `json:"last_duration,omitempty"`
	LastExitCode int    `json:"last_exit_code"`
	LastError    string `json:"last_error,omitempty"`
	lastRunTime  time.Time
}

type Scheduler struct {
	runner   *Runner
	cron     *cron.Cron
	callback JobCallback
	jobs     map[string]JobConfig
	statuses map[string]*JobStatus
	mu       sync.Mutex
	locks    map[string]*sync.Mutex // keyed scheduler mutexes; default key is job workdir
}

const defaultJobTimeout = 30 * time.Minute

func NewScheduler(runner *Runner, callback JobCallback) *Scheduler {
	return &Scheduler{
		runner:   runner,
		callback: callback,
		jobs:     make(map[string]JobConfig),
		statuses: make(map[string]*JobStatus),
		locks:    make(map[string]*sync.Mutex),
	}
}

func (s *Scheduler) Load(jobs []JobConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(jobs)
}

func (s *Scheduler) loadLocked(jobs []JobConfig) error {
	c := cron.New()
	m := make(map[string]JobConfig, len(jobs))
	newStatuses := make(map[string]*JobStatus, len(jobs))
	for _, job := range jobs {
		j := job // capture
		_, err := c.AddFunc(j.Schedule, func() {
			s.runJob(j)
		})
		if err != nil {
			return fmt.Errorf("invalid schedule %q for job %s: %w", j.Schedule, j.Name, err)
		}
		m[j.Name] = j
		// Preserve existing status for active jobs; drop removed ones.
		st, ok := s.statuses[j.Name]
		if !ok {
			st = &JobStatus{
				Name: j.Name,
				Type: j.Type,
			}
		}
		st.Name = j.Name
		st.Schedule = j.Schedule
		st.Type = j.Type
		newStatuses[j.Name] = st
	}
	s.cron = c
	s.jobs = m
	s.statuses = newStatuses
	return nil
}

func (s *Scheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
}

func (s *Scheduler) startLocked() {
	if s.cron != nil {
		s.cron.Start()
	}
}

func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

func (s *Scheduler) stopLocked() {
	if s.cron != nil {
		s.cron.Stop()
		s.cron = nil
	}
}

func (s *Scheduler) Reload(jobs []JobConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	if err := s.loadLocked(jobs); err != nil {
		return err
	}
	s.startLocked()
	return nil
}

// RunNow triggers a job by name immediately in a goroutine.
func (s *Scheduler) RunNow(name string) error {
	s.mu.Lock()
	job, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("job not found: %s", name)
	}
	go s.runJob(job)
	return nil
}

// JobNames returns all registered job names.
func (s *Scheduler) JobNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.jobs))
	for name := range s.jobs {
		names = append(names, name)
	}
	return names
}

// JobStatuses returns status of all jobs sorted by name.
func (s *Scheduler) JobStatuses() []JobStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]JobStatus, 0, len(s.statuses))
	for _, st := range s.statuses {
		result = append(result, *st)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

// lock returns the mutex for a scheduler lock key, creating one if needed.
func (s *Scheduler) lock(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lk, ok := s.locks[key]
	if !ok {
		lk = &sync.Mutex{}
		s.locks[key] = lk
	}
	return lk
}

func jobLockKey(job JobConfig) string {
	key := strings.TrimSpace(job.LockKey)
	if key != "" {
		return key
	}
	return job.Workdir
}

func (s *Scheduler) runJob(job JobConfig) {
	s.mu.Lock()
	st := s.statuses[job.Name]
	if st == nil {
		st = &JobStatus{Name: job.Name, Schedule: job.Schedule, Type: job.Type}
		s.statuses[job.Name] = st
	}
	if st.Running {
		s.mu.Unlock()
		slog.Warn("job skipped: already running", "name", job.Name)
		return
	}
	s.mu.Unlock()

	// Jobs sharing a lock key contend for the same runtime state and DB files.
	// When lock_key is omitted, the workdir remains the lock key for compatibility.
	lockKey := jobLockKey(job)
	jobLock := s.lock(lockKey)
	if !jobLock.TryLock() {
		slog.Warn("job skipped: lock busy", "name", job.Name, "workdir", job.Workdir, "lock_key", lockKey)
		return
	}
	locked := true
	defer func() {
		if locked {
			jobLock.Unlock()
		}
	}()

	s.mu.Lock()
	st = s.statuses[job.Name]
	if st == nil {
		st = &JobStatus{Name: job.Name, Schedule: job.Schedule, Type: job.Type}
		s.statuses[job.Name] = st
	}
	if st.Running {
		s.mu.Unlock()
		slog.Warn("job skipped: already running", "name", job.Name)
		return
	}
	st.Running = true
	st.lastRunTime = time.Now()
	st.LastRunAt = st.lastRunTime.Format(time.RFC3339)
	st.LastError = ""
	s.mu.Unlock()

	timeout, err := jobTimeout(job)
	if err != nil {
		s.mu.Lock()
		st.Running = false
		st.LastDuration = time.Since(st.lastRunTime).Truncate(time.Second).String()
		st.LastExitCode = -1
		st.LastError = err.Error()
		s.mu.Unlock()
		slog.Error("job failed", "name", job.Name, "error", err)
		jobLock.Unlock()
		locked = false
		if s.callback != nil {
			s.callback(job.Name, nil, err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	slog.Info("job started", "name", job.Name, "type", job.Type)
	start := time.Now()

	result, err := s.runner.Run(ctx, job)
	completedSuccessfully := err == nil && result != nil && result.ExitCode == 0

	s.mu.Lock()
	st.Running = false
	st.LastDuration = time.Since(start).Truncate(time.Second).String()
	if err != nil {
		st.LastExitCode = -1
		st.LastError = err.Error()
		slog.Error("job failed", "name", job.Name, "error", err, "duration", time.Since(start))
	} else {
		st.LastExitCode = result.ExitCode
		if result.ExitCode != 0 {
			slog.Warn("job exited non-zero", "name", job.Name, "exit_code", result.ExitCode, "duration", result.Duration)
		} else {
			slog.Info("job completed", "name", job.Name, "duration", result.Duration)
		}
	}
	s.mu.Unlock()

	jobLock.Unlock()
	locked = false

	if s.callback != nil {
		s.callback(job.Name, result, err)
	}
	if completedSuccessfully {
		s.triggerRunAfterSuccess(job)
	}
}

func (s *Scheduler) triggerRunAfterSuccess(job JobConfig) {
	for _, nextName := range job.RunAfterSuccess {
		if err := s.RunNow(nextName); err != nil {
			slog.Error("run_after_success trigger failed", "name", job.Name, "next", nextName, "error", err)
			continue
		}
		slog.Info("job follow-up triggered", "name", job.Name, "next", nextName)
	}
}

func jobTimeout(job JobConfig) (time.Duration, error) {
	timeoutText := strings.TrimSpace(job.Timeout)
	if timeoutText == "" {
		return defaultJobTimeout, nil
	}
	timeout, err := time.ParseDuration(timeoutText)
	if err != nil || timeout <= 0 {
		return 0, fmt.Errorf("job %q has invalid timeout %q", job.Name, job.Timeout)
	}
	return timeout, nil
}
