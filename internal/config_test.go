package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	cfg, err := LoadConfig("../testdata/workflow_test.yaml")
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	// platforms
	if len(cfg.Platforms) != 1 {
		t.Fatalf("expected 1 platform, got %d", len(cfg.Platforms))
	}
	p, ok := cfg.Platforms["test_platform"]
	if !ok {
		t.Fatal("test_platform not found")
	}
	if !p.Enabled {
		t.Error("expected enabled=true")
	}
	if p.DB != "testdata/test.db" {
		t.Errorf("expected db=testdata/test.db, got %s", p.DB)
	}
	if p.Metrics.CandidateBacklogTable != "test_candidate_backlog" {
		t.Errorf("expected candidate_backlog_table=test_candidate_backlog, got %s", p.Metrics.CandidateBacklogTable)
	}

	// engines
	if len(cfg.Engines) != 2 {
		t.Fatalf("expected 2 engines, got %d", len(cfg.Engines))
	}
	if cfg.Engines["claude"].Kind != "claude" {
		t.Errorf("expected claude engine kind=claude, got %s", cfg.Engines["claude"].Kind)
	}

	// scheduling
	if len(cfg.Scheduling.Jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(cfg.Scheduling.Jobs))
	}
	j := cfg.Scheduling.Jobs[0]
	if j.Name != "test_scan" {
		t.Errorf("expected name=test_scan, got %s", j.Name)
	}
	if j.Schedule != "@every 1m" {
		t.Errorf("expected schedule=@every 1m, got %s", j.Schedule)
	}
	if j.Type != "script" {
		t.Errorf("expected type=script, got %s", j.Type)
	}

	skillJob := cfg.Scheduling.Jobs[1]
	if skillJob.Type != "skill" {
		t.Errorf("expected second job type=skill, got %s", skillJob.Type)
	}
	if skillJob.Engine != "claude" {
		t.Errorf("expected skill engine=claude, got %s", skillJob.Engine)
	}
	if skillJob.Skill != "test-skill" {
		t.Errorf("expected skill=test-skill, got %s", skillJob.Skill)
	}
}

func TestLoadConfigMissing(t *testing.T) {
	_, err := LoadConfig("nonexistent.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadRealWorkflow(t *testing.T) {
	cfg, err := LoadConfig("../WORKFLOW.md")
	if err != nil {
		t.Fatalf("LoadConfig public WORKFLOW.md failed: %v", err)
	}

	platform, ok := cfg.Platforms["demo"]
	if !ok {
		t.Fatal("missing demo platform")
	}
	if platform.Enabled {
		t.Fatal("demo platform should be disabled in the public example")
	}

	requiredEngines := map[string]string{
		"claude": "claude",
		"codex":  "codex",
	}
	for name, kind := range requiredEngines {
		eng, ok := cfg.Engines[name]
		if !ok {
			t.Fatalf("missing required engine %q", name)
		}
		if eng.Kind != kind {
			t.Fatalf("engine %q kind mismatch: got %q want %q", name, eng.Kind, kind)
		}
		if strings.TrimSpace(eng.SkillsDir) == "" {
			t.Fatalf("engine %q must configure skills_dir", name)
		}
	}

	for name, eng := range cfg.Engines {
		if eng.DangerouslyBypass {
			t.Fatalf("public example engine %q must not bypass approvals or sandbox", name)
		}
	}

	if len(cfg.Scheduling.Jobs) != 1 {
		t.Fatalf("expected one public example job, got %d", len(cfg.Scheduling.Jobs))
	}
	job := cfg.Scheduling.Jobs[0]
	if job.Name != "demo_noop" {
		t.Fatalf("example job name = %q, want demo_noop", job.Name)
	}
	if job.Type != "script" {
		t.Fatalf("example job type = %q, want script", job.Type)
	}
	if strings.TrimSpace(job.Command) == "" {
		t.Fatal("example script job must set command")
	}
}

func TestLoadConfigRejectsUnknownRunAfterSuccessTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-run-after.yaml")
	content := `engines:
  claude:
    kind: claude
    cli: claude
    skills_dir: /tmp/skills
scheduling:
  jobs:
    - name: parent
      schedule: "@every 1m"
      command: "echo ok"
      workdir: .
      type: script
      run_after_success:
        - missing
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected config validation error for unknown run_after_success target")
	}
	if !strings.Contains(err.Error(), "run_after_success references unknown job") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigRejectsRunAfterSuccessCycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-run-after-cycle.yaml")
	content := `engines:
  claude:
    kind: claude
    cli: claude
    skills_dir: /tmp/skills
scheduling:
  jobs:
    - name: job_a
      schedule: "@every 1m"
      command: "echo a"
      workdir: .
      type: script
      run_after_success:
        - job_b
    - name: job_b
      schedule: "@every 1m"
      command: "echo b"
      workdir: .
      type: script
      run_after_success:
        - job_a
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected config validation error for run_after_success cycle")
	}
	if !strings.Contains(err.Error(), "run_after_success cycle") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigRejectsSkillCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	content := `engines:
  claude:
    kind: claude
    cli: claude
    skills_dir: /tmp/skills
scheduling:
  jobs:
    - name: bad_skill
      schedule: "@every 1m"
      workdir: .
      type: skill
      engine: claude
      skill: x-reply
      command: "claude -p '/x-reply'"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected config validation error for skill command compatibility mode")
	}
	if !strings.Contains(err.Error(), "must not set command") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigRejectsInvalidTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-timeout.yaml")
	content := `engines:
  claude:
    kind: claude
    cli: claude
    skills_dir: /tmp/skills
scheduling:
  jobs:
    - name: bad_timeout
      schedule: "@every 1m"
      command: "echo ok"
      workdir: .
      type: script
      timeout: nope
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected config validation error for invalid timeout")
	}
	if !strings.Contains(err.Error(), "invalid timeout") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigRejectsDuplicateJobName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dup.yaml")
	content := `engines:
  claude:
    kind: claude
    cli: claude
    skills_dir: /tmp/skills
scheduling:
  jobs:
    - name: same
      schedule: "@every 1m"
      workdir: .
      type: script
      command: "echo 1"
    - name: same
      schedule: "@every 2m"
      workdir: .
      type: script
      command: "echo 2"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected duplicate job validation error")
	}
	if !strings.Contains(err.Error(), "duplicate job name") {
		t.Fatalf("unexpected error: %v", err)
	}
}
