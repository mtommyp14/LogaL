package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	_ "embed"
)

//go:embed ui.html
var uiHTML []byte

// ── Config ────────────────────────────────────────────────────────────────────

var (
	port              = getEnv("PORT", "8080")
	kubeconfig        = getEnv("KUBECONFIG", "")
	databaseURL       = getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/logal")
	retentionDays     = getEnvInt("LOG_RETENTION_DAYS", 3)
	allowedNamespaces = getEnv("ALLOWED_NAMESPACES", "")
)

var dbPool *pgxpool.Pool

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

// ── Known sidecars (default OFF in container selector) ────────────────────────

var knownSidecars = map[string]bool{
	"istio-proxy": true, "envoy": true, "sidecar": true,
	"filebeat": true, "fluentd": true, "fluent-bit": true, "logstash": true,
	"datadog-agent": true, "newrelic": true, "vault-agent": true,
	"linkerd-proxy": true, "jaeger-agent": true, "otel-collector": true,
	"apm-agent-java": true, "apm-agent": true, "elastic-agent": true,
	"init": true, "init-container": true,
}

// isSidecarContainer checks both exact match and common sidecar patterns
func isSidecarContainer(name string) bool {
	if knownSidecars[name] {
		return true
	}
	lower := strings.ToLower(name)
	for _, pattern := range []string{"apm-agent", "init-", "-sidecar", "-agent", "filebeat", "fluentd"} {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// ── Kubectl helper ────────────────────────────────────────────────────────────

func kubectl(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	if kubeconfig != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	}
	return cmd.Output()
}

// ── API: contexts ─────────────────────────────────────────────────────────────

func handleContexts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	args := []string{"config", "get-contexts", "-o", "name"}
	out, err := kubectl(ctx, args...)
	if err != nil {
		// in-cluster fallback
		jsonResp(w, []string{"in-cluster"})
		return
	}
	var contexts []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			contexts = append(contexts, line)
		}
	}
	if len(contexts) == 0 {
		contexts = []string{"in-cluster"}
	}
	jsonResp(w, contexts)
}

// ── API: namespaces ───────────────────────────────────────────────────────────

func handleNamespaces(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ctxName := r.URL.Query().Get("ctx")

	args := []string{"get", "namespaces", "-o", "jsonpath={.items[*].metadata.name}"}
	if ctxName != "" && ctxName != "in-cluster" {
		args = append([]string{"--context", ctxName}, args...)
	}

	out, err := kubectl(ctx, args...)
	if err != nil {
		http.Error(w, "failed to list namespaces", http.StatusInternalServerError)
		return
	}

	all := strings.Fields(string(out))
	sort.Strings(all)

	if allowedNamespaces != "" {
		allowed := map[string]bool{}
		for _, ns := range strings.Split(allowedNamespaces, ",") {
			allowed[strings.TrimSpace(ns)] = true
		}
		var filtered []string
		for _, ns := range all {
			if allowed[ns] {
				filtered = append(filtered, ns)
			}
		}
		all = filtered
	}

	jsonResp(w, all)
}

// ── API: workloads ────────────────────────────────────────────────────────────

type Workload struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

func handleWorkloads(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ctxName := r.URL.Query().Get("ctx")
	ns := r.URL.Query().Get("ns")
	if ns == "" {
		http.Error(w, "ns required", http.StatusBadRequest)
		return
	}

	var result []Workload
	var mu sync.Mutex
	var wg sync.WaitGroup

	kinds := []string{"deployments", "statefulsets", "daemonsets"}
	for _, kind := range kinds {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			args := []string{"get", k, "-n", ns, "-o", "jsonpath={.items[*].metadata.name}"}
			if ctxName != "" && ctxName != "in-cluster" {
				args = append([]string{"--context", ctxName}, args...)
			}
			out, err := kubectl(ctx, args...)
			if err != nil {
				return
			}
			shortKind := map[string]string{
				"deployments":  "Deployment",
				"statefulsets": "StatefulSet",
				"daemonsets":   "DaemonSet",
			}[k]
			mu.Lock()
			for _, name := range strings.Fields(string(out)) {
				result = append(result, Workload{Kind: shortKind, Name: name})
			}
			mu.Unlock()
		}(kind)
	}
	wg.Wait()

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	jsonResp(w, result)
}

// ── API: pods ─────────────────────────────────────────────────────────────────

type PodInfo struct {
	Name       string   `json:"name"`
	Containers []string `json:"containers"`
	IsSidecar  []bool   `json:"is_sidecar"`
}

func handlePods(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ctxName := r.URL.Query().Get("ctx")
	ns := r.URL.Query().Get("ns")
	workload := r.URL.Query().Get("workload")
	kind := r.URL.Query().Get("kind")

	if ns == "" || workload == "" {
		http.Error(w, "ns and workload required", http.StatusBadRequest)
		return
	}

	// Get pod label selector from workload
	var labelSelector string
	{
		resType := strings.ToLower(kind)
		if resType == "" {
			resType = "deployment"
		}
		args := []string{"get", resType, workload, "-n", ns,
			"-o", "jsonpath={.spec.selector.matchLabels}"}
		if ctxName != "" && ctxName != "in-cluster" {
			args = append([]string{"--context", ctxName}, args...)
		}
		out, err := kubectl(ctx, args...)
		if err == nil {
			var labels map[string]string
			if json.Unmarshal(out, &labels) == nil {
				var parts []string
				for k, v := range labels {
					parts = append(parts, k+"="+v)
				}
				labelSelector = strings.Join(parts, ",")
			}
		}
	}

	// List pods with both regular and init containers
	// jsonpath format: name\tcontainers\tinitContainers
	args := []string{"get", "pods", "-n", ns,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\t"}{range .spec.containers[*]}{.name}{" "}{end}{"\t"}{range .spec.initContainers[*]}{.name}{" "}{end}{"\n"}{end}`}
	if labelSelector != "" {
		args = append(args, "-l", labelSelector)
	} else {
		// No label selector means workload not found as a k8s resource.
		// Use field-selector by pod name prefix to avoid listing all pods.
		args = append(args, "--field-selector", "metadata.name="+workload)
	}
	if ctxName != "" && ctxName != "in-cluster" {
		args = append([]string{"--context", ctxName}, args...)
	}

	out, err := kubectl(ctx, args...)
	if err != nil {
		http.Error(w, "failed to list pods", http.StatusInternalServerError)
		return
	}

	var pods []PodInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			continue
		}
		podName := parts[0]
		// Combine regular containers + init containers
		regularContainers := strings.Fields(parts[1])
		var initContainers []string
		if len(parts) == 3 {
			initContainers = strings.Fields(parts[2])
		}
		containers := append(regularContainers, initContainers...)
		isSidecar := make([]bool, len(containers))
		// Mark containers as sidecars (init containers always sidecar)
		for i, c := range containers {
			if i >= len(regularContainers) {
				isSidecar[i] = true // init containers always treated as sidecar
			} else {
				isSidecar[i] = isSidecarContainer(c)
			}
		}
		pods = append(pods, PodInfo{
			Name:       podName,
			Containers: containers,
			IsSidecar:  isSidecar,
		})
	}

	jsonResp(w, pods)
}

// ── API: pod age ──────────────────────────────────────────────────────────────

type PodAge struct {
	PodName      string `json:"pod_name"`
	StartTime    string `json:"start_time"`
	AgeSeconds   int64  `json:"age_seconds"`
	AgeHuman     string `json:"age_human"`
	MaxHistoryH  int    `json:"max_history_hours"`
	HasFileCache bool   `json:"has_file_cache"`
}

func handlePodAge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ctxName := r.URL.Query().Get("ctx")
	ns := r.URL.Query().Get("ns")
	workload := r.URL.Query().Get("workload")

	args := []string{"get", "pods", "-n", ns,
		"-o", "jsonpath={.items[0].metadata.name}{\"\\t\"}{.items[0].status.startTime}"}
	if ctxName != "" && ctxName != "in-cluster" {
		args = append([]string{"--context", ctxName}, args...)
	}

	out, err := kubectl(ctx, args...)
	if err != nil {
		http.Error(w, "failed to get pod age", http.StatusInternalServerError)
		return
	}

	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(parts) < 2 {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}

	podName := parts[0]
	startTime, err := time.Parse(time.RFC3339, parts[1])
	if err != nil {
		http.Error(w, "failed to parse start time", http.StatusInternalServerError)
		return
	}

	ageSeconds := int64(time.Since(startTime).Seconds())
	ageHours := int(ageSeconds / 3600)
	ageMinutes := int((ageSeconds % 3600) / 60)

	ageHuman := fmt.Sprintf("%dj %dm", ageHours, ageMinutes)
	if ageHours == 0 {
		ageHuman = fmt.Sprintf("%dm", ageMinutes)
	}

	maxH := ageHours
	if maxH > retentionDays*24 {
		maxH = retentionDays * 24
	}

	// Check if database history exists for this workload
	var hasCache bool
	_ = dbPool.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM logs WHERE cluster=$1 AND namespace=$2 AND workload=$3 LIMIT 1)`,
		ctxName, ns, workload).Scan(&hasCache)

	jsonResp(w, PodAge{
		PodName:      podName,
		StartTime:    startTime.Format(time.RFC3339),
		AgeSeconds:   ageSeconds,
		AgeHuman:     ageHuman,
		MaxHistoryH:  maxH,
		HasFileCache: hasCache,
	})
}

// ── Database / PostgreSQL layer ───────────────────────────────────────────────

type LogLine struct {
	Cluster   string
	Namespace string
	Workload  string
	Pod       string
	Container string
	Level     string
	Ts        time.Time
	Message   string
}

// formatLogLine returns the same plain-text format that was previously stored in flat files.
// Format: "2006-01-02T15:04:05Z [pod][container] message"
func formatLogLine(l LogLine) string {
	return fmt.Sprintf("%s [%s][%s] %s",
		l.Ts.UTC().Format("2006-01-02T15:04:05Z"),
		l.Pod,
		l.Container,
		l.Message,
	)
}

// parseLogLine parses a formatted log line back into structured fields for storage.
func parseLogLine(line string) (LogLine, bool) {
	var l LogLine
	spaceIdx := strings.IndexByte(line, ' ')
	if spaceIdx <= 0 {
		return l, false
	}
	ts, err := time.Parse("2006-01-02T15:04:05Z", line[:spaceIdx])
	if err != nil {
		return l, false
	}
	l.Ts = ts.UTC()

	rest := line[spaceIdx+1:]
	// Parse [pod][container] prefix
	if !strings.HasPrefix(rest, "[") {
		return l, false
	}
	endPod := strings.IndexByte(rest, ']')
	if endPod <= 0 {
		return l, false
	}
	l.Pod = rest[1:endPod]
	rest = rest[endPod+1:]

	if !strings.HasPrefix(rest, "[") {
		return l, false
	}
	endContainer := strings.IndexByte(rest, ']')
	if endContainer <= 0 {
		return l, false
	}
	l.Container = rest[1:endContainer]
	l.Message = strings.TrimLeft(rest[endContainer+1:], " ")
	return l, true
}

func initDB(ctx context.Context) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}
	_, err = pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS logs (
			id          BIGSERIAL PRIMARY KEY,
			cluster     TEXT NOT NULL,
			namespace   TEXT NOT NULL,
			workload    TEXT NOT NULL,
			pod         TEXT NOT NULL,
			container   TEXT NOT NULL,
			level       TEXT,
			ts          TIMESTAMPTZ NOT NULL,
			message     TEXT NOT NULL,
			created_at  TIMESTAMPTZ DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_logs_lookup ON logs(cluster, namespace, workload, ts);
		CREATE INDEX IF NOT EXISTS idx_logs_ts ON logs(ts);
		CREATE INDEX IF NOT EXISTS idx_logs_message ON logs USING gin(to_tsvector('english', message));
	`)
	if err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return pool, nil
}

func insertLogLine(ctx context.Context, l LogLine) error {
	_, err := dbPool.Exec(ctx, `
		INSERT INTO logs (cluster, namespace, workload, pod, container, level, ts, message)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, l.Cluster, l.Namespace, l.Workload, l.Pod, l.Container, l.Level, l.Ts.UTC(), l.Message)
	return err
}

// streamHistoryFiles streams historical log lines from PostgreSQL for the given date range.
// Output lines use the same plain-text format as the original flat files.
func streamHistoryFiles(ctx context.Context, cluster, ns, workload string, since time.Time, until *time.Time, filter string, out chan<- string) {
	args := []interface{}{cluster, ns, workload, since.UTC()}
	sql := `SELECT ts, pod, container, level, message FROM logs
	        WHERE cluster=$1 AND namespace=$2 AND workload=$3 AND ts >= $4`
	if until != nil {
		sql += ` AND ts <= $5`
		args = append(args, until.UTC())
	}
	if filter != "" {
		sql += fmt.Sprintf(` AND message ILIKE '%%' || $%d || '%%'`, len(args)+1)
		args = append(args, filter)
	}
	sql += ` ORDER BY ts ASC`

	rows, err := dbPool.Query(ctx, sql, args...)
	if err != nil {
		log.Printf("[history] query error: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		var l LogLine
		l.Cluster = cluster
		l.Namespace = ns
		l.Workload = workload
		if err := rows.Scan(&l.Ts, &l.Pod, &l.Container, &l.Level, &l.Message); err != nil {
			continue
		}
		out <- formatLogLine(l)
	}
}

// ── Cleanup goroutine ─────────────────────────────────────────────────────────

func startCleanup() {
	go func() {
		for {
			cleanOldLogs()
			time.Sleep(1 * time.Hour)
		}
	}()
}

func cleanOldLogs() {
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
	_, err := dbPool.Exec(context.Background(),
		`DELETE FROM logs WHERE ts < $1`, cutoff)
	if err != nil {
		log.Printf("[cleanup] delete old logs error: %v", err)
	}
}

// ── SSE log streaming ─────────────────────────────────────────────────────────

func handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ctxName := q.Get("ctx")
	ns := q.Get("ns")
	workload := q.Get("workload")
	filter := q.Get("filter")
	sinceStr := q.Get("since")        // e.g. "30m", "1h", "2d", "realtime"
	fromStr  := q.Get("from")         // ISO8601 custom range start
	toStr    := q.Get("to")           // ISO8601 custom range end
	pods := q.Get("pods")             // comma-separated pod names
	containers := q.Get("containers") // comma-separated container names

	if ns == "" || workload == "" {
		http.Error(w, "ns and workload required", http.StatusBadRequest)
		return
	}

	// SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	logCh := make(chan string, 500)

	// Parse since duration or custom range
	var sinceTime *time.Time
	var toTime *time.Time
	isCustomRange := fromStr != ""
	isRealtime := !isCustomRange && (sinceStr == "" || sinceStr == "realtime")

	if isCustomRange {
		// UI sends toISOString() which includes milliseconds: "2026-06-19T09:21:00.000Z"
		// Try RFC3339Nano first, then RFC3339
		parseTime := func(s string) (time.Time, error) {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return t.UTC(), nil
			}
			t, err := time.Parse(time.RFC3339, s)
			return t.UTC(), err
		}
		if t, err := parseTime(fromStr); err == nil {
			sinceTime = &t
		}
		if toStr != "" {
			if t2, err := parseTime(toStr); err == nil {
				toTime = &t2
			}
		}
	} else if !isRealtime {
		dur := parseSince(sinceStr)
		if dur > 0 {
			t := time.Now().Add(-dur)
			sinceTime = &t
		}
	}

	// Stream history from flat files first
	if sinceTime != nil {
		if isCustomRange && toTime != nil {
			// Custom range: stream history (with exact time bounds) then send __END__, no stern
			go func() {
				streamHistoryFiles(ctx, ctxName, ns, workload, *sinceTime, toTime, filter, logCh)
				// Safely send end sentinel (ctx may already be done)
				select {
				case logCh <- `{"__end__":true}`:
				case <-ctx.Done():
				}
			}()
			goto streamLoop
		}
		go func() {
			streamHistoryFiles(ctx, ctxName, ns, workload, *sinceTime, nil, filter, logCh)
		}()
	}

	// Build stern command for real-time
	go func() {
		// Small delay if we're also streaming history, to let it start first
		if sinceTime != nil {
			time.Sleep(100 * time.Millisecond)
		}

		// Build pod pattern: workload name as prefix regex (matches all pods in deployment)
		// If specific pods selected, use exact pod names as regex OR pattern
		podPattern := workload
		if pods != "" {
			podList := []string{}
			for _, pod := range strings.Split(pods, ",") {
				pod = strings.TrimSpace(pod)
				if pod != "" {
					podList = append(podList, regexp.QuoteMeta(pod))
				}
			}
			if len(podList) > 0 {
				podPattern = strings.Join(podList, "|")
			}
		}

		args := []string{podPattern, "--namespace", ns, "--output", "json", "--timestamps=default"}

		if ctxName != "" && ctxName != "in-cluster" {
			args = append(args, "--context", ctxName)
		}
		if kubeconfig != "" {
			args = append(args, "--kubeconfig", kubeconfig)
		}
		if filter != "" {
			args = append(args, "--grep", filter)
		}

		// Don't pass --since or --tail: stern default is --since=48h which keeps
		// it alive streaming live logs after printing recent history.
		// Only pass --since if user picked a specific time range, converting
		// "Nd" days format to hours since stern doesn't support "d" unit.
		if sinceStr != "" && sinceStr != "realtime" {
			sternSince := toSternDuration(sinceStr)
			if sternSince != "" {
				args = append(args, "--since", sternSince)
			}
		}

		// Filter specific containers
		if containers != "" {
			containerList := []string{}
			for _, c := range strings.Split(containers, ",") {
				c = strings.TrimSpace(c)
				if c != "" {
					containerList = append(containerList, regexp.QuoteMeta(c))
				}
			}
			if len(containerList) > 0 {
				args = append(args, "--container", strings.Join(containerList, "|"))
			}
		}

		cmd := exec.CommandContext(ctx, "stern", args...)
		// Always inherit full environment; add/override KUBECONFIG if specified
		cmd.Env = os.Environ()
		if kubeconfig != "" {
			cmd.Env = append(cmd.Env, "KUBECONFIG="+kubeconfig)
		}

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			logCh <- `{"error":"failed to start stern: ` + err.Error() + `"}`
			return
		}
		stderr, _ := cmd.StderrPipe()
		// Log stern stderr so we can see errors
		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				log.Printf("[stern stderr] %s", scanner.Text())
			}
		}()

		if err := cmd.Start(); err != nil {
			logCh <- `{"error":"failed to start stern: ` + err.Error() + `"}`
			return
		}

		log.Printf("[stern] started: %v", args)

		// Ensure stern is killed when context is cancelled (client disconnect)
		go func() {
			<-ctx.Done()
			if cmd.Process != nil {
				log.Printf("[stern] killing pid %d", cmd.Process.Pid)
				cmd.Process.Kill()
			}
		}()

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}
			line := scanner.Text()
			// Skip non-JSON lines (e.g. stern info lines like "+ pod › container")
			if len(line) == 0 || line[0] != '{' {
				log.Printf("[stern] skip non-json: %s", line)
				continue
			}
			var sl SternLine
			if err := json.Unmarshal([]byte(line), &sl); err != nil {
				log.Printf("[stern] parse error: %v | line: %s", err, line)
				continue
			}
			log.Printf("[stern] got line: pod=%s container=%s", sl.PodName, sl.ContainerName)
			logLine := formatParsedSternLine(sl)
			if logLine.Message != "" || logLine.Pod != "" {
				logLine.Cluster = ctxName
				logLine.Namespace = ns
				logLine.Workload = workload
				if err := insertLogLine(ctx, logLine); err != nil {
					log.Printf("[db] insert error: %v", err)
				}
				logCh <- formatLogLine(logLine)
			}
		}
		log.Printf("[stern] scanner done: %v", scanner.Err())
	}()

	// Send SSE to browser
streamLoop:
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-logCh:
			if !ok {
				return
			}
			// End sentinel from custom range history
			if line == `{"__end__":true}` {
				fmt.Fprintf(w, "data: __END__\n\n")
				flusher.Flush()
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", jsonEscape(line))
			flusher.Flush()
		}
	}
}

// ── Stern JSON parser ─────────────────────────────────────────────────────────

type SternLine struct {
	Message       string `json:"message"`
	NodeName      string `json:"nodeName"`
	Namespace     string `json:"namespace"`
	PodName       string `json:"podName"`
	ContainerName string `json:"containerName"`
	Timestamp     string `json:"timestamp"`
}

func formatSternLine(raw string) string {
	var sl SternLine
	if err := json.Unmarshal([]byte(raw), &sl); err != nil {
		return ""
	}
	l := formatParsedSternLine(sl)
	return formatLogLine(l)
}

func formatParsedSternLine(sl SternLine) LogLine {
	// Stern --timestamps puts timestamp inside the message field itself
	// message format: "2026-06-19T13:00:56Z actual log message"
	// We extract it and use podName/containerName from JSON fields
	msg := strings.TrimRight(sl.Message, "\n")
	l := LogLine{
		Pod:       sl.PodName,
		Container: sl.ContainerName,
	}

	// Extract leading timestamp from message if present
	if parts := strings.SplitN(msg, " ", 2); len(parts) == 2 {
		if t, err := time.Parse(time.RFC3339Nano, parts[0]); err == nil {
			l.Ts = t.UTC()
			msg = parts[1]
		}
	}
	if l.Ts.IsZero() {
		l.Ts = time.Now().UTC()
	}

	l.Message = msg

	// Detect log level from message prefix if present
	msgLower := strings.ToLower(msg)
	switch {
	case strings.Contains(msgLower, "error"), strings.Contains(msgLower, "err"):
		l.Level = "ERROR"
	case strings.Contains(msgLower, "warn"), strings.Contains(msgLower, "warning"):
		l.Level = "WARN"
	case strings.Contains(msgLower, "info"):
		l.Level = "INFO"
	case strings.Contains(msgLower, "debug"):
		l.Level = "DEBUG"
	}
	return l
}

// ── API: history files list ───────────────────────────────────────────────────

type HistoryInfo struct {
	Dates     []string `json:"dates"`
	Available bool     `json:"available"`
}

func handleHistoryInfo(w http.ResponseWriter, r *http.Request) {
	ctxName := r.URL.Query().Get("ctx")
	ns := r.URL.Query().Get("ns")
	workload := r.URL.Query().Get("workload")

	rows, err := dbPool.Query(r.Context(),
		`SELECT DISTINCT ts::date FROM logs
		 WHERE cluster=$1 AND namespace=$2 AND workload=$3
		 ORDER BY ts::date ASC`,
		ctxName, ns, workload)
	if err != nil {
		jsonResp(w, HistoryInfo{Available: false})
		return
	}
	defer rows.Close()

	var dates []string
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err == nil {
			dates = append(dates, d.Format("2006-01-02"))
		}
	}

	jsonResp(w, HistoryInfo{Dates: dates, Available: len(dates) > 0})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// toSternDuration converts a since string to a format stern understands.
// stern only supports Go duration units (s, m, h) — not "d" for days.
func toSternDuration(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err == nil {
			return fmt.Sprintf("%dh", n*24)
		}
	}
	// already valid Go duration (e.g. "1h", "30m")
	if _, err := time.ParseDuration(s); err == nil {
		return s
	}
	return ""
}

func parseSince(s string) time.Duration {
	s = strings.TrimSpace(strings.ToLower(s))
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err == nil {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1]) // strip surrounding quotes
}

func jsonResp(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ── API: container logs (for terminated/init containers) ──────────────────────

func handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ctxName   := r.URL.Query().Get("ctx")
	ns        := r.URL.Query().Get("ns")
	pod       := r.URL.Query().Get("pod")
	container := r.URL.Query().Get("container")
	isInit    := r.URL.Query().Get("init") == "true"

	if ns == "" || pod == "" || container == "" {
		http.Error(w, "ns, pod, container required", http.StatusBadRequest)
		return
	}

	args := []string{"logs", pod, "-n", ns, "-c", container, "--timestamps"}
	if isInit {
		args = append(args, "--previous=false") // init containers don't need --previous
	}
	if ctxName != "" && ctxName != "in-cluster" {
		args = append([]string{"--context", ctxName}, args...)
	}

	out, err := kubectl(ctx, args...)
	if err != nil {
		// Try with --previous for terminated containers
		args2 := append(args, "--previous")
		out, err = kubectl(ctx, args2...)
		if err != nil {
			http.Error(w, "failed to get logs: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	lines := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"pod":       pod,
		"container": container,
		"lines":     lines,
		"total":     len(lines),
	})
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	ctx := context.Background()

	// Connect to PostgreSQL
	pool, err := initDB(ctx)
	if err != nil {
		log.Fatalf("failed to initialize database: %v", err)
	}
	dbPool = pool
	defer dbPool.Close()

	// Start cleanup goroutine
	startCleanup()

	mux := http.NewServeMux()

	// UI
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(uiHTML)
	})

	// API
	mux.HandleFunc("/api/contexts", handleContexts)
	mux.HandleFunc("/api/namespaces", handleNamespaces)
	mux.HandleFunc("/api/workloads", handleWorkloads)
	mux.HandleFunc("/api/pods", handlePods)
	mux.HandleFunc("/api/pod-age", handlePodAge)
	mux.HandleFunc("/api/logs", handleLogs)
	mux.HandleFunc("/api/history-info", handleHistoryInfo)
	mux.HandleFunc("/api/container-logs", handleContainerLogs)

	log.Printf("LogaL starting on :%s", port)
	log.Printf("Database: %s (retention: %d days)", databaseURL, retentionDays)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
