package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"looper/internal"
)

func TestObserverListenAddrIsLoopback(t *testing.T) {
	listener, err := net.Listen("tcp", observerListenAddr(0))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	addr := listener.Addr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() {
		t.Fatalf("observer exposed on non-loopback address %s", addr)
	}
}

type fakeReloadObserver struct {
	cfg *internal.Config
}

func (f *fakeReloadObserver) UpdateConfig(cfg *internal.Config) {
	f.cfg = cfg
}

func TestResolveConfigPathExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("home directory unavailable")
	}

	got := resolveConfigPath("~/.codex/skills", "/tmp/base")
	want := filepath.Join(home, ".codex", "skills")
	if got != want {
		t.Fatalf("resolveConfigPath tilde expansion mismatch: got %q want %q", got, want)
	}
}

func TestResolveRelativePathsExpandsEngineSkillsDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("home directory unavailable")
	}

	cfg := &internal.Config{
		Platforms: map[string]internal.PlatformConfig{
			"demo": {DB: "data/tracker.db"},
		},
		Engines: map[string]internal.EngineConfig{
			"codex": {SkillsDir: "~/.codex/skills"},
		},
		Scheduling: internal.SchedulingConfig{
			Jobs: []internal.JobConfig{
				{Name: "job", Workdir: "../x", Type: "script", Command: "echo ok", Schedule: "@every 1m"},
			},
		},
	}

	baseDir := "/tmp/looper"
	resolveRelativePaths(cfg, baseDir)

	wantSkills := filepath.Join(home, ".codex", "skills")
	if got := cfg.Engines["codex"].SkillsDir; got != wantSkills {
		t.Fatalf("skills_dir not expanded: got %q want %q", got, wantSkills)
	}

	wantWorkdir := filepath.Join(baseDir, "../x")
	if got := cfg.Scheduling.Jobs[0].Workdir; got != wantWorkdir {
		t.Fatalf("workdir not resolved: got %q want %q", got, wantWorkdir)
	}

	wantDB := filepath.Join(baseDir, "data/tracker.db")
	if got := cfg.Platforms["demo"].DB; got != wantDB {
		t.Fatalf("db path not resolved: got %q want %q", got, wantDB)
	}
}

func TestApplyConfigReloadRollsBackOnSchedulerFailure(t *testing.T) {
	oldCfg := &internal.Config{
		Engines: map[string]internal.EngineConfig{
			"old_engine": {Kind: "claude", CLI: "claude", SkillsDir: "/tmp/old-skills"},
		},
		Scheduling: internal.SchedulingConfig{
			Jobs: []internal.JobConfig{
				{Name: "old_job", Schedule: "@every 1m", Workdir: ".", Type: "script", Command: "echo old"},
			},
		},
	}
	newCfg := &internal.Config{
		Engines: map[string]internal.EngineConfig{
			"new_engine": {Kind: "codex", CLI: "codex", SkillsDir: "/tmp/new-skills"},
		},
		Scheduling: internal.SchedulingConfig{
			Jobs: []internal.JobConfig{
				{Name: "bad_job", Schedule: "not a cron", Workdir: ".", Type: "script", Command: "echo bad"},
			},
		},
	}

	runner := internal.NewRunner(t.TempDir(), nil, oldCfg.Engines)
	scheduler := internal.NewScheduler(runner, nil)
	if err := scheduler.Load(oldCfg.Scheduling.Jobs); err != nil {
		t.Fatalf("load old jobs failed: %v", err)
	}

	current := oldCfg
	observer := &fakeReloadObserver{cfg: oldCfg}
	err := applyConfigReload(newCfg, &current, scheduler, runner, nil, observer)
	if err == nil {
		t.Fatal("expected reload failure")
	}

	if current != oldCfg {
		t.Fatalf("current config should roll back to old config")
	}
	if observer.cfg != oldCfg {
		t.Fatalf("observer config should roll back to old config")
	}

	engines := runner.EnginesSnapshot()
	if len(engines) != 1 || engines["old_engine"].CLI != "claude" {
		t.Fatalf("runner engines not rolled back: %+v", engines)
	}

	names := scheduler.JobNames()
	sort.Strings(names)
	if len(names) != 1 || names[0] != "old_job" {
		t.Fatalf("scheduler jobs not rolled back: %v", names)
	}
}

func TestApplyConfigReloadSuccess(t *testing.T) {
	oldCfg := &internal.Config{
		Engines: map[string]internal.EngineConfig{
			"old_engine": {Kind: "claude", CLI: "claude", SkillsDir: "/tmp/old-skills"},
		},
		Scheduling: internal.SchedulingConfig{
			Jobs: []internal.JobConfig{
				{Name: "old_job", Schedule: "@every 1m", Workdir: ".", Type: "script", Command: "echo old"},
			},
		},
	}
	newCfg := &internal.Config{
		Engines: map[string]internal.EngineConfig{
			"new_engine": {Kind: "codex", CLI: "codex", SkillsDir: "/tmp/new-skills"},
		},
		Scheduling: internal.SchedulingConfig{
			Jobs: []internal.JobConfig{
				{Name: "new_job", Schedule: "@every 2m", Workdir: ".", Type: "script", Command: "echo new"},
			},
		},
	}

	runner := internal.NewRunner(t.TempDir(), nil, oldCfg.Engines)
	scheduler := internal.NewScheduler(runner, nil)
	if err := scheduler.Load(oldCfg.Scheduling.Jobs); err != nil {
		t.Fatalf("load old jobs failed: %v", err)
	}

	current := oldCfg
	observer := &fakeReloadObserver{cfg: oldCfg}
	if err := applyConfigReload(newCfg, &current, scheduler, runner, nil, observer); err != nil {
		t.Fatalf("reload should succeed: %v", err)
	}

	if current != newCfg {
		t.Fatalf("current config should switch to new config")
	}
	if observer.cfg != newCfg {
		t.Fatalf("observer config should switch to new config")
	}

	engines := runner.EnginesSnapshot()
	if len(engines) != 1 || engines["new_engine"].CLI != "codex" {
		t.Fatalf("runner engines not updated: %+v", engines)
	}

	names := scheduler.JobNames()
	sort.Strings(names)
	if len(names) != 1 || names[0] != "new_job" {
		t.Fatalf("scheduler jobs not updated: %v", names)
	}
}

// Run the same CLI in a child process so startup and the real watcher share the
// production activation boundaries, including when this test runs with -race.
func TestLearningWarningAfterActivation(t *testing.T) {
	if config := os.Getenv("LOOPER_TEST_LEARNING_CONFIG"); config != "" {
		os.Args = []string{"looper", "-config", config, "-port", os.Getenv("LOOPER_TEST_LEARNING_PORT")}
		main()
		os.Exit(0)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const warning = "learning is not implemented; enabled and guardrails have no effect"
	for _, format := range []string{"yaml", "markdown"} {
		for _, initial := range []string{"enabled", "disabled", "omitted", "bad_schedule"} {
			t.Run(format+"/"+initial, func(t *testing.T) {
				dir := t.TempDir()
				if err := os.Mkdir(filepath.Join(dir, "skills"), 0o755); err != nil {
					t.Fatal(err)
				}
				config := filepath.Join(dir, "workflow")
				writeConfig := func(state string) {
					t.Helper()
					schedule := "@every 1h"
					if state == "bad_schedule" {
						schedule = "not a cron"
					}
					content := fmt.Sprintf("engines:\n  fixture:\n    kind: claude\n    cli: echo\n    skills_dir: ./skills\nscheduling:\n  jobs:\n    - name: %s\n      schedule: %q\n      type: script\n      command: echo ok\n      workdir: .\n", state, schedule)
					if state != "omitted" {
						content += fmt.Sprintf("learning:\n  enabled: %t\n", state != "disabled")
					}
					if format == "markdown" {
						content = "---\n" + content + "---\n# Workflow\n"
					}
					if err := os.WriteFile(config, []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				writeConfig(initial)
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				port := listener.Addr().(*net.TCPAddr).Port
				if err := listener.Close(); err != nil {
					t.Fatal(err)
				}
				logPath := filepath.Join(dir, "process.log")
				log, err := os.Create(logPath)
				if err != nil {
					t.Fatal(err)
				}
				defer log.Close()
				cmd := exec.Command(binary, "-test.run=^TestLearningWarningAfterActivation$")
				cmd.Env = append(os.Environ(), "LOOPER_TEST_LEARNING_CONFIG="+config, fmt.Sprintf("LOOPER_TEST_LEARNING_PORT=%d", port))
				cmd.Stdout, cmd.Stderr = log, log
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				reaped := false
				defer func() {
					if !reaped {
						_ = cmd.Process.Kill()
						<-done
					}
				}()
				logs := func() string {
					t.Helper()
					data, err := os.ReadFile(logPath)
					if err != nil {
						t.Fatal(err)
					}
					return string(data)
				}
				waitFor := func(predicate func() bool) {
					t.Helper()
					deadline := time.Now().Add(8 * time.Second)
					for time.Now().Before(deadline) {
						if predicate() {
							return
						}
						select {
						case err := <-done:
							reaped = true
							t.Fatalf("CLI exited: %v\n%s", err, logs())
						default:
						}
						time.Sleep(10 * time.Millisecond)
					}
					t.Fatalf("timed out waiting for CLI\n%s", logs())
				}
				if initial == "bad_schedule" {
					select {
					case err := <-done:
						reaped = true
						if err == nil || !strings.Contains(logs(), "load scheduler") || strings.Contains(logs(), warning) {
							t.Fatalf("failed startup warned or lost its error: %v\n%s", err, logs())
						}
					case <-time.After(8 * time.Second):
						t.Fatal("invalid schedule did not stop startup")
					}
					return
				}
				client := &http.Client{Timeout: 200 * time.Millisecond}
				get := func(path string) string {
					response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
					if err != nil {
						return ""
					}
					defer response.Body.Close()
					data, err := io.ReadAll(response.Body)
					if err != nil || response.StatusCode != http.StatusOK {
						return ""
					}
					return string(data)
				}
				waitFor(func() bool { return strings.Contains(get("/health"), "ok") })
				expected := 0
				if initial == "enabled" {
					expected = 1
				}
				if got := strings.Count(logs(), warning); got != expected {
					t.Fatalf("startup warnings = %d, want %d\n%s", got, expected, logs())
				}
				active := initial
				for _, state := range []string{"bad_schedule", "enabled", "disabled", "omitted"} {
					// Respect the existing file watcher's 500ms debounce.
					time.Sleep(600 * time.Millisecond)
					before := logs()
					writeConfig(state)
					message := "config reloaded"
					if state == "bad_schedule" {
						message = "config reload failed"
					}
					waitFor(func() bool { return strings.Count(logs(), message) > strings.Count(before, message) })
					if state == "bad_schedule" {
						if !strings.Contains(logs(), "config reload rolled back") {
							t.Fatalf("missing rollback error\n%s", logs())
						}
					} else {
						active = state
						if state == "enabled" {
							expected++
						}
					}
					if got := strings.Count(logs(), warning); got != expected {
						t.Fatalf("%s warnings = %d, want %d\n%s", state, got, expected, logs())
					}
					if !strings.Contains(get("/api/jobs"), fmt.Sprintf(`"name":%q`, active)) || get("/health") == "" {
						t.Fatalf("%s did not preserve active jobs and health", state)
					}
				}
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					reaped = true
					if err != nil || !strings.Contains(logs(), "goodbye") {
						t.Fatalf("shutdown failed: %v\n%s", err, logs())
					}
				case <-time.After(8 * time.Second):
					t.Fatal("CLI did not exit after SIGTERM")
				}
			})
		}
	}
}
