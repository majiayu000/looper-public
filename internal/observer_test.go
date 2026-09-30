package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	return setupPublishedItemTestDBNamed(t, "published_item_tracking")
}

func setupPublishedItemTestDBNamed(t *testing.T, table string) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(fmt.Sprintf(`
		CREATE TABLE %s (
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
	`, table)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`
		INSERT INTO %s (
			reply_id, parent_id, author, skeleton, reply_style, score,
			selection_score, candidate_vr, likes, views, target_likes,
			target_views, type, posted_at, check_stage, last_checked_at
		) VALUES (
			'reply_1', 'parent_1', 'author_1', 'followup', 'short', 10,
			8.5, 10.683506096350133, 1, 2, 3, 4, 'reply',
			datetime('now', 'localtime'), 'early', datetime('now', 'localtime')
		)
	`, table)); err != nil {
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

func TestObserverPublishedItemsTable(t *testing.T) {
	for _, tc := range []struct {
		name, table, configured string
		keepDefault, optional   bool
	}{
		{name: "omitted", table: "published_item_tracking", optional: true},
		{name: "explicit_default", table: "published_item_tracking", configured: "published_item_tracking", optional: true},
		{name: "custom_only", table: "custom_published_items", configured: "custom_published_items", optional: true},
		{name: "custom_with_default", table: "custom_published_items", configured: "custom_published_items", keepDefault: true, optional: true},
		{name: "custom_without_optional_columns", table: "custom_published_items", configured: "custom_published_items"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := setupPublishedItemTestDBNamed(t, tc.table)
			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if tc.keepDefault {
				if _, err := db.Exec("CREATE TABLE published_item_tracking AS SELECT * FROM " + tc.table); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec("UPDATE " + tc.table + " SET reply_id = 'configured_row'"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("INSERT INTO " + tc.table + " SELECT * FROM " + tc.table); err != nil {
				t.Fatal(err)
			}
			if !tc.optional {
				for _, column := range []string{"selection_score", "candidate_vr"} {
					if _, err := db.Exec("ALTER TABLE " + tc.table + " DROP COLUMN " + column); err != nil {
						t.Fatal(err)
					}
				}
			}
			obs := NewObserver(&Config{Platforms: map[string]PlatformConfig{
				"x": {Enabled: true, DB: dbPath, Metrics: MetricsConfig{PublishedItemsTable: tc.configured}},
			}}, nil, nil, time.Now())
			for _, endpoint := range []string{"replies", "tracking"} {
				t.Run(endpoint, func(t *testing.T) {
					w := httptest.NewRecorder()
					// Omitted platform also exercises the existing x default.
					obs.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/"+endpoint+"?page=2&per_page=1", nil))
					if w.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
					var resp paginatedResponse
					if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
						t.Fatal(err)
					}
					items, ok := resp.Data.([]any)
					if !ok || len(items) != 1 || resp.Total != 2 || resp.Page != 2 || resp.Pages != 2 {
						t.Fatalf("wrong configured table or pagination: %#v", resp)
					}
					row := items[0].(map[string]any)
					if row["reply_id"] != "configured_row" {
						t.Fatalf("wrong table row: %#v", row)
					}
					if tc.optional {
						if row["selection_score"] != 8.5 || row["candidate_vr"] != 10.683506096350133 {
							t.Fatalf("optional columns: %#v", row)
						}
					} else if row["selection_score"] != nil || row["candidate_vr"] != nil {
						t.Fatalf("missing optional columns: %#v", row)
					}
				})
			}
		})
	}
}

func TestObserverPublishedItemsErrors(t *testing.T) {
	dbPath := setupPublishedItemTestDB(t)
	for _, tc := range []struct {
		name, dbPath, table, platform string
		code                          int
	}{
		{name: "invalid_identifier", dbPath: dbPath, table: "published_item_tracking; DROP TABLE published_item_tracking", platform: "x", code: http.StatusBadRequest},
		{name: "missing_table", dbPath: dbPath, table: "missing_items", platform: "x", code: http.StatusInternalServerError},
		{name: "missing_db", dbPath: filepath.Join(t.TempDir(), "missing.db"), table: "custom_items", platform: "x", code: http.StatusInternalServerError},
		{name: "unknown_platform", dbPath: dbPath, table: "published_item_tracking", platform: "unknown", code: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := NewObserver(&Config{Platforms: map[string]PlatformConfig{
				"x": {Enabled: true, DB: tc.dbPath, Metrics: MetricsConfig{PublishedItemsTable: tc.table}},
			}}, nil, nil, time.Now())
			for _, endpoint := range []string{"replies", "tracking"} {
				w := httptest.NewRecorder()
				obs.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/"+endpoint+"?platform="+tc.platform, nil))
				if w.Code != tc.code {
					t.Errorf("%s: status=%d want=%d body=%s", endpoint, w.Code, tc.code, w.Body.String())
				}
				var resp map[string]string
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp["error"] == "" {
					t.Errorf("%s: missing JSON error: %s", endpoint, w.Body.String())
				}
			}
		})
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM published_item_tracking").Scan(&count); err != nil || count != 1 {
		t.Fatalf("invalid name changed data: count=%d err=%v", count, err)
	}
}

func TestObserverPublishedItemsHotReload(t *testing.T) {
	configs := make([]*Config, 2)
	for i, table := range []string{"first_items", "second_items"} {
		configs[i] = &Config{Platforms: map[string]PlatformConfig{
			"x": {Enabled: true, DB: setupPublishedItemTestDBNamed(t, table), Metrics: MetricsConfig{PublishedItemsTable: table}},
		}}
	}
	obs := NewObserver(configs[0], nil, nil, time.Now())
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			obs.UpdateConfig(configs[i%2])
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	for i := 0; i < 100; i++ {
		for _, endpoint := range []string{"replies", "tracking"} {
			w := httptest.NewRecorder()
			obs.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/"+endpoint, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("%s mixed database/table during reload: status=%d body=%s", endpoint, w.Code, w.Body.String())
			}
			var resp paginatedResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Total != 1 {
				t.Fatalf("%s wrong total during reload: %#v", endpoint, resp)
			}
		}
	}
}

func TestValidateMetricsConfigRejectsInvalidPublishedItemsTable(t *testing.T) {
	obs := NewObserver(&Config{Platforms: map[string]PlatformConfig{
		"x": {Enabled: true, DB: setupPublishedItemTestDB(t), Metrics: MetricsConfig{PublishedItemsTable: "bad-table;drop"}},
	}}, nil, nil, time.Now())
	w := httptest.NewRecorder()
	obs.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Platforms["x"].Health != "config_error" {
		t.Fatalf("invalid metrics config not reported: %#v", resp)
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
