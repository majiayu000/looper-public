package internal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunCommandSuccess(t *testing.T) {
	r := NewRunner(t.TempDir(), nil, nil)
	result, err := r.Run(context.Background(), JobConfig{
		Name:    "test_echo",
		Type:    "script",
		Command: "echo hello",
		Workdir: ".",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if !strings.Contains(result.Output, "hello") {
		t.Errorf("expected output containing 'hello', got %q", result.Output)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
	if result.Duration <= 0 {
		t.Error("expected positive duration")
	}
}

func TestRunCommandFailure(t *testing.T) {
	r := NewRunner(t.TempDir(), nil, nil)
	result, err := r.Run(context.Background(), JobConfig{
		Name:    "test_fail",
		Type:    "script",
		Command: "false",
		Workdir: ".",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
}

func TestRunCommandTimeout(t *testing.T) {
	r := NewRunner(t.TempDir(), nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := r.Run(ctx, JobConfig{
		Name:    "test_timeout",
		Type:    "script",
		Command: "sleep 10",
		Workdir: ".",
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestRunCommandTimeoutKillsDescendant(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	marker := fmt.Sprintf("looper-runner-orphan-test-%d", time.Now().UnixNano())
	r := NewRunner(t.TempDir(), nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := r.Run(ctx, JobConfig{
		Name:    "test_timeout_descendant",
		Type:    "script",
		Command: fmt.Sprintf("python3 -c 'import time; time.sleep(30)' %s & wait", marker),
		Workdir: ".",
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processWithMarkerExists(t, marker) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("descendant process with marker %q still exists after timeout", marker)
}

func processWithMarkerExists(t *testing.T, marker string) bool {
	t.Helper()
	out, err := exec.Command("ps", "-axo", "command=").Output()
	if err != nil {
		t.Fatalf("ps failed: %v", err)
	}
	return strings.Contains(string(out), marker)
}

func TestBuildSkillCommandClaude(t *testing.T) {
	r := NewRunner(t.TempDir(), nil, nil)
	job := JobConfig{
		Name:         "demo_skill",
		Type:         "skill",
		Engine:       "claude",
		Skill:        "example-skill",
		Model:        "sonnet",
		AllowedTools: []string{"Bash", "Read"},
	}
	eng := EngineConfig{
		Kind: "claude",
		CLI:  "claude",
	}

	cmd, err := r.buildSkillCommand(job, eng)
	if err != nil {
		t.Fatalf("buildSkillCommand failed: %v", err)
	}
	if !strings.Contains(cmd, "claude") || !strings.Contains(cmd, "-p") || !strings.Contains(cmd, "/example-skill") {
		t.Fatalf("unexpected claude skill command: %s", cmd)
	}
	if !strings.Contains(cmd, "--output-format stream-json") || !strings.Contains(cmd, "--verbose") {
		t.Fatalf("expected stream-json/verbose flags in command: %s", cmd)
	}
	if !strings.Contains(cmd, "--allowedTools") {
		t.Fatalf("expected allowedTools in command: %s", cmd)
	}
}

func TestBuildSkillCommandCodex(t *testing.T) {
	skillsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(skillsDir, "example-skill"), 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "example-skill", "SKILL.md"), []byte("# test skill\n"), 0o644); err != nil {
		t.Fatalf("write skill file: %v", err)
	}
	r := NewRunner(t.TempDir(), NewSkillManager(skillsDir, []string{t.TempDir()}), nil)

	job := JobConfig{
		Name:            "demo_skill_codex",
		Type:            "skill",
		Engine:          "codex",
		Skill:           "example-skill",
		Model:           "gpt-5.4",
		ReasoningEffort: "low",
		Sandbox:         "read-only",
	}
	eng := EngineConfig{
		Kind:             "codex",
		CLI:              "codex",
		SkipGitRepoCheck: true,
	}

	cmd, err := r.buildSkillCommand(job, eng)
	if err != nil {
		t.Fatalf("buildSkillCommand failed: %v", err)
	}
	if !strings.Contains(cmd, "codex") || !strings.Contains(cmd, "exec") || !strings.Contains(cmd, "--json") {
		t.Fatalf("unexpected codex skill command: %s", cmd)
	}
	if !strings.Contains(cmd, "--skip-git-repo-check") {
		t.Fatalf("expected skip-git-repo-check in command: %s", cmd)
	}
	if !strings.Contains(cmd, "--sandbox 'read-only'") {
		t.Fatalf("expected sandbox override in command: %s", cmd)
	}
	if !strings.Contains(cmd, "model_reasoning_effort") || !strings.Contains(cmd, "SKILL SPECIFICATION") {
		t.Fatalf("expected reasoning effort override in command: %s", cmd)
	}
}

func TestBuildSkillCommandCodexBypassSkipsSandbox(t *testing.T) {
	skillsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(skillsDir, "example-skill"), 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "example-skill", "SKILL.md"), []byte("# test skill\n"), 0o644); err != nil {
		t.Fatalf("write skill file: %v", err)
	}
	r := NewRunner(t.TempDir(), NewSkillManager(skillsDir, []string{t.TempDir()}), nil)

	job := JobConfig{
		Name:    "demo_skill_codex",
		Type:    "skill",
		Engine:  "codex_auto",
		Skill:   "example-skill",
		Sandbox: "workspace-write",
	}
	eng := EngineConfig{
		Kind:              "codex",
		CLI:               "codex",
		DangerouslyBypass: true,
	}

	cmd, err := r.buildSkillCommand(job, eng)
	if err != nil {
		t.Fatalf("buildSkillCommand failed: %v", err)
	}
	if !strings.Contains(cmd, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("expected dangerous bypass in command: %s", cmd)
	}
	if strings.Contains(cmd, "--sandbox") {
		t.Fatalf("dangerous bypass should not also pass sandbox: %s", cmd)
	}
}

func TestLogPath(t *testing.T) {
	for _, logDir := range []string{t.TempDir(), filepath.Join("relative", "logs")} {
		runner := &Runner{logDir: logDir}
		for _, name := range []string{"demo_job", "nightly..backup", "daily report"} {
			path, err := runner.LogPath(name)
			if err != nil {
				t.Fatalf("LogPath(%q): %v", name, err)
			}
			if want := filepath.Join(logDir, name+".log"); path != want {
				t.Errorf("LogPath(%q) = %q, want %q", name, path, want)
			}
		}
		for _, name := range []string{"", ".", "..", "../outside", "nested/../../outside", "nested/job", `nested\job`, "/absolute"} {
			if path, err := runner.LogPath(name); err == nil || path != "" {
				t.Errorf("LogPath(%q) = %q, %v; want empty path and error", name, path, err)
			}
		}
	}
}

func TestRunRejectsLogPathTraversal(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.log")
	if err := os.WriteFile(outside, []byte("original log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(filepath.Join(dir, "logs"), nil, nil)
	result, err := runner.Run(context.Background(), JobConfig{
		Name:    "../outside",
		Type:    "script",
		Command: "echo overwritten",
		Workdir: dir,
	})
	if err == nil || !strings.Contains(err.Error(), "resolve log path") || result != nil {
		t.Fatalf("Run = %#v, %v; want log path error and no result", result, err)
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original log\n" {
		t.Fatalf("outside log was overwritten: %q", data)
	}
}
