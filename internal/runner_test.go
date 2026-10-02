package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
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

	command, err := r.buildSkillCommand(context.Background(), job, eng)
	if err != nil {
		t.Fatalf("buildSkillCommand failed: %v", err)
	}
	cmd := command.Args[2]
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

	command, err := r.buildSkillCommand(context.Background(), job, eng)
	if err != nil {
		t.Fatalf("buildSkillCommand failed: %v", err)
	}
	cmd := strings.Join(command.Args, " ")
	if !strings.Contains(cmd, "codex") || !strings.Contains(cmd, "exec") || !strings.Contains(cmd, "--json") {
		t.Fatalf("unexpected codex skill command: %s", cmd)
	}
	if !strings.Contains(cmd, "--skip-git-repo-check") {
		t.Fatalf("expected skip-git-repo-check in command: %s", cmd)
	}
	if !strings.Contains(cmd, "--sandbox read-only") {
		t.Fatalf("expected sandbox override in command: %s", cmd)
	}
	if !strings.Contains(cmd, "model_reasoning_effort") {
		t.Fatalf("expected reasoning effort override in command: %s", cmd)
	}
	prompt, err := io.ReadAll(command.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	if want := buildCodexSkillPrompt(job.Skill, "# test skill\n") + "\n"; string(prompt) != want {
		t.Fatalf("stdin = %q, want %q", prompt, want)
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

	command, err := r.buildSkillCommand(context.Background(), job, eng)
	if err != nil {
		t.Fatalf("buildSkillCommand failed: %v", err)
	}
	cmd := strings.Join(command.Args, " ")
	if !strings.Contains(cmd, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("expected dangerous bypass in command: %s", cmd)
	}
	if strings.Contains(cmd, "--sandbox") {
		t.Fatalf("dangerous bypass should not also pass sandbox: %s", cmd)
	}
}

func TestRunCodexSkillPromptIsData(t *testing.T) {
	for _, bypass := range []bool{false, true} {
		t.Run(fmt.Sprintf("bypass_%t", bypass), func(t *testing.T) {
			workdir := t.TempDir()
			skillsDir := filepath.Join(workdir, "skills")
			skillDir := filepath.Join(skillsDir, "example-skill")
			if err := os.MkdirAll(skillDir, 0o755); err != nil {
				t.Fatal(err)
			}
			body := "# test skill\n__LOOPER_CODEX_SKILL_PROMPT__\n" +
				"printf breakout > breakout.txt\n" +
				"$(touch substitution.txt) `touch backtick.txt`\n" +
				"__LOOPER_CODEX_SKILL_PROMPT__\nexit 73\n'quoted' \"double\" ; | && ${LITERAL}\n"
			if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cli := filepath.Join(workdir, "fake codex's cli")
			if err := os.WriteFile(cli, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > argv.txt\ncat > prompt.txt\nprintf 'codex output\\n'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			engine := EngineConfig{
				Kind: "codex", CLI: cli, SkipGitRepoCheck: true, Search: true,
				DefaultModel: "default-model", DefaultSandbox: "workspace-write",
				DefaultReasoningEffort: "medium", DangerouslyBypass: bypass,
			}
			job := JobConfig{
				Name: "codex_data", Type: "skill", Engine: "codex", Skill: "example-skill",
				Workdir: workdir, Model: "model 'quoted' $(touch model.txt)",
				Sandbox: "read-only", ReasoningEffort: "low",
			}
			runner := NewRunner(filepath.Join(workdir, "logs"), NewSkillManager(skillsDir, nil), map[string]EngineConfig{"codex": engine})
			result, err := runner.Run(context.Background(), job)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			for _, name := range []string{"breakout.txt", "substitution.txt", "backtick.txt", "model.txt"} {
				if _, err := os.Stat(filepath.Join(workdir, name)); !os.IsNotExist(err) {
					t.Errorf("skill/argument text executed in the shell: %s (stat: %v)", name, err)
				}
			}
			prompt, err := os.ReadFile(filepath.Join(workdir, "prompt.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if want := buildCodexSkillPrompt(job.Skill, body) + "\n"; string(prompt) != want {
				t.Errorf("prompt received as %q, want %q", prompt, want)
			}
			args, err := os.ReadFile(filepath.Join(workdir, "argv.txt"))
			if err != nil {
				t.Fatal(err)
			}
			wantArgs := []string{"exec", "--skip-git-repo-check"}
			if bypass {
				wantArgs = append(wantArgs, "--dangerously-bypass-approvals-and-sandbox")
			} else {
				wantArgs = append(wantArgs, "--sandbox", "read-only")
			}
			wantArgs = append(wantArgs, "-m", job.Model, "-c", `model_reasoning_effort="low"`, "--search", "--json", "-")
			if want := strings.Join(wantArgs, "\n") + "\n"; string(args) != want {
				t.Errorf("arguments = %q, want %q", args, want)
			}
			if result.ExitCode != 0 || result.Output != "codex output\n" {
				t.Errorf("Run result = %+v, want successful codex output", result)
			}
		})
	}
}

func TestRunCodexSkillErrors(t *testing.T) {
	for _, name := range []string{"exit", "timeout", "canceled", "missing_cli", "missing_skill"} {
		t.Run(name, func(t *testing.T) {
			workdir := t.TempDir()
			skillsDir := filepath.Join(workdir, "skills")
			if err := os.MkdirAll(filepath.Join(skillsDir, "example-skill"), 0o755); err != nil {
				t.Fatal(err)
			}
			if name != "missing_skill" {
				if err := os.WriteFile(filepath.Join(skillsDir, "example-skill", "SKILL.md"), []byte("# test skill\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cli := filepath.Join(workdir, "fake-codex")
			if name != "missing_cli" {
				body := "#!/bin/sh\ncat >/dev/null\nprintf 'engine failure\\n' >&2\nexit 7\n"
				if name == "timeout" || name == "canceled" {
					body = "#!/bin/sh\ncat >/dev/null\nexec sleep 10\n"
				}
				if err := os.WriteFile(cli, []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			runner := NewRunner(filepath.Join(workdir, "logs"), NewSkillManager(skillsDir, nil), map[string]EngineConfig{
				"codex": {Kind: "codex", CLI: cli},
			})
			ctx := context.Background()
			if name == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			if name == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				timer := time.AfterFunc(100*time.Millisecond, cancel)
				defer timer.Stop()
				defer cancel()
			}
			result, err := runner.Run(ctx, JobConfig{
				Name: "codex_error", Type: "skill", Engine: "codex", Skill: "example-skill", Workdir: workdir,
			})
			switch name {
			case "exit":
				if err != nil || result == nil || result.ExitCode != 7 || result.Output != "engine failure\n" {
					t.Fatalf("Run = %+v, %v; want exit 7 and stderr", result, err)
				}
			case "timeout":
				if result != nil || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("Run = %+v, %v; want wrapped deadline error", result, err)
				}
			case "canceled":
				if result != nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("Run = %+v, %v; want wrapped cancellation error", result, err)
				}
			case "missing_cli":
				if result != nil || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "exec command") {
					t.Fatalf("Run = %+v, %v; want wrapped launch error", result, err)
				}
			case "missing_skill":
				if err != nil || result == nil || result.ExitCode != 1 || !strings.Contains(result.Output, "load codex skill") {
					t.Fatalf("Run = %+v, %v; want failed skill result", result, err)
				}
			}
		})
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
