package internal

import (
	"bytes"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed dashboard.html
var dashboardHTML []byte

var validIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

type Observer struct {
	mu        sync.RWMutex
	config    *Config
	scheduler *Scheduler
	runner    *Runner
	startTime time.Time
	authToken string
	enableRun bool
}

type StatusResponse struct {
	Platforms map[string]PlatformStatus `json:"platforms"`
	Uptime    string                    `json:"uptime"`
}

type PlatformStatus struct {
	CandidateBacklogPending int            `json:"candidate_backlog_pending"`
	TodayActions            int            `json:"today_actions"`
	ActionBreakdown         map[string]int `json:"action_breakdown,omitempty"`
	Health                  string         `json:"health"`
}

func NewObserver(config *Config, scheduler *Scheduler, runner *Runner, startTime time.Time) *Observer {
	return &Observer{
		config:    config,
		scheduler: scheduler,
		runner:    runner,
		startTime: startTime,
		enableRun: true,
	}
}

// ConfigureRunEndpoint sets whether POST /run is available and which shared token
// is required. An empty token rejects all run requests (fail closed).
func (o *Observer) ConfigureRunEndpoint(authToken string, enableRun bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.authToken = authToken
	o.enableRun = enableRun
}

func (o *Observer) UpdateConfig(config *Config) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.config = config
}

func (o *Observer) runEndpointConfig() (authToken string, enableRun bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.authToken, o.enableRun
}

func (o *Observer) getConfig() *Config {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.config
}

func (o *Observer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/jobs", o.handleJobs)
	mux.HandleFunc("GET /api/replies", o.handleReplies)
	mux.HandleFunc("GET /api/candidate-backlog", o.handleCandidateBacklog)
	mux.HandleFunc("GET /api/reply-reviews", o.handleReplyReviews)
	mux.HandleFunc("GET /api/tracking", o.handleTracking)
	mux.HandleFunc("GET /api/rounds", o.handleRounds)
	mux.HandleFunc("GET /api/costs", o.handleCosts)
	mux.HandleFunc("GET /status", o.handleStatus)
	mux.HandleFunc("GET /health", o.handleHealth)
	mux.HandleFunc("POST /run", o.handleRun)
	mux.HandleFunc("GET /logs", o.handleLogs)
	mux.HandleFunc("GET /{$}", o.handleDashboard)
	return mux
}

func (o *Observer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Never embed the shared run token in GET / HTML. The dashboard stores the
	// operator-provided token client-side (localStorage) so unauthenticated
	// clients cannot harvest it when -listen is non-loopback.
	w.Write(dashboardHTML)
}

func (o *Observer) handleJobs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(o.scheduler.JobStatuses()); err != nil {
		slog.Error("encode jobs response", "error", err)
	}
}

func (o *Observer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
		slog.Error("encode health response", "error", err)
	}
}

func (o *Observer) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := o.getConfig()
	resp := StatusResponse{
		Platforms: make(map[string]PlatformStatus),
		Uptime:    time.Since(o.startTime).Truncate(time.Second).String(),
	}

	for name, platform := range cfg.Platforms {
		if !platform.Enabled {
			continue
		}
		status := o.queryPlatformMetrics(name, platform)
		resp.Platforms[name] = status
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("encode status response", "error", err)
	}
}

func (o *Observer) handleRun(w http.ResponseWriter, r *http.Request) {
	authToken, enableRun := o.runEndpointConfig()
	if !enableRun {
		jsonError(w, "POST /run is disabled (pass -enable-run=true to allow)", http.StatusForbidden)
		return
	}
	if !authorizeSharedToken(r, authToken) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="looper"`)
		jsonError(w, "unauthorized: provide Authorization: Bearer <token> or X-Looper-Token", http.StatusUnauthorized)
		return
	}

	jobName := r.URL.Query().Get("job")
	if jobName == "" {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"error": "missing ?job= parameter",
			"jobs":  o.scheduler.JobNames(),
		}); err != nil {
			slog.Error("encode run response", "error", err)
		}
		return
	}
	if err := o.scheduler.RunNow(jobName); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"error": err.Error(),
			"jobs":  o.scheduler.JobNames(),
		}); err != nil {
			slog.Error("encode run error response", "error", err)
		}
		return
	}
	slog.Info("job triggered manually", "name", jobName)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{
		"status": "triggered",
		"job":    jobName,
	}); err != nil {
		slog.Error("encode run success response", "error", err)
	}
}

func extractSharedToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		scheme, value, ok := strings.Cut(auth, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(value)
		}
	}
	if token := strings.TrimSpace(r.Header.Get("X-Looper-Token")); token != "" {
		return token
	}
	return ""
}

// authorizeSharedToken requires a non-empty configured token and a matching request token.
func authorizeSharedToken(r *http.Request, expected string) bool {
	if expected == "" {
		return false
	}
	provided := extractSharedToken(r)
	if provided == "" || len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b\[[?][0-9;]*[a-zA-Z]|\x1b\[<[a-zA-Z]|\x04`)

func stripANSI(data []byte) []byte {
	return ansiRegex.ReplaceAll(data, nil)
}

func appendSummaryLines(buf *[]byte, prefix, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		*buf = append(*buf, []byte(prefix+" "+line+"\n")...)
	}
}

// filterNDJSON extracts key events from both Claude stream-json and Codex --json logs.
func filterNDJSON(data []byte) []byte {
	var buf []byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var evt map[string]any
		if json.Unmarshal(line, &evt) != nil {
			continue
		}
		typ, _ := evt["type"].(string)

		// Claude stream-json format
		if typ == "assistant" {
			msg, ok := evt["message"].(map[string]any)
			if !ok {
				continue
			}
			content, _ := msg["content"].([]any)
			for _, c := range content {
				block, _ := c.(map[string]any)
				btype, _ := block["type"].(string)
				switch btype {
				case "text":
					text, _ := block["text"].(string)
					appendSummaryLines(&buf, "[text]", text)
				case "tool_use":
					name, _ := block["name"].(string)
					appendSummaryLines(&buf, "[tool]", name)
				}
			}
			continue
		}

		// Codex --json format
		if typ == "item.started" || typ == "item.completed" {
			item, ok := evt["item"].(map[string]any)
			if !ok {
				continue
			}
			itemType, _ := item["type"].(string)
			switch itemType {
			case "agent_message":
				if typ == "item.completed" {
					text, _ := item["text"].(string)
					appendSummaryLines(&buf, "[text]", text)
				}
			case "command_execution":
				command, _ := item["command"].(string)
				if typ == "item.started" {
					appendSummaryLines(&buf, "[tool]", command)
					continue
				}

				exitVal, ok := item["exit_code"]
				if !ok || exitVal == nil {
					continue
				}
				exitCode, ok := exitVal.(float64)
				if !ok {
					continue
				}
				if int(exitCode) != 0 {
					appendSummaryLines(&buf, "[error]", fmt.Sprintf("exit %d: %s", int(exitCode), command))
				}
			}
			continue
		}

		// Generic error lines
		if typ == "error" || typ == "turn.failed" {
			msg, _ := evt["message"].(string)
			appendSummaryLines(&buf, "[error]", msg)
		}
	}
	return buf
}

func (o *Observer) handleLogs(w http.ResponseWriter, r *http.Request) {
	jobName := r.URL.Query().Get("job")
	if jobName == "" || o.runner == nil {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"error": "missing ?job= parameter",
			"jobs":  o.scheduler.JobNames(),
		}); err != nil {
			slog.Error("encode logs response", "error", err)
		}
		return
	}

	logPath := o.runner.LogPath(jobName)
	data, err := os.ReadFile(logPath)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		if err := json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("no log for job %s", jobName),
		}); err != nil {
			slog.Error("encode logs error response", "error", err)
		}
		return
	}

	data = stripANSI(data)

	if r.URL.Query().Get("filter") == "summary" {
		data = filterNDJSON(data)
	}

	maxTail := 16384
	if len(data) > maxTail {
		data = data[len(data)-maxTail:]
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

// --- Platform-specific data APIs ---

func (o *Observer) openPlatformDB(platform string) (*sql.DB, error) {
	cfg := o.getConfig()
	p, ok := cfg.Platforms[platform]
	if !ok {
		return nil, fmt.Errorf("platform %q not found", platform)
	}
	return sql.Open("sqlite", p.DB+"?mode=ro")
}

func platformHasColumn(db *sql.DB, table, column string) bool {
	rows, err := db.Query(fmt.Sprintf(`SELECT name FROM pragma_table_info('%s')`, table))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		if strings.EqualFold(name, column) {
			return true
		}
	}
	return false
}

func platformHasTable(db *sql.DB, table string) bool {
	row := db.QueryRow("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", table)
	var exists int
	return row.Scan(&exists) == nil
}

func (o *Observer) handleReplies(w http.ResponseWriter, r *http.Request) {
	platform := r.URL.Query().Get("platform")
	if platform == "" {
		platform = "x"
	}
	db, err := o.openPlatformDB(platform)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	defer db.Close()

	pp := parsePage(r)
	today := time.Now().Format("2006-01-02")

	var total int
	db.QueryRow("SELECT COUNT(*) FROM published_item_tracking WHERE date(posted_at) = ?", today).Scan(&total)

	selectionScoreExpr := "NULL AS selection_score"
	if platformHasColumn(db, "published_item_tracking", "selection_score") {
		selectionScoreExpr = "selection_score"
	}
	candidateVRExpr := "NULL AS candidate_vr"
	if platformHasColumn(db, "published_item_tracking", "candidate_vr") {
		candidateVRExpr = "candidate_vr"
	}

	replyQuery := fmt.Sprintf(`
		SELECT reply_id, parent_id, author, skeleton, reply_style,
		       score, %s, %s, likes, views, target_likes, target_views,
		       type, posted_at
		FROM published_item_tracking
		WHERE date(posted_at) = ?
		ORDER BY posted_at DESC
		LIMIT ? OFFSET ?
	`, selectionScoreExpr, candidateVRExpr)

	rows, err := db.Query(replyQuery, today, pp.PerPage, pp.Offset)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var replyID, parentID, author string
		var skeleton, replyStyle, replyType, postedAt sql.NullString
		var score, selectionScore, candidateVR sql.NullFloat64
		var likes, views, targetLikes, targetViews int
		if err := rows.Scan(&replyID, &parentID, &author, &skeleton, &replyStyle,
			&score, &selectionScore, &candidateVR, &likes, &views, &targetLikes, &targetViews,
			&replyType, &postedAt); err != nil {
			slog.Error("scan reply row", "error", err)
			continue
		}
		result = append(result, map[string]any{
			"reply_id":        replyID,
			"parent_id":       parentID,
			"author":          author,
			"skeleton":        nullStr(skeleton),
			"reply_style":     nullStr(replyStyle),
			"score":           score.Float64,
			"selection_score": nullFloat(selectionScore),
			"candidate_vr":    nullFloat(candidateVR),
			"likes":           likes,
			"views":           views,
			"target_likes":    targetLikes,
			"target_views":    targetViews,
			"type":            nullStr(replyType),
			"posted_at":       nullStr(postedAt),
		})
	}
	if result == nil {
		result = []map[string]any{}
	}

	sendPaginated(w, result, total, pp)
}

func (o *Observer) handleCandidateBacklog(w http.ResponseWriter, r *http.Request) {
	platform := r.URL.Query().Get("platform")
	if platform == "" {
		platform = "x"
	}
	db, err := o.openPlatformDB(platform)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	defer db.Close()

	pp := parsePage(r)

	var total int
	db.QueryRow("SELECT COUNT(*) FROM reply_candidate_backlog WHERE status = 'pending'").Scan(&total)

	rows, err := db.Query(`
		SELECT tweet_id, author, text, likes, views, score, status,
		       skip_reason, discovered_at, created_at
		FROM reply_candidate_backlog
		WHERE status = 'pending'
		ORDER BY score DESC
		LIMIT ? OFFSET ?
	`, pp.PerPage, pp.Offset)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var tweetID, author string
		var text, skipReason, discoveredAt, createdAt sql.NullString
		var likes, views int
		var score float64
		var status string
		if err := rows.Scan(&tweetID, &author, &text, &likes, &views, &score, &status,
			&skipReason, &discoveredAt, &createdAt); err != nil {
			slog.Error("scan candidate backlog row", "error", err)
			continue
		}
		result = append(result, map[string]any{
			"tweet_id":      tweetID,
			"author":        author,
			"text":          truncate(nullStr(text), 120),
			"likes":         likes,
			"views":         views,
			"score":         score,
			"status":        status,
			"skip_reason":   nullStr(skipReason),
			"discovered_at": nullStr(discoveredAt),
			"created_at":    nullStr(createdAt),
		})
	}
	if result == nil {
		result = []map[string]any{}
	}

	sendPaginated(w, result, total, pp)
}

func (o *Observer) handleReplyReviews(w http.ResponseWriter, r *http.Request) {
	platform := r.URL.Query().Get("platform")
	if platform == "" {
		platform = "x"
	}
	db, err := o.openPlatformDB(platform)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	defer db.Close()

	if !platformHasTable(db, "reply_review_queue") {
		sendPaginated(w, []map[string]any{}, 0, parsePage(r))
		return
	}

	pp := parsePage(r)
	status := r.URL.Query().Get("status")
	where := "WHERE status IN ('pending', 'needs_human', 'approved')"
	args := []any{}
	if status != "" && status != "active" {
		where = "WHERE status = ?"
		args = append(args, status)
	}

	var total int
	countArgs := append([]any{}, args...)
	if err := db.QueryRow("SELECT COUNT(*) FROM reply_review_queue "+where, countArgs...).Scan(&total); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	args = append(args, pp.PerPage, pp.Offset)
	rows, err := db.Query(`
		SELECT id, parent_id, author, parent_text, reply_text, status,
		       reviewer_type, reviewer_id, reject_reason, reply_id,
		       created_at, reviewed_at, expires_at
		FROM reply_review_queue
		`+where+`
		ORDER BY
		  CASE status WHEN 'needs_human' THEN 0 WHEN 'pending' THEN 1 WHEN 'approved' THEN 2 ELSE 3 END,
		  id DESC
		LIMIT ? OFFSET ?
	`, args...)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var id int
		var parentID, author, statusValue string
		var parentText, replyText, reviewerType, reviewerID, rejectReason, replyID, createdAt, reviewedAt, expiresAt sql.NullString
		if err := rows.Scan(&id, &parentID, &author, &parentText, &replyText, &statusValue,
			&reviewerType, &reviewerID, &rejectReason, &replyID, &createdAt, &reviewedAt, &expiresAt); err != nil {
			slog.Error("scan reply review row", "error", err)
			continue
		}
		result = append(result, map[string]any{
			"id":            id,
			"parent_id":     parentID,
			"author":        author,
			"parent_text":   truncate(nullStr(parentText), 160),
			"reply_text":    truncate(nullStr(replyText), 160),
			"status":        statusValue,
			"reviewer_type": nullStr(reviewerType),
			"reviewer_id":   nullStr(reviewerID),
			"reject_reason": nullStr(rejectReason),
			"reply_id":      nullStr(replyID),
			"created_at":    nullStr(createdAt),
			"reviewed_at":   nullStr(reviewedAt),
			"expires_at":    nullStr(expiresAt),
		})
	}
	if result == nil {
		result = []map[string]any{}
	}

	sendPaginated(w, result, total, pp)
}

func (o *Observer) handleTracking(w http.ResponseWriter, r *http.Request) {
	platform := r.URL.Query().Get("platform")
	if platform == "" {
		platform = "x"
	}
	db, err := o.openPlatformDB(platform)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	defer db.Close()

	pp := parsePage(r)

	var total int
	db.QueryRow("SELECT COUNT(*) FROM published_item_tracking WHERE likes > 0 OR views > 0").Scan(&total)

	selectionScoreExpr := "NULL AS selection_score"
	if platformHasColumn(db, "published_item_tracking", "selection_score") {
		selectionScoreExpr = "selection_score"
	}
	candidateVRExpr := "NULL AS candidate_vr"
	if platformHasColumn(db, "published_item_tracking", "candidate_vr") {
		candidateVRExpr = "candidate_vr"
	}

	trackingQuery := fmt.Sprintf(`
		SELECT reply_id, parent_id, author, skeleton, reply_style,
		       score, %s, %s, likes, views, target_likes, target_views,
		       type, posted_at, check_stage, last_checked_at
		FROM published_item_tracking
		WHERE likes > 0 OR views > 0
		ORDER BY views DESC, likes DESC
		LIMIT ? OFFSET ?
	`, selectionScoreExpr, candidateVRExpr)

	rows, err := db.Query(trackingQuery, pp.PerPage, pp.Offset)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var replyID, parentID, author string
		var skeleton, replyStyle, replyType, postedAt, checkStage, lastChecked sql.NullString
		var score, selectionScore, candidateVR sql.NullFloat64
		var likes, views, targetLikes, targetViews int
		if err := rows.Scan(&replyID, &parentID, &author, &skeleton, &replyStyle,
			&score, &selectionScore, &candidateVR, &likes, &views, &targetLikes, &targetViews,
			&replyType, &postedAt, &checkStage, &lastChecked); err != nil {
			slog.Error("scan tracking row", "error", err)
			continue
		}
		result = append(result, map[string]any{
			"reply_id":        replyID,
			"parent_id":       parentID,
			"author":          author,
			"skeleton":        nullStr(skeleton),
			"reply_style":     nullStr(replyStyle),
			"score":           score.Float64,
			"selection_score": nullFloat(selectionScore),
			"candidate_vr":    nullFloat(candidateVR),
			"likes":           likes,
			"views":           views,
			"target_likes":    targetLikes,
			"target_views":    targetViews,
			"type":            nullStr(replyType),
			"posted_at":       nullStr(postedAt),
			"check_stage":     nullStr(checkStage),
			"last_checked_at": nullStr(lastChecked),
		})
	}
	if result == nil {
		result = []map[string]any{}
	}

	sendPaginated(w, result, total, pp)
}

func (o *Observer) handleRounds(w http.ResponseWriter, r *http.Request) {
	platform := r.URL.Query().Get("platform")
	if platform == "" {
		platform = "x"
	}
	db, err := o.openPlatformDB(platform)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	defer db.Close()

	pp := parsePage(r)

	var total int
	db.QueryRow("SELECT COUNT(*) FROM round_log").Scan(&total)

	rows, err := db.Query(`
		SELECT id, ts, mode, hot_topic, hot_pct,
		       foryou_total, foryou_new, following_total, following_new,
		       candidate_backlog_pending, replies_total, replies_cn, replies_en,
		       skeletons, skip_reasons, backlog_count,
		       quota_reply_used, quota_reply_limit, quota_hourly_used, quota_hourly_limit,
		       empty_reason
		FROM round_log
		ORDER BY ts DESC
		LIMIT ? OFFSET ?
	`, pp.PerPage, pp.Offset)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var id int
		var ts, mode string
		var hotTopic, skeletons, skipReasons, emptyReason sql.NullString
		var hotPct sql.NullFloat64
		var foryouTotal, foryouNew, followingTotal, followingNew int
		var candidateBacklogPending sql.NullInt64
		var repliesTotal, repliesCN, repliesEN, backlogCount int
		var quotaReplyUsed, quotaReplyLimit, quotaHourlyUsed, quotaHourlyLimit int
		if err := rows.Scan(&id, &ts, &mode, &hotTopic, &hotPct,
			&foryouTotal, &foryouNew, &followingTotal, &followingNew,
			&candidateBacklogPending, &repliesTotal, &repliesCN, &repliesEN,
			&skeletons, &skipReasons, &backlogCount,
			&quotaReplyUsed, &quotaReplyLimit, &quotaHourlyUsed, &quotaHourlyLimit,
			&emptyReason); err != nil {
			slog.Error("scan round row", "error", err)
			continue
		}
		result = append(result, map[string]any{
			"id":                        id,
			"ts":                        ts,
			"mode":                      mode,
			"hot_topic":                 nullStr(hotTopic),
			"hot_pct":                   hotPct.Float64,
			"foryou_total":              foryouTotal,
			"foryou_new":                foryouNew,
			"following_total":           followingTotal,
			"following_new":             followingNew,
			"candidate_backlog_pending": candidateBacklogPending.Int64,
			"replies_total":             repliesTotal,
			"replies_cn":                repliesCN,
			"replies_en":                repliesEN,
			"skeletons":                 nullStr(skeletons),
			"skip_reasons":              nullStr(skipReasons),
			"backlog_count":             backlogCount,
			"quota_reply_used":          quotaReplyUsed,
			"quota_reply_limit":         quotaReplyLimit,
			"quota_hourly_used":         quotaHourlyUsed,
			"quota_hourly_limit":        quotaHourlyLimit,
			"empty_reason":              nullStr(emptyReason),
		})
	}
	if result == nil {
		result = []map[string]any{}
	}

	sendPaginated(w, result, total, pp)
}

// --- Pagination ---

type pageParams struct {
	Page    int
	PerPage int
	Offset  int
}

func parsePage(r *http.Request) pageParams {
	p := pageParams{Page: 1, PerPage: 50}
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		p.Page = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && v > 0 && v <= 200 {
		p.PerPage = v
	}
	p.Offset = (p.Page - 1) * p.PerPage
	return p
}

type paginatedResponse struct {
	Data  any `json:"data"`
	Total int `json:"total"`
	Page  int `json:"page"`
	Pages int `json:"pages"`
}

func sendPaginated(w http.ResponseWriter, data any, total int, pp pageParams) {
	pages := (total + pp.PerPage - 1) / pp.PerPage
	if pages < 1 {
		pages = 1
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(paginatedResponse{
		Data:  data,
		Total: total,
		Page:  pp.Page,
		Pages: pages,
	})
}

// --- Helpers ---

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func nullStr(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}

func nullFloat(ns sql.NullFloat64) any {
	if ns.Valid {
		return ns.Float64
	}
	return nil
}

func nullInt(ns sql.NullInt64) any {
	if ns.Valid {
		return ns.Int64
	}
	return nil
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "…"
}

// --- Cost tracking ---

func (o *Observer) handleCosts(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}

	cfg := o.getConfig()
	report, err := ScanCosts(cfg, date, BuildPricingTable(cfg.Pricing))
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(report)
}

// --- Existing metrics ---

func validateIdentifier(s string) error {
	if !validIdentifier.MatchString(s) {
		return fmt.Errorf("invalid SQL identifier: %q", s)
	}
	return nil
}

func validateMetricsConfig(m MetricsConfig) error {
	identifiers := []string{
		m.CandidateBacklogTable, m.CandidateBacklogStatusField,
		m.ActionTable, m.ActionTypeField, m.ActionTimeField,
	}
	for _, id := range identifiers {
		if id != "" {
			if err := validateIdentifier(id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (o *Observer) queryPlatformMetrics(name string, p PlatformConfig) PlatformStatus {
	status := PlatformStatus{Health: "ok"}

	if err := validateMetricsConfig(p.Metrics); err != nil {
		slog.Error("invalid metrics config", "platform", name, "error", err)
		status.Health = "config_error"
		return status
	}

	db, err := sql.Open("sqlite", p.DB+"?mode=ro")
	if err != nil {
		slog.Error("open db", "platform", name, "error", err)
		status.Health = "db_error"
		return status
	}
	defer db.Close()

	// Candidate backlog pending count
	if p.Metrics.CandidateBacklogTable != "" {
		query := fmt.Sprintf(
			"SELECT COUNT(*) FROM %s WHERE %s = ?",
			p.Metrics.CandidateBacklogTable, p.Metrics.CandidateBacklogStatusField,
		)
		row := db.QueryRow(query, p.Metrics.CandidateBacklogPendingVal)
		if err := row.Scan(&status.CandidateBacklogPending); err != nil {
			slog.Error("query candidate backlog pending", "platform", name, "error", err)
			status.Health = "query_error"
		}
	}

	// Today's action count
	if p.Metrics.ActionTable != "" {
		today := time.Now().Format("2006-01-02")
		query := fmt.Sprintf(
			"SELECT COUNT(*) FROM %s WHERE date(%s) = ?",
			p.Metrics.ActionTable, p.Metrics.ActionTimeField,
		)
		row := db.QueryRow(query, today)
		if err := row.Scan(&status.TodayActions); err != nil {
			slog.Error("query today actions", "platform", name, "error", err)
			status.Health = "query_error"
		}

		// Action breakdown by type
		breakdownQuery := fmt.Sprintf(
			"SELECT %s, COUNT(*) FROM %s WHERE date(%s) = ? GROUP BY %s",
			p.Metrics.ActionTypeField, p.Metrics.ActionTable,
			p.Metrics.ActionTimeField, p.Metrics.ActionTypeField,
		)
		rows, err := db.Query(breakdownQuery, today)
		if err != nil {
			slog.Error("query action breakdown", "platform", name, "error", err)
		} else {
			defer rows.Close()
			status.ActionBreakdown = make(map[string]int)
			for rows.Next() {
				var actionType string
				var count int
				if err := rows.Scan(&actionType, &count); err != nil {
					slog.Error("scan action breakdown row", "platform", name, "error", err)
					continue
				}
				status.ActionBreakdown[actionType] = count
			}
		}
	}

	return status
}
