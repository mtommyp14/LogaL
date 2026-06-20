package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	podName           = getEnv("POD_NAME", "logal-local") // used for leader election
)

var dbPool *pgxpool.Pool

// isLeader tracks whether this pod currently holds the cluster-wide collector lock.
var isLeader atomic.Bool

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

		CREATE TABLE IF NOT EXISTS leader_lock (
			lock_id      INT PRIMARY KEY,
			holder       TEXT NOT NULL,
			acquired_at  TIMESTAMPTZ NOT NULL,
			expires_at   TIMESTAMPTZ NOT NULL
		);
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

// ── Leader election ───────────────────────────────────────────────────────────

const leaderLockID = 424242

// tryAcquireLeader tries to become the active collector pod using a DB-backed lease.
// Returns true if this pod successfully acquired or renewed the lease.
func tryAcquireLeader(ctx context.Context) (bool, error) {
	leaseDuration := 15 * time.Second

	res, err := dbPool.Exec(ctx, `
		INSERT INTO leader_lock (lock_id, holder, acquired_at, expires_at)
		VALUES ($1, $2, now(), now() + $3::interval)
		ON CONFLICT (lock_id) DO UPDATE
		SET holder = EXCLUDED.holder,
		    acquired_at = EXCLUDED.acquired_at,
		    expires_at = EXCLUDED.expires_at
		WHERE leader_lock.expires_at < now()
		   OR leader_lock.holder = $2
	`, leaderLockID, podName, fmt.Sprintf("%f seconds", leaseDuration.Seconds()))
	if err != nil {
		return false, err
	}
	return res.RowsAffected() > 0, nil
}

// releaseLeader voluntarily gives up the leader lease.
func releaseLeader(ctx context.Context) {
	_, _ = dbPool.Exec(ctx, `
		DELETE FROM leader_lock WHERE lock_id = $1 AND holder = $2
	`, leaderLockID, podName)
}

// runLeaderElection keeps trying to acquire/renew the leader lease.
// When this pod becomes leader, it starts the global log collector.
// When it loses leadership, it stops the collector.
func runLeaderElection(ctx context.Context) {
	const tick = 5 * time.Second
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	var collectorCancel context.CancelFunc

	for {
		acquired, err := tryAcquireLeader(ctx)
		if err != nil {
			log.Printf("[leader] acquire error: %v", err)
			isLeader.Store(false)
		} else if acquired {
			if !isLeader.Load() {
				log.Printf("[leader] %s became leader", podName)
				isLeader.Store(true)
				cctx, cancel := context.WithCancel(ctx)
				collectorCancel = cancel
				go runLogCollector(cctx)
			}
		} else {
			if isLeader.Load() {
				log.Printf("[leader] %s lost leadership", podName)
				isLeader.Store(false)
				if collectorCancel != nil {
					collectorCancel()
					collectorCancel = nil
				}
			}
		}

		select {
		case <-ctx.Done():
			if isLeader.Load() {
				isLeader.Store(false)
				releaseLeader(ctx)
				if collectorCancel != nil {
					collectorCancel()
				}
			}
			return
		case <-ticker.C:
		}
	}
}

// ── Global log collector ────────────────────────────────────────────────────

// podKey uniquely identifies a pod in the cluster.
type podKey struct {
	Namespace string
	Name      string
}

// containerKey uniquely identifies a container inside a pod.
type containerKey struct {
	podKey
	Container string
}

// podContainerState holds the cancel function for a running container stream.
type collectorState struct {
	mu       sync.Mutex
	streams  map[containerKey]context.CancelFunc
	podNames map[podKey]bool
}

// runLogCollector is the leader-only background loop that watches all pods
// and streams every container's logs into PostgreSQL.
func runLogCollector(ctx context.Context) {
	state := &collectorState{
		streams:  make(map[containerKey]context.CancelFunc),
		podNames: make(map[podKey]bool),
	}

	for {
		select {
		case <-ctx.Done():
			state.stopAll()
			return
		default:
		}
		log.Printf("[collector] starting pod watcher")
		watchPods(ctx, state)
		log.Printf("[collector] pod watcher ended, retrying in 5s")
		select {
		case <-ctx.Done():
			state.stopAll()
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// watchPods runs `kubectl get pods --all-namespaces --watch` and keeps log
// streams alive for every running pod/container.
func watchPods(ctx context.Context, state *collectorState) {
	args := []string{"get", "pods", "--all-namespaces", "--watch", "-o", "json"}
	if kubeconfig != "" {
		args = append(args, "--kubeconfig", kubeconfig)
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Env = os.Environ()
	if kubeconfig != "" {
		cmd.Env = append(cmd.Env, "KUBECONFIG="+kubeconfig)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[collector] stdout pipe error: %v", err)
		return
	}
	stderr, _ := cmd.StderrPipe()

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("[collector stderr] %s", scanner.Text())
		}
	}()

	if err := cmd.Start(); err != nil {
		log.Printf("[collector] start error: %v", err)
		return
	}
	defer func() {
		_ = cmd.Wait()
	}()

	decoder := json.NewDecoder(stdout)
	for {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
			return
		default:
		}

		var event podWatchEvent
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				return
			}
			log.Printf("[collector] decode error: %v", err)
			return
		}
		state.handlePodEvent(ctx, event)
	}
}

// podWatchEvent mirrors the fields we need from a kubectl watch event.
type podWatchEvent struct {
	Type   string `json:"type"`
	Object struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Containers     []struct {
				Name string `json:"name"`
			} `json:"containers"`
			InitContainers []struct {
				Name string `json:"name"`
			} `json:"initContainers"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	} `json:"object"`
}

func (s *collectorState) handlePodEvent(ctx context.Context, e podWatchEvent) {
	ns := e.Object.Metadata.Namespace
	pod := e.Object.Metadata.Name
	phase := e.Object.Status.Phase
	key := podKey{Namespace: ns, Name: pod}

	// Ignore pods that are not yet running or already terminating.
	if phase != "Running" && phase != "Pending" {
		// For delete events, stop immediately.
		if e.Type == "DELETED" {
			s.stopPod(key)
		}
		return
	}

	if e.Type == "DELETED" {
		s.stopPod(key)
		return
	}

	// Collect regular + init containers.
	var containers []string
	for _, c := range e.Object.Spec.Containers {
		containers = append(containers, c.Name)
	}
	for _, c := range e.Object.Spec.InitContainers {
		containers = append(containers, c.Name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.podNames[key] = true

	for _, c := range containers {
		ck := containerKey{podKey: key, Container: c}
		if _, ok := s.streams[ck]; ok {
			continue
		}
		cctx, cancel := context.WithCancel(ctx)
		s.streams[ck] = cancel
		go streamContainerLogsForCollector(cctx, ns, pod, c)
		log.Printf("[collector] started %s/%s/%s", ns, pod, c)
	}
}

func (s *collectorState) stopPod(key podKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.podNames, key)
	for ck, cancel := range s.streams {
		if ck.podKey == key {
			cancel()
			delete(s.streams, ck)
			log.Printf("[collector] stopped %s/%s/%s", ck.Namespace, ck.Name, ck.Container)
		}
	}
}

func (s *collectorState) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.streams {
		cancel()
	}
	s.streams = make(map[containerKey]context.CancelFunc)
	s.podNames = make(map[podKey]bool)
	log.Printf("[collector] stopped all streams")
}

// streamContainerLogsForCollector follows a single container's logs and writes
// every line to PostgreSQL. It runs for the lifetime of the pod.
func streamContainerLogsForCollector(ctx context.Context, ns, pod, container string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		args := []string{"logs", pod, "-n", ns, "-c", container, "--timestamps=true", "--follow", "--tail=100"}
		if kubeconfig != "" {
			args = append(args, "--kubeconfig", kubeconfig)
		}

		cmd := exec.CommandContext(ctx, "kubectl", args...)
		cmd.Env = os.Environ()
		if kubeconfig != "" {
			cmd.Env = append(cmd.Env, "KUBECONFIG="+kubeconfig)
		}

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("[collector] stdout pipe error %s/%s/%s: %v", ns, pod, container, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		stderr, _ := cmd.StderrPipe()

		if err := cmd.Start(); err != nil {
			log.Printf("[collector] start error %s/%s/%s: %v", ns, pod, container, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				log.Printf("[collector stderr %s/%s/%s] %s", ns, pod, container, scanner.Text())
			}
		}()

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				if cmd.Process != nil {
					cmd.Process.Kill()
				}
				return
			default:
			}
			raw := scanner.Text()
			l := parseKubectlLogLine(raw, pod, container)
			l.Cluster = "in-cluster"
			if kubeconfig != "" {
				l.Cluster = "kubeconfig"
			}
			l.Namespace = ns
			l.Workload = podWorkloadKey(ns, pod)
			if err := insertLogLine(ctx, l); err != nil {
				log.Printf("[db] insert error: %v", err)
			}
		}
		scannerErr := scanner.Err()
		_ = cmd.Wait()
		log.Printf("[collector] scanner done %s/%s/%s: %v", ns, pod, container, scannerErr)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// podWorkloadKey returns a stable grouping key for the collector.
// We don't know the parent workload name cheaply, so we group by namespace/pod
// for the global collector and expose it as the workload field.
func podWorkloadKey(ns, pod string) string {
	return ns + "/" + pod
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

// ── Kubernetes log streaming via kubectl ────────────────────────────────────

// podContainer holds a pod name and its container names.
type podContainer struct {
	Name       string
	Containers []string
	IsSidecar  []bool
}

// listWorkloadPods lists pods for a workload using the same label selector logic as handlePods.
func listWorkloadPods(ctxName, ns, workload, kind string) []podContainer {
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
		out, err := kubectl(context.Background(), args...)
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

	args := []string{"get", "pods", "-n", ns,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\t"}{range .spec.containers[*]}{.name}{" "}{end}{"\t"}{range .spec.initContainers[*]}{.name}{" "}{end}{"\n"}{end}`}
	if labelSelector != "" {
		args = append(args, "-l", labelSelector)
	} else {
		args = append(args, "--field-selector", "metadata.name="+workload)
	}
	if ctxName != "" && ctxName != "in-cluster" {
		args = append([]string{"--context", ctxName}, args...)
	}

	out, err := kubectl(context.Background(), args...)
	if err != nil {
		log.Printf("[list pods] error: %v", err)
		return nil
	}

	var result []podContainer
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		pc := podContainer{Name: parts[0]}
		for _, c := range strings.Fields(parts[1]) {
			pc.Containers = append(pc.Containers, c)
			pc.IsSidecar = append(pc.IsSidecar, isSidecarContainer(c))
		}
		if len(parts) >= 3 {
			for _, c := range strings.Fields(parts[2]) {
				pc.Containers = append(pc.Containers, c)
				pc.IsSidecar = append(pc.IsSidecar, true)
			}
		}
		result = append(result, pc)
	}
	return result
}

// parseKubectlLogLine parses a kubectl logs --timestamps line.
func parseKubectlLogLine(raw, pod, container string) LogLine {
	l := LogLine{
		Pod:       pod,
		Container: container,
		Ts:        time.Now().UTC(),
	}
	raw = strings.TrimRight(raw, "\n")
	// kubectl logs --timestamps format: "2006-01-02T15:04:05.123456789Z message"
	if idx := strings.IndexByte(raw, ' '); idx > 0 {
		if t, err := time.Parse(time.RFC3339Nano, raw[:idx]); err == nil {
			l.Ts = t.UTC()
			raw = raw[idx+1:]
		} else if t, err := time.Parse(time.RFC3339, raw[:idx]); err == nil {
			l.Ts = t.UTC()
			raw = raw[idx+1:]
		}
	}
	l.Message = raw
	msgLower := strings.ToLower(raw)
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

// streamContainerLogs follows logs for one container using kubectl logs --follow.
// It reconnects automatically when the stream ends or fails.
func streamContainerLogs(ctx context.Context, ctxName, ns, workload, pod, container string, sinceTime *time.Time, filter string, logCh chan<- string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		args := []string{"logs", pod, "-n", ns, "-c", container, "--timestamps=true", "--follow"}
		if ctxName != "" && ctxName != "in-cluster" {
			args = append([]string{"--context", ctxName}, args...)
		}
		if kubeconfig != "" {
			args = append(args, "--kubeconfig", kubeconfig)
		}
		if sinceTime != nil {
			args = append(args, "--since-time", sinceTime.UTC().Format(time.RFC3339))
		} else {
			args = append(args, "--tail=100")
		}

		cmd := exec.CommandContext(ctx, "kubectl", args...)
		cmd.Env = os.Environ()
		if kubeconfig != "" {
			cmd.Env = append(cmd.Env, "KUBECONFIG="+kubeconfig)
		}

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("[kubectl logs] stdout pipe error %s/%s: %v", pod, container, err)
			time.Sleep(2 * time.Second)
			continue
		}
		stderr, _ := cmd.StderrPipe()

		if err := cmd.Start(); err != nil {
			log.Printf("[kubectl logs] start error %s/%s: %v", pod, container, err)
			time.Sleep(2 * time.Second)
			continue
		}

		log.Printf("[kubectl logs] started %s/%s", pod, container)

		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				log.Printf("[kubectl logs stderr %s/%s] %s", pod, container, scanner.Text())
			}
		}()

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				if cmd.Process != nil {
					cmd.Process.Kill()
				}
				return
			default:
			}
			raw := scanner.Text()
			l := parseKubectlLogLine(raw, pod, container)
			if filter != "" && !strings.Contains(strings.ToLower(l.Message), strings.ToLower(filter)) {
				continue
			}
			l.Cluster = ctxName
			l.Namespace = ns
			l.Workload = workload
			if err := insertLogLine(ctx, l); err != nil {
				log.Printf("[db] insert error: %v", err)
			}
			select {
			case logCh <- formatLogLine(l):
			case <-ctx.Done():
				if cmd.Process != nil {
					cmd.Process.Kill()
				}
				return
			}
		}
		scannerErr := scanner.Err()
		_ = cmd.Wait()
		log.Printf("[kubectl logs] scanner done %s/%s: %v", pod, container, scannerErr)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// streamWorkloadLogsKubectl polls pods for a workload and follows logs for each container.
func streamWorkloadLogsKubectl(ctx context.Context, ctxName, ns, workload, kind, requestedPods, requestedContainers string, sinceTime *time.Time, filter string, logCh chan<- string) {
	requestedPodSet := map[string]bool{}
	if requestedPods != "" {
		for _, p := range strings.Split(requestedPods, ",") {
			if p = strings.TrimSpace(p); p != "" {
				requestedPodSet[p] = true
			}
		}
	}
	requestedContainerSet := map[string]bool{}
	if requestedContainers != "" {
		for _, c := range strings.Split(requestedContainers, ",") {
			if c = strings.TrimSpace(c); c != "" {
				requestedContainerSet[c] = true
			}
		}
	}

	active := map[string]bool{}
	var mu sync.Mutex

	for {
		podList := listWorkloadPods(ctxName, ns, workload, kind)
		for _, pc := range podList {
			if len(requestedPodSet) > 0 && !requestedPodSet[pc.Name] {
				continue
			}
			for i, c := range pc.Containers {
				if len(requestedContainerSet) > 0 && !requestedContainerSet[c] {
					continue
				}
				if len(requestedContainerSet) == 0 && pc.IsSidecar[i] {
					continue
				}
				key := pc.Name + "/" + c
				mu.Lock()
				if active[key] {
					mu.Unlock()
					continue
				}
				active[key] = true
				mu.Unlock()

				go func(pod, container string) {
					streamContainerLogs(ctx, ctxName, ns, workload, pod, container, sinceTime, filter, logCh)
					mu.Lock()
					delete(active, key)
					mu.Unlock()
				}(pc.Name, c)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
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
			// Custom range: stream history (with exact time bounds) then send __END__, no live streaming
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

	// Stream real-time logs via kubectl logs --follow
	go func() {
		// Small delay if we're also streaming history, to let it start first
		if sinceTime != nil {
			time.Sleep(100 * time.Millisecond)
		}
		streamWorkloadLogsKubectl(ctx, ctxName, ns, workload, q.Get("kind"), pods, containers, sinceTime, filter, logCh)
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

// ── API: config ───────────────────────────────────────────────────────────────

func handleConfig(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, map[string]interface{}{
		"retentionDays": retentionDays,
	})
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

	// Start leader election / global log collector
	go runLeaderElection(ctx)

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
	mux.HandleFunc("/api/config", handleConfig)

	log.Printf("LogaL starting on :%s", port)
	log.Printf("Database: %s (retention: %d days)", databaseURL, retentionDays)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
