package internal

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Platforms  map[string]PlatformConfig `yaml:"platforms"`
	Engines    map[string]EngineConfig   `yaml:"engines"`
	Scheduling SchedulingConfig          `yaml:"scheduling"`
	Learning   LearningConfig            `yaml:"learning"`
	Pricing    map[string]PricingConfig  `yaml:"pricing"`
}

type EngineConfig struct {
	Kind                   string `yaml:"kind"`                               // claude | codex
	CLI                    string `yaml:"cli"`                                // executable, e.g. claude / codex
	SkillsDir              string `yaml:"skills_dir"`                         // global skills dir used by the engine
	DefaultModel           string `yaml:"default_model,omitempty"`            // optional default model for skill jobs
	DefaultSandbox         string `yaml:"default_sandbox,omitempty"`          // codex only
	DefaultReasoningEffort string `yaml:"default_reasoning_effort,omitempty"` // codex only
	SkipGitRepoCheck       bool   `yaml:"skip_git_repo_check,omitempty"`      // codex only
	DangerouslyBypass      bool   `yaml:"dangerously_bypass_approvals_and_sandbox,omitempty"`
	Search                 bool   `yaml:"search,omitempty"` // codex only
}

type PlatformConfig struct {
	Enabled bool          `yaml:"enabled"`
	CLI     string        `yaml:"cli"`
	DB      string        `yaml:"db"`
	Metrics MetricsConfig `yaml:"metrics"`
}

type MetricsConfig struct {
	CandidateBacklogTable       string `yaml:"candidate_backlog_table"`
	CandidateBacklogStatusField string `yaml:"candidate_backlog_status_field"`
	CandidateBacklogPendingVal  string `yaml:"candidate_backlog_pending_value"`
	ActionTable                 string `yaml:"action_table"`
	ActionTypeField             string `yaml:"action_type_field"`
	ActionTimeField             string `yaml:"action_time_field"`
	PublishedItemsTable         string `yaml:"published_items_table"`
}

type SchedulingConfig struct {
	Jobs []JobConfig `yaml:"jobs"`
}

type JobConfig struct {
	Name            string   `yaml:"name"`
	Schedule        string   `yaml:"schedule"`
	Command         string   `yaml:"command,omitempty"` // required for script jobs
	Workdir         string   `yaml:"workdir"`
	LockKey         string   `yaml:"lock_key,omitempty"`         // optional scheduler mutex key; defaults to workdir
	Type            string   `yaml:"type"`                       // script | skill
	Timeout         string   `yaml:"timeout,omitempty"`          // Go duration, default 30m
	Engine          string   `yaml:"engine,omitempty"`           // required for skill jobs
	Skill           string   `yaml:"skill,omitempty"`            // required for skill jobs
	Model           string   `yaml:"model,omitempty"`            // overrides engine default model
	ReasoningEffort string   `yaml:"reasoning_effort,omitempty"` // codex only
	Sandbox         string   `yaml:"sandbox,omitempty"`          // codex only
	AllowedTools    []string `yaml:"allowed_tools,omitempty"`    // claude only
	PermissionMode  string   `yaml:"permission_mode,omitempty"`  // claude only: default | acceptEdits | plan | bypassPermissions
	RunAfterSuccess []string `yaml:"run_after_success,omitempty"`
}

type PricingConfig struct {
	Input       float64 `yaml:"input"`
	Output      float64 `yaml:"output"`
	CacheCreate float64 `yaml:"cache_create"`
	CacheRead   float64 `yaml:"cache_read"`
}

type LearningConfig struct {
	Enabled    bool            `yaml:"enabled"`
	Guardrails GuardrailConfig `yaml:"guardrails"`
}

type GuardrailConfig struct {
	MaxWeight      float64 `yaml:"max_weight"`
	MinWeight      float64 `yaml:"min_weight"`
	MaxDailyChange float64 `yaml:"max_daily_change"`
	MinSamples     int     `yaml:"min_samples"`
}

// yamlFrontMatter extracts YAML between --- delimiters from a markdown file.
var yamlFrontMatter = regexp.MustCompile(`(?s)^---\n(.+?)\n---`)

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	// Support both plain YAML and markdown with YAML front-matter
	yamlData := data
	if m := yamlFrontMatter.FindSubmatch(data); m != nil {
		yamlData = m[1]
	}

	var cfg Config
	if err := yaml.Unmarshal(yamlData, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	if len(c.Engines) == 0 {
		return fmt.Errorf("engines is required")
	}

	for name, e := range c.Engines {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("engine name cannot be empty")
		}
		kind := strings.TrimSpace(e.Kind)
		switch kind {
		case "claude", "codex":
		default:
			return fmt.Errorf("engine %q has unsupported kind %q", name, e.Kind)
		}
		if strings.TrimSpace(e.CLI) == "" {
			return fmt.Errorf("engine %q must set cli", name)
		}
		if strings.TrimSpace(e.SkillsDir) == "" {
			return fmt.Errorf("engine %q must set skills_dir", name)
		}
	}

	seenJobs := make(map[string]bool, len(c.Scheduling.Jobs))
	for _, j := range c.Scheduling.Jobs {
		name := strings.TrimSpace(j.Name)
		if name == "" {
			return fmt.Errorf("job name is required")
		}
		if seenJobs[name] {
			return fmt.Errorf("duplicate job name %q", j.Name)
		}
		seenJobs[name] = true
	}

	for _, j := range c.Scheduling.Jobs {
		jobName := strings.TrimSpace(j.Name)
		if strings.TrimSpace(j.Schedule) == "" {
			return fmt.Errorf("job %q must set schedule", j.Name)
		}
		if strings.TrimSpace(j.Workdir) == "" {
			return fmt.Errorf("job %q must set workdir", j.Name)
		}
		if timeout := strings.TrimSpace(j.Timeout); timeout != "" {
			d, err := time.ParseDuration(timeout)
			if err != nil || d <= 0 {
				return fmt.Errorf("job %q has invalid timeout %q", j.Name, j.Timeout)
			}
		}

		switch j.Type {
		case "script":
			if strings.TrimSpace(j.Command) == "" {
				return fmt.Errorf("script job %q must set command", j.Name)
			}
		case "skill":
			if strings.TrimSpace(j.Engine) == "" {
				return fmt.Errorf("skill job %q must set engine", j.Name)
			}
			if _, ok := c.Engines[j.Engine]; !ok {
				return fmt.Errorf("skill job %q references unknown engine %q", j.Name, j.Engine)
			}
			if strings.TrimSpace(j.Skill) == "" {
				return fmt.Errorf("skill job %q must set skill", j.Name)
			}
			// No backward compatibility: skill jobs cannot provide raw command.
			if strings.TrimSpace(j.Command) != "" {
				return fmt.Errorf("skill job %q must not set command; use engine/skill fields", j.Name)
			}
		default:
			return fmt.Errorf("job %q has unsupported type %q", j.Name, j.Type)
		}

		seenFollowUps := make(map[string]bool, len(j.RunAfterSuccess))
		for _, next := range j.RunAfterSuccess {
			next = strings.TrimSpace(next)
			if next == "" {
				return fmt.Errorf("job %q has empty run_after_success target", j.Name)
			}
			if next == jobName {
				return fmt.Errorf("job %q cannot run itself after success", j.Name)
			}
			if !seenJobs[next] {
				return fmt.Errorf("job %q run_after_success references unknown job %q", j.Name, next)
			}
			if seenFollowUps[next] {
				return fmt.Errorf("job %q has duplicate run_after_success target %q", j.Name, next)
			}
			seenFollowUps[next] = true
		}
	}
	if err := validateRunAfterSuccessCycles(c.Scheduling.Jobs); err != nil {
		return err
	}

	return nil
}

func validateRunAfterSuccessCycles(jobs []JobConfig) error {
	graph := make(map[string][]string, len(jobs))
	for _, job := range jobs {
		graph[strings.TrimSpace(job.Name)] = append([]string(nil), job.RunAfterSuccess...)
	}

	visiting := make(map[string]bool, len(jobs))
	visited := make(map[string]bool, len(jobs))
	var stack []string
	var visit func(string) error
	visit = func(name string) error {
		if visited[name] {
			return nil
		}
		if visiting[name] {
			cycle := append(stack, name)
			return fmt.Errorf("run_after_success cycle: %s", strings.Join(cycle, " -> "))
		}
		visiting[name] = true
		stack = append(stack, name)
		for _, next := range graph[name] {
			if err := visit(next); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		visiting[name] = false
		visited[name] = true
		return nil
	}

	for name := range graph {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}
