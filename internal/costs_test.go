package internal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCollectEngineWorkdirs(t *testing.T) {
	cfg := &Config{
		Engines: map[string]EngineConfig{
			"claude_main": {Kind: "claude"},
			"codex_main":  {Kind: "codex"},
		},
		Scheduling: SchedulingConfig{
			Jobs: []JobConfig{
				{Name: "a", Type: "skill", Engine: "claude_main", Workdir: "/a"},
				{Name: "b", Type: "skill", Engine: "claude_main", Workdir: "/a"},
				{Name: "c", Type: "skill", Engine: "codex_main", Workdir: "/b"},
				{Name: "d", Type: "script", Workdir: "/c"},
			},
		},
	}

	claudeWds := collectEngineWorkdirs(cfg, "claude")
	codexWds := collectEngineWorkdirs(cfg, "codex")

	if len(claudeWds) != 1 || claudeWds[0] != "/a" {
		t.Fatalf("unexpected claude workdirs: %+v", claudeWds)
	}
	if len(codexWds) != 1 || codexWds[0] != "/b" {
		t.Fatalf("unexpected codex workdirs: %+v", codexWds)
	}
}

func TestBuildPricingTableKnownModelPrices(t *testing.T) {
	table := BuildPricingTable(nil)

	cases := []struct {
		model     string
		input     float64
		output    float64
		cacheRead float64
	}{
		{model: "haiku-4.5", input: 1e-6, output: 5e-6, cacheRead: 0.1e-6},
		{model: "gpt-5.5", input: 5e-6, output: 30e-6, cacheRead: 0.5e-6},
		{model: "gpt-5.4", input: 2.5e-6, output: 15e-6, cacheRead: 0.25e-6},
	}

	for _, tc := range cases {
		got := resolvePricingFrom(table, tc.model)
		if got.Input != tc.input || got.Output != tc.output || got.CacheRead != tc.cacheRead {
			t.Fatalf("pricing %s = input %g output %g cache_read %g, want input %g output %g cache_read %g",
				tc.model, got.Input, got.Output, got.CacheRead, tc.input, tc.output, tc.cacheRead)
		}
	}
}

func TestParseCodexSessionJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-test.jsonl")
	now := time.Now().Format(time.RFC3339Nano)

	content := "" +
		`{"type":"session_meta","payload":{"id":"sess_abcdef1234","timestamp":"` + now + `","cwd":"/tmp/wd","model":"gpt-5.4"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"/x-post"}]}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":200,"output_tokens":50}}}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":3333,"cached_input_tokens":444,"output_tokens":88},"last_token_usage":{"input_tokens":10,"cached_input_tokens":20,"output_tokens":30}}}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write session file: %v", err)
	}

	dayStart := time.Now().Add(-1 * time.Hour)
	dayEnd := time.Now().Add(1 * time.Hour)
	workdirSet := map[string]bool{"/tmp/wd": true}
	sc := parseCodexSessionJSONL(path, workdirSet, dayStart, dayEnd, BuildPricingTable(nil))
	if sc == nil {
		t.Fatal("expected parsed codex session cost")
	}
	if sc.InputTokens != 3333 || sc.CacheRead != 444 || sc.OutputTokens != 88 {
		t.Fatalf("unexpected token usage: %+v", sc)
	}
	if sc.Requests != 2 {
		t.Fatalf("expected 2 requests from last_token_usage events, got %d", sc.Requests)
	}
	if sc.JobType != "x_post" {
		t.Fatalf("expected job type x_post, got %s", sc.JobType)
	}
	if sc.CostUSD <= 0 {
		t.Fatalf("expected positive cost, got %f", sc.CostUSD)
	}
}

func TestParseCodexSessionJSONLFallbackToSumLast(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-fallback.jsonl")
	now := time.Now().Format(time.RFC3339Nano)

	content := "" +
		`{"type":"session_meta","payload":{"id":"sess_xyz","timestamp":"` + now + `","cwd":"/tmp/wd2","model":"gpt-5.4"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"/x-reply"}]}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":5}}}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":300,"cached_input_tokens":40,"output_tokens":15}}}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write session file: %v", err)
	}

	dayStart := time.Now().Add(-1 * time.Hour)
	dayEnd := time.Now().Add(1 * time.Hour)
	workdirSet := map[string]bool{"/tmp/wd2": true}
	sc := parseCodexSessionJSONL(path, workdirSet, dayStart, dayEnd, BuildPricingTable(nil))
	if sc == nil {
		t.Fatal("expected parsed codex session cost")
	}
	if sc.InputTokens != 400 || sc.CacheRead != 60 || sc.OutputTokens != 20 {
		t.Fatalf("expected summed last_token_usage, got %+v", sc)
	}
	if sc.Requests != 2 {
		t.Fatalf("expected 2 requests, got %d", sc.Requests)
	}
}
