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

func TestLogPathRejectsTraversal(t *testing.T) {
	logDir := t.TempDir()
	r := NewRunner(logDir, nil, nil)

	got, err := r.LogPath("demo_job")
	if err != nil {
		t.Fatalf("LogPath(demo_job): %v", err)
	}
	want := filepath.Join(logDir, "demo_job.log")
	wantAbs, err := filepath.Abs(want)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	if got != filepath.Clean(wantAbs) {
		t.Fatalf("LogPath = %q, want %q", got, wantAbs)
	}

	for _, name := range []string{
		"../etc/passwd",
		"..",
		"foo/bar",
		"foo\\bar",
		"a/../../b",
		"",
	} {
		if _, err := r.LogPath(name); err == nil {
			t.Fatalf("LogPath(%q) unexpectedly succeeded", name)
		}
	}
}
