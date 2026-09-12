// main.go
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"looper/internal"
)

func main() {
	var (
		configPath string
		port       int
	)
	flag.StringVar(&configPath, "config", "WORKFLOW.md", "path to WORKFLOW.md config")
	flag.IntVar(&port, "port", 5567, "HTTP observer port")
	flag.Parse()

	// Resolve config path relative to binary location
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		slog.Error("resolve config path", "error", err)
		os.Exit(1)
	}

	// Load config
	cfg, err := internal.LoadConfig(absConfig)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	slog.Info("config loaded",
		"platforms", len(cfg.Platforms),
		"jobs", len(cfg.Scheduling.Jobs),
	)
	warnIfLearningEnabled(cfg)

	// Resolve workdir paths relative to config file directory
	configDir := filepath.Dir(absConfig)
	resolveRelativePaths(cfg, configDir)

	// Skill manager: sync symlinks + generate lock
	skillsDir := filepath.Join(filepath.Dir(absConfig), "skills")
	var skills *internal.SkillManager
	if info, err := os.Stat(skillsDir); err == nil && info.IsDir() {
		skills = internal.NewSkillManager(skillsDir, collectGlobalSkillDirs(cfg))
		if n, err := skills.EnsureSymlinks(); err != nil {
			slog.Warn("skill symlink sync failed", "error", err)
		} else if n > 0 {
			slog.Info("skill symlinks updated", "count", n)
		}
		if err := skills.GenerateLock(); err != nil {
			slog.Warn("skill lock generation failed", "error", err)
		}
	}

	startTime := time.Now()
	runner := internal.NewRunner(filepath.Join(filepath.Dir(absConfig), "logs"), skills, cfg.Engines)
	currentCfg := cfg

	// Scheduler
	scheduler := internal.NewScheduler(runner, func(name string, result *internal.RunResult, err error) {
		if err != nil {
			slog.Error("job callback", "name", name, "error", err)
		}
	})
	if err := scheduler.Load(cfg.Scheduling.Jobs); err != nil {
		slog.Error("load scheduler", "error", err)
		os.Exit(1)
	}
	scheduler.Start()
	slog.Info("scheduler started", "jobs", len(cfg.Scheduling.Jobs))

	// Observer
	observer := internal.NewObserver(cfg, scheduler, runner, startTime)
	go func() {
		addr := fmt.Sprintf(":%d", port)
		slog.Info("observer listening", "addr", addr)
		if err := http.ListenAndServe(addr, observer.Handler()); err != nil {
			slog.Error("observer http", "error", err)
		}
	}()

	// File watcher for hot-reload
	watcher, err := internal.NewWatcher(absConfig, func() {
		newCfg, err := internal.LoadConfig(absConfig)
		if err != nil {
			slog.Error("reload config", "error", err)
			return
		}
		resolveRelativePaths(newCfg, configDir)
		if err := applyConfigReload(newCfg, &currentCfg, scheduler, runner, skills, observer); err != nil {
			slog.Error("config reload failed", "error", err)
			return
		}
		warnIfLearningEnabled(currentCfg)
		slog.Info("config reloaded",
			"platforms", len(currentCfg.Platforms),
			"jobs", len(currentCfg.Scheduling.Jobs),
		)
	})
	if err != nil {
		slog.Error("init watcher", "error", err)
		os.Exit(1)
	}
	defer watcher.Close()
	go watcher.Watch()

	// Wait for signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	slog.Info("shutting down", "signal", s)

	scheduler.Stop()
	slog.Info("goodbye")
}

// warnIfLearningEnabled notices operators that learning.enabled has no runtime
// effect. LearningConfig is kept for YAML compatibility only; no ranking or
// weight updates are wired into scheduler, runner, or observer.
func warnIfLearningEnabled(cfg *internal.Config) {
	if cfg == nil || !cfg.Learning.Enabled {
		return
	}
	slog.Warn("learning.enabled is set but learning is reserved/unimplemented; no ranking or weight updates will run",
		"enabled", true,
		"max_weight", cfg.Learning.Guardrails.MaxWeight,
		"min_weight", cfg.Learning.Guardrails.MinWeight,
		"max_daily_change", cfg.Learning.Guardrails.MaxDailyChange,
		"min_samples", cfg.Learning.Guardrails.MinSamples,
	)
}

func collectGlobalSkillDirs(cfg *internal.Config) []string {
	var dirs []string
	for _, eng := range cfg.Engines {
		if eng.SkillsDir != "" {
			dirs = append(dirs, eng.SkillsDir)
		}
	}
	return dirs
}

func resolveRelativePaths(cfg *internal.Config, baseDir string) {
	for i := range cfg.Scheduling.Jobs {
		job := &cfg.Scheduling.Jobs[i]
		job.Workdir = resolveConfigPath(job.Workdir, baseDir)
	}
	for name, p := range cfg.Platforms {
		p.DB = resolveConfigPath(p.DB, baseDir)
		cfg.Platforms[name] = p
	}
	for name, e := range cfg.Engines {
		e.SkillsDir = resolveConfigPath(e.SkillsDir, baseDir)
		cfg.Engines[name] = e
	}
}

type reloadScheduler interface {
	Stop()
	Reload([]internal.JobConfig) error
}

type reloadRunner interface {
	UpdateEngines(map[string]internal.EngineConfig)
	EnginesSnapshot() map[string]internal.EngineConfig
}

type reloadSkills interface {
	SetGlobalDirs([]string)
	GlobalDirs() []string
	EnsureSymlinks() (int, error)
}

type reloadObserver interface {
	UpdateConfig(*internal.Config)
}

func applyConfigReload(
	newCfg *internal.Config,
	currentCfg **internal.Config,
	scheduler reloadScheduler,
	runner reloadRunner,
	skills reloadSkills,
	observer reloadObserver,
) error {
	if newCfg == nil {
		return fmt.Errorf("new config is nil")
	}
	if currentCfg == nil || *currentCfg == nil {
		return fmt.Errorf("current config is nil")
	}

	oldCfg := *currentCfg
	oldEngines := runner.EnginesSnapshot()
	var oldSkillDirs []string
	if skills != nil {
		oldSkillDirs = skills.GlobalDirs()
	}

	rollback := func(reason string, cause error) error {
		runner.UpdateEngines(oldEngines)
		if skills != nil {
			skills.SetGlobalDirs(oldSkillDirs)
			if _, err := skills.EnsureSymlinks(); err != nil {
				return fmt.Errorf("%s: %w (rollback symlink sync failed: %v)", reason, cause, err)
			}
		}
		if err := scheduler.Reload(oldCfg.Scheduling.Jobs); err != nil {
			return fmt.Errorf("%s: %w (rollback scheduler failed: %v)", reason, cause, err)
		}
		if observer != nil {
			observer.UpdateConfig(oldCfg)
		}
		slog.Warn("config reload rolled back")
		return fmt.Errorf("%s: %w", reason, cause)
	}

	scheduler.Stop()
	runner.UpdateEngines(newCfg.Engines)
	if skills != nil {
		skills.SetGlobalDirs(collectGlobalSkillDirs(newCfg))
		if n, err := skills.EnsureSymlinks(); err != nil {
			return rollback("reload skill symlink sync failed", err)
		} else if n > 0 {
			slog.Info("reload skill symlinks updated", "count", n)
		}
	}

	if err := scheduler.Reload(newCfg.Scheduling.Jobs); err != nil {
		return rollback("reload scheduler failed", err)
	}
	*currentCfg = newCfg
	if observer != nil {
		observer.UpdateConfig(*currentCfg)
	}
	return nil
}

func resolveConfigPath(path, baseDir string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = expandHome(path)
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(baseDir, path)
}

func expandHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	if strings.HasPrefix(path, `~\`) {
		return filepath.Join(home, path[2:])
	}
	return path
}
