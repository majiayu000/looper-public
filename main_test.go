package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"looper/internal"
)

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
