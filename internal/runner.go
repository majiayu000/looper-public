package internal

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type RunResult struct {
	Output   string
	ExitCode int
	Duration time.Duration
}

type Runner struct {
	logDir  string
	skills  *SkillManager
	mu      sync.RWMutex
	engines map[string]EngineConfig
}

func NewRunner(logDir string, skills *SkillManager, engines map[string]EngineConfig) *Runner {
	os.MkdirAll(logDir, 0o755)
	r := &Runner{logDir: logDir, skills: skills}
	r.UpdateEngines(engines)
	return r
}

func (r *Runner) UpdateEngines(engines map[string]EngineConfig) {
	cp := make(map[string]EngineConfig, len(engines))
	for k, v := range engines {
		cp[k] = v
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.engines = cp
}

func (r *Runner) EnginesSnapshot() map[string]EngineConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cp := make(map[string]EngineConfig, len(r.engines))
	for k, v := range r.engines {
		cp[k] = v
	}
	return cp
}

func (r *Runner) engine(name string) (EngineConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.engines[name]
	return e, ok
}

// LogPath returns the log file path for a job.
func (r *Runner) LogPath(jobName string) string {
	return filepath.Join(r.logDir, jobName+".log")
}

func (r *Runner) Run(ctx context.Context, job JobConfig) (*RunResult, error) {
	command, err := r.buildCommand(job)
	if err != nil {
		return &RunResult{
			Output:   err.Error(),
			ExitCode: 1,
			Duration: 0,
		}, nil
	}

	if job.Type == "skill" && r.skills != nil {
		if err := r.skills.ValidateSkill(job.Skill); err != nil {
			return &RunResult{
				Output:   fmt.Sprintf("skill validation failed: %v", err),
				ExitCode: 1,
				Duration: 0,
			}, nil
		}
	}

	start := time.Now()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = job.Workdir
	cmd.Env = scrubChildEnviron(os.Environ())
	prepareCommand(cmd)
	cmd.Cancel = func() error {
		return terminateCommand(cmd)
	}
	cmd.WaitDelay = 2 * time.Second

	logPath := r.LogPath(job.Name)
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("create log file %s: %w", logPath, err)
	}
	defer logFile.Close()

	// Tee stdout+stderr to both log file and memory
	cmd.Stdout = io.MultiWriter(logFile)
	cmd.Stderr = io.MultiWriter(logFile)

	err = cmd.Run()
	duration := time.Since(start)

	if ctx.Err() != nil {
		return nil, fmt.Errorf("command timed out after %v: %w", duration, ctx.Err())
	}

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("exec command: %w", err)
		}
	}

	// Tail last 64KB to avoid memory issues with long-running jobs
	const maxOutput = 64 * 1024
	output, _ := os.ReadFile(logPath)
	if len(output) > maxOutput {
		output = output[len(output)-maxOutput:]
	}

	return &RunResult{
		Output:   string(output),
		ExitCode: exitCode,
		Duration: duration,
	}, nil
}

func (r *Runner) buildCommand(job JobConfig) (string, error) {
	switch job.Type {
	case "script":
		if strings.TrimSpace(job.Command) == "" {
			return "", fmt.Errorf("script job %q has empty command", job.Name)
		}
		return job.Command, nil
	case "skill":
		engine, ok := r.engine(job.Engine)
		if !ok {
			return "", fmt.Errorf("skill job %q references unknown engine %q", job.Name, job.Engine)
		}
		return r.buildSkillCommand(job, engine)
	default:
		return "", fmt.Errorf("job %q has unsupported type %q", job.Name, job.Type)
	}
}

func (r *Runner) buildSkillCommand(job JobConfig, engine EngineConfig) (string, error) {
	skillPath := "/" + strings.TrimPrefix(job.Skill, "/")

	switch engine.Kind {
	case "claude":
		model := firstNonEmpty(job.Model, engine.DefaultModel)
		cli := engine.CLI
		parts := []string{shellQuote(cli), "-p", shellQuote(skillPath), "--dangerously-skip-permissions"}
		if model != "" {
			parts = append(parts, "--model", shellQuote(model))
		}
		parts = append(parts, "--output-format", "stream-json", "--verbose")
		if len(job.AllowedTools) > 0 {
			parts = append(parts, "--allowedTools", shellQuote(strings.Join(job.AllowedTools, ",")))
		}
		if job.PermissionMode != "" {
			parts = append(parts, "--permission-mode", shellQuote(job.PermissionMode))
		}
		return strings.Join(parts, " "), nil

	case "codex":
		if r.skills == nil {
			return "", fmt.Errorf("codex skill job %q requires skill manager", job.Name)
		}
		skillBody, err := r.skills.LoadSkillContent(job.Skill)
		if err != nil {
			return "", fmt.Errorf("load codex skill %q: %w", job.Skill, err)
		}

		prompt := buildCodexSkillPrompt(job.Skill, skillBody)
		model := firstNonEmpty(job.Model, engine.DefaultModel)
		sandbox := firstNonEmpty(job.Sandbox, engine.DefaultSandbox)
		reasoning := firstNonEmpty(job.ReasoningEffort, engine.DefaultReasoningEffort)

		parts := []string{shellQuote(engine.CLI), "exec"}
		if engine.SkipGitRepoCheck {
			parts = append(parts, "--skip-git-repo-check")
		}
		if engine.DangerouslyBypass {
			parts = append(parts, "--dangerously-bypass-approvals-and-sandbox")
		} else if sandbox != "" {
			parts = append(parts, "--sandbox", shellQuote(sandbox))
		}
		if model != "" {
			parts = append(parts, "-m", shellQuote(model))
		}
		if reasoning != "" {
			cfg := fmt.Sprintf(`model_reasoning_effort="%s"`, reasoning)
			parts = append(parts, "-c", shellQuote(cfg))
		}
		if engine.Search {
			parts = append(parts, "--search")
		}
		parts = append(parts, "--json", "-")
		delimiter := "__LOOPER_CODEX_SKILL_PROMPT__"
		return "cat <<'" + delimiter + "' | " + strings.Join(parts, " ") + "\n" + prompt + "\n" + delimiter, nil

	default:
		return "", fmt.Errorf("unsupported engine kind %q for job %q", engine.Kind, job.Name)
	}
}

func buildCodexSkillPrompt(skillName, skillBody string) string {
	return fmt.Sprintf(
		"You are executing Looper scheduled skill %q.\n"+
			"Follow the SKILL SPECIFICATION below as strict operating instructions and execute it now.\n"+
			"Do real work with tools; do not only summarize.\n"+
			"If required external dependencies are unavailable, report a concrete blocker and stop.\n\n"+
			"--- SKILL SPECIFICATION BEGIN ---\n%s\n--- SKILL SPECIFICATION END ---",
		skillName,
		skillBody,
	)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// scrubChildEnviron returns env without LOOPER_AUTH_TOKEN so job processes
// cannot read or log the observer command-triggering credential.
func scrubChildEnviron(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, "LOOPER_AUTH_TOKEN=") {
			continue
		}
		out = append(out, e)
	}
	return out
}
