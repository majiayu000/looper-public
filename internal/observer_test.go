package internal

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().Format("2006-01-02 15:04:05")
	for _, stmt := range []string{
		"CREATE TABLE test_candidate_backlog (id INTEGER PRIMARY KEY, status TEXT)",
		"INSERT INTO test_candidate_backlog (status) VALUES ('pending')",
		"INSERT INTO test_candidate_backlog (status) VALUES ('pending')",
		"INSERT INTO test_candidate_backlog (status) VALUES ('done')",
		"CREATE TABLE test_actions (id INTEGER PRIMARY KEY, action_type TEXT, acted_at TEXT)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, action := range []string{"reply", "reply", "post"} {
		if _, err := db.Exec("INSERT INTO test_actions (action_type, acted_at) VALUES (?, ?)", action, now); err != nil {
			t.Fatal(err)
		}
	}
	return dbPath
}

func TestObserverStatus(t *testing.T) {
	dbPath := setupTestDB(t)

	cfg := &Config{
		Platforms: map[string]PlatformConfig{
			"test_platform": {
				Enabled: true,
				DB:      dbPath,
				Metrics: MetricsConfig{
					CandidateBacklogTable:       "test_candidate_backlog",
					CandidateBacklogStatusField: "status",
					CandidateBacklogPendingVal:  "pending",
					ActionTable:                 "test_actions",
					ActionTypeField:             "action_type",
					ActionTimeField:             "acted_at",
				},
			},
		},
	}

	obs := NewObserver(cfg, nil, nil, time.Now())
	handler := obs.Handler()

	req := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json decode: %v", err)
	}

	p, ok := resp.Platforms["test_platform"]
	if !ok {
		t.Fatal("test_platform not in response")
	}
	if p.CandidateBacklogPending != 2 {
		t.Errorf("expected 2 pending, got %d", p.CandidateBacklogPending)
	}
	if p.TodayActions < 3 {
		t.Errorf("expected at least 3 actions today, got %d", p.TodayActions)
	}
	if resp.Uptime == "" {
		t.Error("expected non-empty uptime")
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

func TestObserverHealth(t *testing.T) {
	cfg := &Config{Platforms: map[string]PlatformConfig{}}
	obs := NewObserver(cfg, nil, nil, time.Now())
	handler := obs.Handler()

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func setupPublishedItemTestDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE published_item_tracking (
			reply_id TEXT,
			parent_id TEXT,
			author TEXT,
			skeleton TEXT,
			reply_style TEXT,
			score INTEGER,
			selection_score REAL,
			candidate_vr INTEGER,
			likes INTEGER,
			views INTEGER,
			target_likes INTEGER,
			target_views INTEGER,
			type TEXT,
			posted_at TEXT,
			check_stage TEXT,
			last_checked_at TEXT
		)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO published_item_tracking (
			reply_id, parent_id, author, skeleton, reply_style, score,
			selection_score, candidate_vr, likes, views, target_likes,
			target_views, type, posted_at, check_stage, last_checked_at
		) VALUES (
			'reply_1', 'parent_1', 'author_1', 'followup', 'short', 10,
			8.5, 10.683506096350133, 1, 2, 3, 4, 'reply',
			datetime('now', 'localtime'), 'early', datetime('now', 'localtime')
		)
	`); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

func TestObserverRepliesHandlesFloatCandidateVR(t *testing.T) {
	dbPath := setupPublishedItemTestDB(t)
	cfg := &Config{
		Platforms: map[string]PlatformConfig{
			"test_platform": {Enabled: true, DB: dbPath},
		},
	}
	obs := NewObserver(cfg, nil, nil, time.Now())

	req := httptest.NewRequest("GET", "/api/replies?platform=test_platform", nil)
	w := httptest.NewRecorder()
	obs.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp paginatedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	items, ok := resp.Data.([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("expected one reply row, got %#v", resp.Data)
	}
	row := items[0].(map[string]any)
	if got := row["candidate_vr"]; got != 10.683506096350133 {
		t.Fatalf("candidate_vr mismatch: %#v", got)
	}
}

func TestObserverTrackingHandlesFloatCandidateVR(t *testing.T) {
	dbPath := setupPublishedItemTestDB(t)
	cfg := &Config{
		Platforms: map[string]PlatformConfig{
			"test_platform": {Enabled: true, DB: dbPath},
		},
	}
	obs := NewObserver(cfg, nil, nil, time.Now())

	req := httptest.NewRequest("GET", "/api/tracking?platform=test_platform", nil)
	w := httptest.NewRecorder()
	obs.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp paginatedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	items, ok := resp.Data.([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("expected one tracking row, got %#v", resp.Data)
	}
	row := items[0].(map[string]any)
	if got := row["candidate_vr"]; got != 10.683506096350133 {
		t.Fatalf("candidate_vr mismatch: %#v", got)
	}
}

func TestObserverReplyReviews(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE reply_review_queue (
			id INTEGER PRIMARY KEY,
			parent_id TEXT,
			author TEXT,
			parent_text TEXT,
			reply_text TEXT,
			status TEXT,
			reviewer_type TEXT,
			reviewer_id TEXT,
			reject_reason TEXT,
			reply_id TEXT,
			created_at TEXT,
			reviewed_at TEXT,
			expires_at TEXT
		)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO reply_review_queue
		(id, parent_id, author, parent_text, reply_text, status, created_at)
		VALUES (1, 'parent_1', 'alice', 'parent text', 'reply text', 'pending',
		        datetime('now', 'localtime'))
	`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	cfg := &Config{
		Platforms: map[string]PlatformConfig{
			"test_platform": {Enabled: true, DB: dbPath},
		},
	}
	obs := NewObserver(cfg, nil, nil, time.Now())

	req := httptest.NewRequest("GET", "/api/reply-reviews?platform=test_platform", nil)
	w := httptest.NewRecorder()
	obs.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp paginatedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	items, ok := resp.Data.([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("expected one review row, got %#v", resp.Data)
	}
	row := items[0].(map[string]any)
	if row["status"] != "pending" {
		t.Fatalf("status mismatch: %#v", row["status"])
	}
}

func TestFilterNDJSONClaude(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello from claude"},{"type":"tool_use","name":"Read"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"second line"}]}}`,
		"",
	}, "\n")

	out := string(filterNDJSON([]byte(in)))

	if !strings.Contains(out, "[text] hello from claude") {
		t.Fatalf("expected Claude text in summary, got: %q", out)
	}
	if !strings.Contains(out, "[tool] Read") {
		t.Fatalf("expected Claude tool in summary, got: %q", out)
	}
	if !strings.Contains(out, "[text] second line") {
		t.Fatalf("expected second text line in summary, got: %q", out)
	}
}

func TestFilterNDJSONCodex(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"I will run ls"}}`,
		`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"/bin/zsh -lc ls","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"/bin/zsh -lc ls","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"command_execution","command":"/bin/zsh -lc false","exit_code":1,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"done"}}`,
		"",
	}, "\n")

	out := string(filterNDJSON([]byte(in)))

	if !strings.Contains(out, "[text] I will run ls") {
		t.Fatalf("expected Codex text in summary, got: %q", out)
	}
	if !strings.Contains(out, "[tool] /bin/zsh -lc ls") {
		t.Fatalf("expected Codex command in summary, got: %q", out)
	}
	if !strings.Contains(out, "[error] exit 1: /bin/zsh -lc false") {
		t.Fatalf("expected Codex command error in summary, got: %q", out)
	}
	if !strings.Contains(out, "[text] done") {
		t.Fatalf("expected final Codex text in summary, got: %q", out)
	}
}

func TestObserverRunRequiresAuthToken(t *testing.T) {
	cfg := &Config{Platforms: map[string]PlatformConfig{}}
	obs := NewObserver(cfg, nil, nil, time.Now())
	obs.ConfigureRunEndpoint("secret-token", true)
	handler := obs.Handler()

	// Missing token must 401 and must not reach scheduler (nil would panic).
	req := httptest.NewRequest("POST", "/run?job=demo", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: expected 401, got %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("POST", "/run?job=demo", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token: expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestObserverRunEmptyTokenRejects(t *testing.T) {
	cfg := &Config{Platforms: map[string]PlatformConfig{}}
	obs := NewObserver(cfg, nil, nil, time.Now())
	// Default enableRun=true but no token configured → fail closed.
	handler := obs.Handler()

	req := httptest.NewRequest("POST", "/run?job=demo", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when no token configured, got %d", w.Code)
	}
}

func TestObserverRunDisabled(t *testing.T) {
	cfg := &Config{Platforms: map[string]PlatformConfig{}}
	obs := NewObserver(cfg, nil, nil, time.Now())
	obs.ConfigureRunEndpoint("secret-token", false)
	handler := obs.Handler()

	req := httptest.NewRequest("POST", "/run?job=demo", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when enable-run=false, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestObserverRunWithValidToken(t *testing.T) {
	runner := NewRunner(t.TempDir(), nil, nil)
	done := make(chan struct{}, 3)
	scheduler := NewScheduler(runner, func(name string, result *RunResult, err error) {
		done <- struct{}{}
	})
	workdir := t.TempDir()
	if err := scheduler.Load([]JobConfig{{
		Name:     "demo_noop",
		Schedule: "@every 24h",
		Type:     "script",
		Command:  "true",
		Workdir:  workdir,
		Timeout:  "5s",
	}}); err != nil {
		t.Fatalf("load jobs: %v", err)
	}

	cfg := &Config{Platforms: map[string]PlatformConfig{}}
	obs := NewObserver(cfg, scheduler, runner, time.Now())
	obs.ConfigureRunEndpoint("secret-token", true)
	handler := obs.Handler()

	waitDone := func(label string) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", label)
		}
	}

	req := httptest.NewRequest("POST", "/run?job=demo_noop", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if resp["status"] != "triggered" || resp["job"] != "demo_noop" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	// Serialize triggers: RunNow is async and skips already-running jobs.
	waitDone("Bearer-triggered job")

	// Scheme names are case-insensitive per HTTP auth.
	req = httptest.NewRequest("POST", "/run?job=demo_noop", nil)
	req.Header.Set("Authorization", "bearer secret-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("lowercase bearer: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	waitDone("lowercase-bearer-triggered job")

	// X-Looper-Token header also accepted.
	req = httptest.NewRequest("POST", "/run?job=demo_noop", nil)
	req.Header.Set("X-Looper-Token", "secret-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("X-Looper-Token: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	waitDone("X-Looper-Token-triggered job")
}

func TestObserverDashboardDoesNotExposeAuthToken(t *testing.T) {
	cfg := &Config{Platforms: map[string]PlatformConfig{}}
	obs := NewObserver(cfg, nil, nil, time.Now())
	obs.ConfigureRunEndpoint("dashboard-secret", true)
	handler := obs.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "dashboard-secret") {
		t.Fatalf("dashboard HTML must not embed the shared auth token")
	}
	if strings.Contains(body, "__LOOPER_AUTH_TOKEN__") {
		t.Fatalf("dashboard HTML must not inject server-side auth token globals")
	}
	if !strings.Contains(body, "looper_auth_token") {
		t.Fatalf("expected dashboard to store operator token client-side")
	}
	if !strings.Contains(body, "Authorization") {
		t.Fatalf("expected dashboard runJob to send Authorization header")
	}
}
