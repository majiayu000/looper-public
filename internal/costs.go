package internal

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// modelPricing holds per-token USD pricing for a model family.
type modelPricing struct {
	Input       float64
	Output      float64
	CacheCreate float64
	CacheRead   float64
}

// Pricing table aligned with ccstats/LiteLLM (per-token USD).
// cache_read = 10% of input, cache_create = 125% of input.
var pricingTable = map[string]modelPricing{
	// Opus 4.5/4.6 tier ($5/$25)
	"opus-4.5": {Input: 5e-6, Output: 25e-6, CacheCreate: 6.25e-6, CacheRead: 0.5e-6},
	"opus-4-6": {Input: 5e-6, Output: 25e-6, CacheCreate: 6.25e-6, CacheRead: 0.5e-6},
	"opus-4-7": {Input: 5e-6, Output: 25e-6, CacheCreate: 6.25e-6, CacheRead: 0.5e-6},
	// Legacy Opus 4 ($15/$75)
	"opus-4": {Input: 15e-6, Output: 75e-6, CacheCreate: 18.75e-6, CacheRead: 1.5e-6},
	// Sonnet 4.5/4.6 ($3/$15)
	"sonnet-4":   {Input: 3e-6, Output: 15e-6, CacheCreate: 3.75e-6, CacheRead: 0.3e-6},
	"sonnet-4.5": {Input: 3e-6, Output: 15e-6, CacheCreate: 3.75e-6, CacheRead: 0.3e-6},
	"sonnet-4-6": {Input: 3e-6, Output: 15e-6, CacheCreate: 3.75e-6, CacheRead: 0.3e-6},
	// Haiku 4.5 ($1/$5)
	"haiku-4":   {Input: 1e-6, Output: 5e-6, CacheCreate: 1.25e-6, CacheRead: 0.1e-6},
	"haiku-4.5": {Input: 1e-6, Output: 5e-6, CacheCreate: 1.25e-6, CacheRead: 0.1e-6},
	// OpenAI GPT pricing. Codex logs expose cached input tokens, not cache writes.
	"gpt-5.5":      {Input: 5e-6, Output: 30e-6, CacheCreate: 5e-6, CacheRead: 0.5e-6},
	"gpt-5.4":      {Input: 2.5e-6, Output: 15e-6, CacheCreate: 2.5e-6, CacheRead: 0.25e-6},
	"gpt-5.4-mini": {Input: 0.75e-6, Output: 4.5e-6, CacheCreate: 0.75e-6, CacheRead: 0.075e-6},
}

// fallback: Sonnet pricing for unknown models
var fallbackPricing = pricingTable["sonnet-4"]

type SessionCost struct {
	SessionID    string  `json:"session_id"`
	JobType      string  `json:"job_type"`
	Model        string  `json:"model"`
	StartedAt    string  `json:"started_at"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CacheWrite   int64   `json:"cache_write"`
	CacheRead    int64   `json:"cache_read"`
	Requests     int     `json:"requests"`
	CostUSD      float64 `json:"cost_usd"`
}

type CostSummary struct {
	JobType      string  `json:"job_type"`
	Sessions     int     `json:"sessions"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CacheWrite   int64   `json:"cache_write"`
	CacheRead    int64   `json:"cache_read"`
	CostUSD      float64 `json:"cost_usd"`
	AvgCostUSD   float64 `json:"avg_cost_usd"`
}

type CostReport struct {
	Date     string        `json:"date"`
	Total    float64       `json:"total_usd"`
	ByType   []CostSummary `json:"by_type"`
	Sessions []SessionCost `json:"sessions"`
}

// claudeProjectDir derives the Claude Code project directory from a workdir.
func claudeProjectDir(workdir string) string {
	home, _ := os.UserHomeDir()
	slug := strings.ReplaceAll(workdir, "/", "-")
	return filepath.Join(home, ".claude", "projects", slug)
}

// BuildPricingTable merges config pricing (per-token USD) over built-in defaults.
func BuildPricingTable(cfgPricing map[string]PricingConfig) map[string]modelPricing {
	table := make(map[string]modelPricing, len(pricingTable))
	for k, v := range pricingTable {
		table[k] = v
	}
	for k, v := range cfgPricing {
		table[k] = modelPricing{
			Input:       v.Input,
			Output:      v.Output,
			CacheCreate: v.CacheCreate,
			CacheRead:   v.CacheRead,
		}
	}
	return table
}

// ScanCosts reads session JSONL files for a given date and computes costs.
func ScanCosts(cfg *Config, date string, pricing map[string]modelPricing) (*CostReport, error) {
	targetDate, err := time.Parse("2006-01-02", date)
	if err != nil {
		return nil, err
	}
	dayStart := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 0, 0, 0, 0, time.Local)
	dayEnd := dayStart.Add(24 * time.Hour)

	var sessions []SessionCost

	claudeWorkdirs := collectEngineWorkdirs(cfg, "claude")
	codexWorkdirs := collectEngineWorkdirs(cfg, "codex")

	sessions = append(sessions, scanClaudeSessions(claudeWorkdirs, dayStart, dayEnd, pricing)...)
	sessions = append(sessions, scanCodexSessions(codexWorkdirs, dayStart, dayEnd, pricing)...)

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt > sessions[j].StartedAt
	})

	// Aggregate by job type
	typeMap := make(map[string]*CostSummary)
	total := 0.0
	for _, s := range sessions {
		total += s.CostUSD
		cs, ok := typeMap[s.JobType]
		if !ok {
			cs = &CostSummary{JobType: s.JobType}
			typeMap[s.JobType] = cs
		}
		cs.Sessions++
		cs.InputTokens += s.InputTokens
		cs.OutputTokens += s.OutputTokens
		cs.CacheWrite += s.CacheWrite
		cs.CacheRead += s.CacheRead
		cs.CostUSD += s.CostUSD
	}

	var byType []CostSummary
	for _, cs := range typeMap {
		if cs.Sessions > 0 {
			cs.AvgCostUSD = cs.CostUSD / float64(cs.Sessions)
		}
		byType = append(byType, *cs)
	}
	sort.Slice(byType, func(i, j int) bool {
		return byType[i].CostUSD > byType[j].CostUSD
	})

	return &CostReport{
		Date:     date,
		Total:    total,
		ByType:   byType,
		Sessions: sessions,
	}, nil
}

func collectEngineWorkdirs(cfg *Config, engineKind string) []string {
	seen := make(map[string]bool)
	var workdirs []string
	for _, job := range cfg.Scheduling.Jobs {
		if job.Type != "skill" || job.Engine == "" || job.Workdir == "" {
			continue
		}
		eng, ok := cfg.Engines[job.Engine]
		if !ok || eng.Kind != engineKind {
			continue
		}
		if seen[job.Workdir] {
			continue
		}
		seen[job.Workdir] = true
		workdirs = append(workdirs, job.Workdir)
	}
	return workdirs
}

func scanClaudeSessions(workdirs []string, dayStart, dayEnd time.Time, pricing map[string]modelPricing) []SessionCost {
	var sessions []SessionCost
	seen := make(map[string]bool)
	for _, wd := range workdirs {
		projDir := claudeProjectDir(wd)
		if seen[projDir] {
			continue
		}
		seen[projDir] = true

		entries, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			mtime := info.ModTime()
			if mtime.Before(dayStart) || mtime.After(dayEnd) {
				continue
			}

			sc := parseSessionJSONL(filepath.Join(projDir, e.Name()), pricing)
			if sc == nil {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".jsonl")
			if len(name) > 8 {
				name = name[:8]
			}
			sc.SessionID = name
			sessions = append(sessions, *sc)
		}
	}
	return sessions
}

func scanCodexSessions(workdirs []string, dayStart, dayEnd time.Time, pricing map[string]modelPricing) []SessionCost {
	if len(workdirs) == 0 {
		return nil
	}
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".codex", "sessions")

	workdirSet := make(map[string]bool, len(workdirs))
	for _, wd := range workdirs {
		workdirSet[wd] = true
	}

	var sessions []SessionCost
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".jsonl") {
			return nil
		}
		sc := parseCodexSessionJSONL(path, workdirSet, dayStart, dayEnd, pricing)
		if sc != nil {
			sessions = append(sessions, *sc)
		}
		return nil
	})
	return sessions
}

func parseCodexSessionJSONL(path string, workdirSet map[string]bool, dayStart, dayEnd time.Time, table map[string]modelPricing) *SessionCost {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var (
		sessionID    string
		startedAt    string
		cwd          string
		sessionModel string
		firstPrompt  string
		inputTokens  int64
		cacheRead    int64
		outputTokens int64
		sumLastIn    int64
		sumLastCache int64
		sumLastOut   int64
		hasTotal     bool
		requests     int
	)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		var raw struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(line, &raw) != nil {
			continue
		}

		switch raw.Type {
		case "session_meta":
			var meta struct {
				ID        string `json:"id"`
				Timestamp string `json:"timestamp"`
				Cwd       string `json:"cwd"`
				Model     string `json:"model"`
			}
			if json.Unmarshal(raw.Payload, &meta) != nil {
				continue
			}
			if sessionID == "" {
				sessionID = meta.ID
			}
			if startedAt == "" {
				startedAt = meta.Timestamp
			}
			if cwd == "" {
				cwd = meta.Cwd
			}
			if sessionModel == "" && meta.Model != "" {
				sessionModel = normalizeModel(meta.Model)
			}
		case "event_msg":
			var evt struct {
				Type string `json:"type"`
				Info struct {
					TotalTokenUsage struct {
						InputTokens       int64 `json:"input_tokens"`
						CachedInputTokens int64 `json:"cached_input_tokens"`
						OutputTokens      int64 `json:"output_tokens"`
					} `json:"total_token_usage"`
					LastTokenUsage struct {
						InputTokens       int64 `json:"input_tokens"`
						CachedInputTokens int64 `json:"cached_input_tokens"`
						OutputTokens      int64 `json:"output_tokens"`
					} `json:"last_token_usage"`
				} `json:"info"`
			}
			if json.Unmarshal(raw.Payload, &evt) != nil || evt.Type != "token_count" {
				continue
			}
			if evt.Info.TotalTokenUsage.InputTokens > 0 || evt.Info.TotalTokenUsage.CachedInputTokens > 0 || evt.Info.TotalTokenUsage.OutputTokens > 0 {
				inputTokens = evt.Info.TotalTokenUsage.InputTokens
				cacheRead = evt.Info.TotalTokenUsage.CachedInputTokens
				outputTokens = evt.Info.TotalTokenUsage.OutputTokens
				hasTotal = true
			}
			if evt.Info.LastTokenUsage.InputTokens > 0 || evt.Info.LastTokenUsage.CachedInputTokens > 0 || evt.Info.LastTokenUsage.OutputTokens > 0 {
				sumLastIn += evt.Info.LastTokenUsage.InputTokens
				sumLastCache += evt.Info.LastTokenUsage.CachedInputTokens
				sumLastOut += evt.Info.LastTokenUsage.OutputTokens
				requests++
			}
		case "response_item":
			if firstPrompt != "" {
				continue
			}
			var item struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(raw.Payload, &item) != nil {
				continue
			}
			if item.Type != "message" || item.Role != "user" {
				continue
			}
			for _, c := range item.Content {
				if c.Text != "" {
					firstPrompt = c.Text
					break
				}
			}
		}
	}

	if !workdirSet[cwd] {
		return nil
	}
	if startedAt == "" {
		return nil
	}
	ts, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		ts, err = time.Parse(time.RFC3339, startedAt)
		if err != nil {
			return nil
		}
	}
	if ts.Before(dayStart) || ts.After(dayEnd) {
		return nil
	}
	if !hasTotal {
		inputTokens = sumLastIn
		cacheRead = sumLastCache
		outputTokens = sumLastOut
	}
	if inputTokens == 0 && cacheRead == 0 && outputTokens == 0 {
		return nil
	}
	if requests == 0 {
		requests = 1
	}

	pr := resolvePricingFrom(table, sessionModel)
	cost := float64(inputTokens)*pr.Input +
		float64(outputTokens)*pr.Output +
		float64(cacheRead)*pr.CacheRead

	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	if len(sessionID) > 8 {
		sessionID = sessionID[:8]
	}

	jobType := classifyJob(firstPrompt)
	if jobType == "interactive" {
		jobType = "codex"
	}

	return &SessionCost{
		SessionID:    sessionID,
		JobType:      jobType,
		Model:        sessionModel,
		StartedAt:    startedAt,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		CacheWrite:   0,
		CacheRead:    cacheRead,
		Requests:     requests,
		CostUSD:      cost,
	}
}

// reqCandidate tracks the best entry per message ID (ccstats dedup strategy).
type reqCandidate struct {
	completed *reqUsage // entry with stop_reason (preferred)
	latest    *reqUsage // latest entry (fallback)
}

type reqUsage struct {
	model                                string
	input, output, cacheWrite, cacheRead int64
	hasStop                              bool
}

func (c *reqCandidate) finalize() *reqUsage {
	if c.completed != nil {
		return c.completed
	}
	return c.latest
}

func parseSessionJSONL(path string, table map[string]modelPricing) *SessionCost {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	reqs := make(map[string]*reqCandidate)
	firstPrompt := ""
	startTime := ""
	sessionModel := ""

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		var msg jsonlMsg
		if json.Unmarshal(line, &msg) != nil {
			continue
		}

		if msg.Timestamp != "" && startTime == "" {
			startTime = msg.Timestamp
		}

		if msg.Type == "user" && firstPrompt == "" {
			firstPrompt = extractFirstPrompt(line)
		}

		if msg.Type == "assistant" && msg.Message.ID != "" {
			u := msg.Message.Usage
			if u.OutputTokens == 0 && u.InputTokens == 0 &&
				u.CacheCreationInputTokens == 0 && u.CacheReadInputTokens == 0 {
				continue
			}

			model := normalizeModel(msg.Message.Model)
			if sessionModel == "" && model != "" {
				sessionModel = model
			}

			entry := &reqUsage{
				model:      model,
				input:      int64(u.InputTokens),
				output:     int64(u.OutputTokens),
				cacheWrite: int64(u.CacheCreationInputTokens),
				cacheRead:  int64(u.CacheReadInputTokens),
				hasStop:    msg.Message.StopReason != "",
			}

			cand, ok := reqs[msg.Message.ID]
			if !ok {
				cand = &reqCandidate{}
				reqs[msg.Message.ID] = cand
			}
			if entry.hasStop {
				cand.completed = entry
			}
			cand.latest = entry
		}
	}

	if len(reqs) == 0 {
		return nil
	}

	pricing := resolvePricingFrom(table, sessionModel)

	var totalIn, totalOut, totalCW, totalCR int64
	var totalCost float64
	for _, cand := range reqs {
		u := cand.finalize()
		totalIn += u.input
		totalOut += u.output
		totalCW += u.cacheWrite
		totalCR += u.cacheRead

		p := resolvePricingFrom(table, u.model)
		totalCost += float64(u.input)*p.Input +
			float64(u.output)*p.Output +
			float64(u.cacheWrite)*p.CacheCreate +
			float64(u.cacheRead)*p.CacheRead
	}

	// If per-request pricing failed, use session-level pricing
	if totalCost == 0 && (totalIn+totalOut+totalCW+totalCR) > 0 {
		totalCost = float64(totalIn)*pricing.Input +
			float64(totalOut)*pricing.Output +
			float64(totalCW)*pricing.CacheCreate +
			float64(totalCR)*pricing.CacheRead
	}

	return &SessionCost{
		JobType:      classifyJob(firstPrompt),
		Model:        sessionModel,
		StartedAt:    startTime,
		InputTokens:  totalIn,
		OutputTokens: totalOut,
		CacheWrite:   totalCW,
		CacheRead:    totalCR,
		Requests:     len(reqs),
		CostUSD:      totalCost,
	}
}

// jsonlMsg is the minimal structure we need from JSONL lines.
type jsonlMsg struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// normalizeModel strips vendor prefix and date suffix, matching ccstats behavior.
// "claude-opus-4-6-20250404" → "opus-4-6"
// "anthropic.claude-3-5-sonnet-20241022" → "3-5-sonnet"
func normalizeModel(model string) string {
	if model == "" {
		return ""
	}
	name := strings.TrimPrefix(model, "anthropic.")
	name = strings.TrimPrefix(name, "claude-")

	// Strip 8-digit date suffix
	if idx := strings.LastIndex(name, "-"); idx > 0 {
		suffix := name[idx+1:]
		if len(suffix) == 8 {
			allDigits := true
			for _, c := range suffix {
				if c < '0' || c > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				name = name[:idx]
			}
		}
	}
	return name
}

// resolvePricingFrom finds the best pricing match from the given table.
func resolvePricingFrom(table map[string]modelPricing, model string) modelPricing {
	if model == "" {
		return fallbackPricing
	}
	ml := strings.ToLower(model)

	// Exact match
	if p, ok := table[ml]; ok {
		return p
	}

	// Keyword match — default to latest tier
	switch {
	case strings.Contains(ml, "opus"):
		if p, ok := table["opus-4.5"]; ok {
			return p
		}
	case strings.Contains(ml, "haiku"):
		if p, ok := table["haiku-4.5"]; ok {
			return p
		}
	case strings.Contains(ml, "sonnet"):
		if p, ok := table["sonnet-4"]; ok {
			return p
		}
	}

	return fallbackPricing
}

func extractFirstPrompt(line []byte) string {
	var raw struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &raw) != nil {
		return ""
	}

	// Try array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw.Message.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}

	// Try plain string
	var s string
	if json.Unmarshal(raw.Message.Content, &s) == nil {
		return s
	}
	return ""
}

func classifyJob(prompt string) string {
	p := strings.ToLower(prompt)
	switch {
	case strings.Contains(p, "x-reply-eval"):
		return "x_reply_eval"
	case strings.Contains(p, "x-post-eval"):
		return "x_post_eval"
	case strings.Contains(p, "x-system-eval"):
		return "x_system_eval"
	case strings.Contains(p, "looper-eval"):
		return "looper_eval"
	case strings.Contains(p, "x-learn-reply"):
		return "x_learn_reply"
	case strings.Contains(p, "x-learn-post"):
		return "x_learn_post"
	case strings.Contains(p, "x-reply"):
		return "x_reply"
	case strings.Contains(p, "x-post"):
		return "x_post"
	case strings.Contains(p, "reddit-reply"):
		return "reddit_reply"
	case strings.Contains(p, "reddit"):
		return "reddit"
	case strings.Contains(p, "social-stats"):
		return "social_stats"
	default:
		return "interactive"
	}
}
