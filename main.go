package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"time"

	"github.com/jackc/pgx/v5"
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
	nodeName          = getEnv("NODE_NAME", "") // empty = watch all nodes (Deployment mode)
)

var dbPool *pgxpool.Pool

type CollectionConfig struct {
	RetentionDays int      `json:"retentionDays"`
	AllWorkloads  bool     `json:"allWorkloads"`
	Workloads     []string `json:"workloads"`
}

var (
	configWriteMu sync.Mutex
	configMu      sync.RWMutex
	currentConfig = CollectionConfig{
		RetentionDays: retentionDays,
		AllWorkloads:  true,
		Workloads:     []string{},
	}
)

func getCollectionConfig() CollectionConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	return currentConfig
}

func setCollectionConfig(cfg CollectionConfig) {
	configMu.Lock()
	currentConfig = cfg
	configMu.Unlock()
}

func isWorkloadSelected(cluster, ns, workload string) bool {
	cfg := getCollectionConfig()
	if cfg.AllWorkloads {
		return true
	}
	for _, w := range cfg.Workloads {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if w == cluster+"/"+ns+"/"+workload {
			return true
		}
	}
	return false
}

func saveConfigToDB(ctx context.Context, cfg CollectionConfig) error {
	if dbPool == nil {
		return fmt.Errorf("database not initialized")
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = dbPool.Exec(ctx, `
		INSERT INTO settings (key, value, updated_at)
		VALUES ('collection', $1, now())
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, updated_at = now()
	`, data)
	return err
}

func syncConfigFromDB(ctx context.Context) error {
	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	if dbPool == nil {
		return nil
	}
	var raw []byte
	err := dbPool.QueryRow(ctx, `SELECT value FROM settings WHERE key = 'collection'`).Scan(&raw)
	if err != nil {
		return err
	}
	var cfg CollectionConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	curr := getCollectionConfig()
	if curr.RetentionDays != cfg.RetentionDays || curr.AllWorkloads != cfg.AllWorkloads || !slicesEqual(curr.Workloads, cfg.Workloads) {
		setCollectionConfig(cfg)
		reconcileAllCollectors()
	}
	return nil
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func startConfigSync(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = syncConfigFromDB(ctx)
			}
		}
	}()
}

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
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	if kubeconfig != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	}
	return cmd.Output()
}

// ── API: contexts ─────────────────────────────────────────────────────────────

func handleContexts(w http.ResponseWriter, r *http.Request) {
	clusters, err := configuredClusters(r.Context())
	if err != nil {
		http.Error(w, "failed to list clusters", 502)
		return
	}
	jsonResp(w, clusters)
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
		filtered := []string{}
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

	result := []Workload{}
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

	pods := []PodInfo{}
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

	pods := listWorkloadPods(ctx, ctxName, ns, workload, r.URL.Query().Get("kind"))
	if len(pods) == 0 {
		http.Error(w, "pod not found", 404)
		return
	}
	args := []string{"get", "pod", pods[0].Name, "-n", ns, "-o", `jsonpath={.metadata.name}{"\t"}{.status.startTime}`}

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
	if maxH > getCollectionConfig().RetentionDays*24 {
		maxH = getCollectionConfig().RetentionDays * 24
	}

	// Check if database history exists for this workload
	var hasCache bool
	if dbPool != nil {
		_ = dbPool.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM logs WHERE cluster=$1 AND namespace=$2 AND workload=$3 LIMIT 1)`,
			ctxName, ns, workload).Scan(&hasCache)
	}

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
		pool.Close()
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
		CREATE TABLE IF NOT EXISTS settings (
			key         TEXT PRIMARY KEY,
			value       JSONB NOT NULL,
			updated_at  TIMESTAMPTZ DEFAULT now()
		);
	`)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	var raw []byte
	err = pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = 'collection'`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		defaultCfg := CollectionConfig{
			RetentionDays: retentionDays,
			AllWorkloads:  true,
			Workloads:     []string{},
		}
		data, marshalErr := json.Marshal(defaultCfg)
		if marshalErr != nil {
			pool.Close()
			return nil, fmt.Errorf("marshal default collection settings: %w", marshalErr)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('collection', $1) ON CONFLICT (key) DO NOTHING`, data); err != nil {
			pool.Close()
			return nil, fmt.Errorf("insert default collection settings: %w", err)
		} else if err := pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = 'collection'`).Scan(&raw); err != nil {
			pool.Close()
			return nil, fmt.Errorf("load collection settings after insert: %w", err)
		}
		var cfg CollectionConfig
		if err := decodeCollectionConfig(raw, &cfg); err != nil {
			pool.Close()
			return nil, fmt.Errorf("invalid stored collection settings: %w", err)
		}
		setCollectionConfig(cfg)
	} else if err != nil {
		pool.Close()
		return nil, fmt.Errorf("load collection settings: %w", err)
	} else {
		var cfg CollectionConfig
		if err := decodeCollectionConfig(raw, &cfg); err != nil {
			pool.Close()
			return nil, fmt.Errorf("invalid stored collection settings: %w", err)
		}
		setCollectionConfig(cfg)
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
func streamHistoryFiles(ctx context.Context, cluster, ns, workload string, since time.Time, until *time.Time, filter, pods, containers string, out chan<- string) {
	if dbPool == nil {
		log.Printf("[history] database pool uninitialized")
		return
	}
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
	for _, selection := range []struct{ column, value string }{{"pod", pods}, {"container", containers}} {
		if selection.value != "" {
			args = append(args, strings.Split(selection.value, ","))
			sql += fmt.Sprintf(" AND %s = ANY($%d::text[])", selection.column, len(args))
		}
	}
	sql += ` ORDER BY ts ASC, id ASC`

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
type podDetails struct {
	workload   string
	containers []string
}

type collectorState struct {
	mu          sync.Mutex
	ctx         context.Context
	cluster     string
	streams     map[containerKey]context.CancelFunc
	podNames    map[podKey]bool
	pods        map[podKey]podDetails
	startStream func(ctx context.Context, cluster, ns, workload, pod, container string)
}

func (s *collectorState) rootContext() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func (s *collectorState) streamContainer(ctx context.Context, cluster, ns, workload, pod, container string) {
	if s.startStream != nil {
		s.startStream(ctx, cluster, ns, workload, pod, container)
		return
	}
	streamContainerLogsForCollector(ctx, cluster, ns, workload, pod, container)
}

func (s *collectorState) reconcile() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for pk, details := range s.pods {
		selected := isWorkloadSelected(s.cluster, pk.Namespace, details.workload)
		for _, c := range details.containers {
			ck := containerKey{podKey: pk, Container: c}
			cancel, running := s.streams[ck]
			if selected && !running {
				cctx, cCancel := context.WithCancel(s.rootContext())
				s.streams[ck] = cCancel
				go s.streamContainer(cctx, s.cluster, pk.Namespace, details.workload, pk.Name, c)
				log.Printf("[collector] started stream %s/%s/%s", pk.Namespace, pk.Name, c)
			} else if !selected && running {
				cancel()
				delete(s.streams, ck)
				log.Printf("[collector] stopped stream %s/%s/%s (deselected)", pk.Namespace, pk.Name, c)
			}
		}
	}
}

var (
	collectorsMu     sync.Mutex
	activeCollectors []*collectorState
)

func registerCollector(s *collectorState) {
	collectorsMu.Lock()
	activeCollectors = append(activeCollectors, s)
	collectorsMu.Unlock()
}

func unregisterCollector(s *collectorState) {
	collectorsMu.Lock()
	defer collectorsMu.Unlock()
	for i, c := range activeCollectors {
		if c == s {
			activeCollectors = append(activeCollectors, activeCollectors[i+1:]...)
			break
		}
	}
}

func reconcileAllCollectors() {
	collectorsMu.Lock()
	cols := make([]*collectorState, len(activeCollectors))
	copy(cols, activeCollectors)
	collectorsMu.Unlock()

	for _, c := range cols {
		c.reconcile()
	}
}

// runLogCollector watches pods in one context and persists their container logs.
func runLogCollector(ctx context.Context, cluster string) {
	state := &collectorState{
		ctx:      ctx,
		cluster:  cluster,
		streams:  make(map[containerKey]context.CancelFunc),
		podNames: make(map[podKey]bool),
		pods:     make(map[podKey]podDetails),
	}
	registerCollector(state)
	defer unregisterCollector(state)

	for {
		select {
		case <-ctx.Done():
			state.stopAll()
			return
		default:
		}
		log.Printf("[collector] syncing existing pods")
		syncExistingPods(ctx, state)
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

// syncExistingPods lists all currently running pods on this node and starts
// log streams for every container. This is needed because kubectl --watch only
// emits events for changes after the watch starts.
func syncExistingPods(ctx context.Context, state *collectorState) {
	args := []string{"get", "pods", "--all-namespaces", "-o", "json"}
	if state.cluster != "in-cluster" {
		args = append(args, "--context", state.cluster)
	}
	if nodeName != "" {
		args = append(args, "--field-selector", "spec.nodeName="+nodeName)
	}
	if kubeconfig != "" {
		args = append(args, "--kubeconfig", kubeconfig)
	}

	out, err := kubectl(ctx, args...)
	if err != nil {
		log.Printf("[collector] list existing pods error: %v", err)
		return
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name            string           `json:"name"`
				Namespace       string           `json:"namespace"`
				OwnerReferences []ownerReference `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				Containers []struct {
					Name string `json:"name"`
				} `json:"containers"`
				InitContainers []struct {
					Name string `json:"name"`
				} `json:"initContainers"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		log.Printf("[collector] unmarshal existing pods error: %v", err)
		return
	}

	seen := make(map[podKey]bool)
	for _, item := range list.Items {
		seen[podKey{Namespace: item.Metadata.Namespace, Name: item.Metadata.Name}] = true
		event := podWatchEvent{Type: "ADDED"}
		event.Object.Metadata = item.Metadata
		event.Object.Spec.Containers = item.Spec.Containers
		event.Object.Spec.InitContainers = item.Spec.InitContainers
		event.Object.Status.Phase = item.Status.Phase
		state.handlePodEvent(ctx, event)
	}
	state.mu.Lock()
	var stale []podKey
	for key := range state.podNames {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	state.mu.Unlock()
	for _, key := range stale {
		state.stopPod(key)
	}
	log.Printf("[collector] synced %d existing pods", len(list.Items))
}

// watchPods runs `kubectl get pods --all-namespaces --watch` and keeps log
// streams alive for every running pod/container.
func watchPods(ctx context.Context, state *collectorState) {
	args := []string{"get", "pods", "--all-namespaces", "--watch", "--output-watch-events", "-o", "json"}
	if state.cluster != "in-cluster" {
		args = append(args, "--context", state.cluster)
	}
	if nodeName != "" {
		args = append(args, "--field-selector", "spec.nodeName="+nodeName)
	}
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
			Name            string           `json:"name"`
			Namespace       string           `json:"namespace"`
			OwnerReferences []ownerReference `json:"ownerReferences"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
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
	if !namespaceAllowed(ns) {
		s.stopPod(key)
		return
	}
	if phase != "Running" && phase != "Pending" {
		s.stopPod(key)
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

	workload := workloadForPod(ctx, s.cluster, ns, pod, e.Object.Metadata.OwnerReferences)
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.podNames == nil {
		s.podNames = make(map[podKey]bool)
	}
	if s.pods == nil {
		s.pods = make(map[podKey]podDetails)
	}

	s.podNames[key] = true
	s.pods[key] = podDetails{
		workload:   workload,
		containers: containers,
	}

	if !isWorkloadSelected(s.cluster, ns, workload) {
		for _, c := range containers {
			ck := containerKey{podKey: key, Container: c}
			if cancel, ok := s.streams[ck]; ok {
				cancel()
				delete(s.streams, ck)
				log.Printf("[collector] stopped %s/%s/%s (filtered)", ns, pod, c)
			}
		}
		return
	}

	for _, c := range containers {
		ck := containerKey{podKey: key, Container: c}
		if _, ok := s.streams[ck]; ok {
			continue
		}
		cctx, cancel := context.WithCancel(s.rootContext())
		s.streams[ck] = cancel
		go s.streamContainer(cctx, s.cluster, ns, workload, pod, c)
		log.Printf("[collector] started %s/%s/%s", ns, pod, c)
	}
}

func (s *collectorState) stopPod(key podKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.podNames, key)
	delete(s.pods, key)
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
	s.pods = make(map[podKey]podDetails)
	log.Printf("[collector] stopped all streams")
}

// streamContainerLogsForCollector follows a single container's logs and writes
// every line to PostgreSQL. It runs for the lifetime of the pod.
func streamContainerLogsForCollector(ctx context.Context, cluster, ns, workload, pod, container string) {
	var cursor logCursor
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		args := []string{"logs", pod, "-n", ns, "-c", container, "--timestamps=true", "--follow"}
		if cursor.last.IsZero() {
			args = append(args, "--tail=100")
		}
		if cluster != "in-cluster" {
			args = append(args, "--context", cluster)
		}
		if kubeconfig != "" {
			args = append(args, "--kubeconfig", kubeconfig)
		}

		cursor.reconnect()
		if !cursor.last.IsZero() {
			args = append(args, "--since-time="+cursor.last.UTC().Format(time.RFC3339Nano))
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
					_ = cmd.Wait()
				}
				return
			default:
			}
			raw := scanner.Text()
			l := parseKubectlLogLine(raw, pod, container)
			if cursor.repeated(l.Ts, raw) {
				continue
			}
			l.Cluster = cluster
			l.Namespace = ns
			l.Workload = workload
			if err := insertLogLine(ctx, l); err != nil {
				log.Printf("[db] insert error: %v", err)
			} else {
				cursor.record(l.Ts, raw)
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
	if dbPool == nil {
		return
	}
	days := getCollectionConfig().RetentionDays
	if days <= 0 || days > 365 {
		days = retentionDays
		if days <= 0 || days > 365 {
			days = 3
		}
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)
	ctx := context.Background()
	conn, err := dbPool.Acquire(ctx)
	if err != nil {
		log.Printf("[cleanup] acquire connection error: %v", err)
		return
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(714329018241)`).Scan(&locked); err != nil {
		log.Printf("[cleanup] lock error: %v", err)
		return
	}
	if !locked {
		return
	}
	defer func() {
		var released bool
		if err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock(714329018241)`).Scan(&released); err != nil {
			log.Printf("[cleanup] unlock error: %v", err)
		}
	}()
	_, err = conn.Exec(ctx,
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
func listWorkloadPods(ctx context.Context, ctxName, ns, workload, kind string) []podContainer {
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

	out, err := kubectl(ctx, args...)
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
func streamContainerLogs(ctx context.Context, ctxName, ns, workload, pod, container string, sinceTime *time.Time, tail int, filter string, logCh chan<- string) {
	var cursor logCursor
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
		if sinceTime != nil && cursor.last.IsZero() {
			args = append(args, "--since-time", sinceTime.UTC().Format(time.RFC3339Nano))
		} else if cursor.last.IsZero() {
			args = append(args, "--tail="+strconv.Itoa(tail))
		}

		cursor.reconnect()
		if !cursor.last.IsZero() {
			args = append(args, "--since-time="+cursor.last.UTC().Format(time.RFC3339Nano))
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
					_ = cmd.Wait()
				}
				return
			default:
			}
			raw := scanner.Text()
			l := parseKubectlLogLine(raw, pod, container)
			if cursor.repeated(l.Ts, raw) {
				continue
			}
			if filter != "" && !strings.Contains(strings.ToLower(l.Message), strings.ToLower(filter)) {
				continue
			}
			l.Cluster = ctxName
			l.Namespace = ns
			l.Workload = workload
			select {
			case logCh <- formatLogLine(l):
				cursor.record(l.Ts, raw)
			case <-ctx.Done():
				if cmd.Process != nil {
					cmd.Process.Kill()
					_ = cmd.Wait()
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
func streamWorkloadLogsKubectl(ctx context.Context, ctxName, ns, workload, kind, requestedPods, requestedContainers string, sinceTime *time.Time, tail int, filter string, logCh chan<- string) {
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
		podList := listWorkloadPods(ctx, ctxName, ns, workload, kind)
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
					streamContainerLogs(ctx, ctxName, ns, workload, pod, container, sinceTime, tail, filter, logCh)
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
	fromStr := q.Get("from")          // ISO8601 custom range start
	toStr := q.Get("to")              // ISO8601 custom range end
	pods := q.Get("pods")             // comma-separated pod names
	containers := q.Get("containers") // comma-separated container names

	if ns == "" || workload == "" {
		http.Error(w, "ns and workload required", http.StatusBadRequest)
		return
	}

	tail := 100
	if fromStr == "" {
		if value := q.Get("tail"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 0 {
				http.Error(w, "invalid tail count", 400)
				return
			}
			tail = parsed
		}
	}
	if tail == 0 {
		tail = -1
	} // kubectl uses -1 for all available lines.
	// Validate the range before sending streaming headers.
	liveStart := time.Now().UTC()
	var sinceTime, toTime *time.Time
	isCustomRange := fromStr != ""
	isRealtime := !isCustomRange && (sinceStr == "" || sinceStr == "realtime")
	if toStr != "" && !isCustomRange {
		http.Error(w, "from required with to", 400)
		return
	}
	if isCustomRange {
		from, err := time.Parse(time.RFC3339Nano, fromStr)
		if err != nil {
			http.Error(w, "invalid from timestamp", 400)
			return
		}
		sinceTime = &from
		to := liveStart
		if toStr != "" {
			to, err = time.Parse(time.RFC3339Nano, toStr)
			if err != nil {
				http.Error(w, "invalid to timestamp", 400)
				return
			}
		}
		if to.Before(from) {
			http.Error(w, "to must be at or after from", 400)
			return
		}
		toTime = &to
	} else if !isRealtime {
		dur := parseSince(sinceStr)
		if dur <= 0 {
			http.Error(w, "invalid since duration", 400)
			return
		}
		since := liveStart.Add(-dur)
		sinceTime = &since
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	logCh := make(chan string, 500)

	// Stream persisted history first
	if sinceTime != nil {
		if isCustomRange && toTime != nil {
			// Custom range: stream history (with exact time bounds) then send __END__, no live streaming
			go func() {
				streamHistoryFiles(ctx, ctxName, ns, workload, *sinceTime, toTime, filter, pods, containers, logCh)
				// Safely send end sentinel (ctx may already be done)
				select {
				case logCh <- `{"__end__":true}`:
				case <-ctx.Done():
				}
			}()
			goto streamLoop
		}
		go func() {
			streamHistoryFiles(ctx, ctxName, ns, workload, *sinceTime, &liveStart, filter, pods, containers, logCh)
		}()
	}

	// Stream real-time logs via kubectl logs --follow
	go func() {
		// Small delay if we're also streaming history, to let it start first
		if sinceTime != nil {
			time.Sleep(100 * time.Millisecond)
		}
		liveSince := &liveStart
		if isRealtime {
			liveSince = nil
		}
		streamWorkloadLogsKubectl(ctx, ctxName, ns, workload, q.Get("kind"), pods, containers, liveSince, tail, filter, logCh)
	}()

	// Send SSE to browser
streamLoop:
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
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
	if dbPool == nil {
		jsonResp(w, HistoryInfo{Available: false})
		return
	}
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

func decodeCollectionConfig(data []byte, cfg *CollectionConfig) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return fmt.Errorf("expected config object")
	}
	if len(fields) != 3 {
		return fmt.Errorf("all config fields required")
	}
	for _, key := range []string{"retentionDays", "allWorkloads", "workloads"} {
		if _, ok := fields[key]; !ok {
			return fmt.Errorf("missing field %s", key)
		}
	}
	for key := range fields {
		if key != "retentionDays" && key != "allWorkloads" && key != "workloads" {
			return fmt.Errorf("unknown field %s", key)
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(cfg)
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		jsonResp(w, getCollectionConfig())
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1048577))
	if err != nil || len(body) > 1048576 {
		http.Error(w, "read error or body too large", http.StatusBadRequest)
		return
	}

	var req CollectionConfig
	if err := decodeCollectionConfig(body, &req); err != nil {
		http.Error(w, "invalid config: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.RetentionDays < 1 || req.RetentionDays > 365 {
		http.Error(w, "retentionDays must be between 1 and 365", http.StatusBadRequest)
		return
	}

	for _, wName := range req.Workloads {
		wName = strings.TrimSpace(wName)
		parts := strings.Split(wName, "/")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			http.Error(w, "workloads must be cluster/namespace/name", http.StatusBadRequest)
			return
		}
		ns := parts[1]
		if ns != "" && !namespaceAllowed(ns) {
			http.Error(w, "forbidden namespace: "+ns, http.StatusForbidden)
			return
		}
	}

	if dbPool != nil {
		if err := saveConfigToDB(r.Context(), req); err != nil {
			log.Printf("[config] save error: %v", err)
			http.Error(w, "failed to save settings: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	setCollectionConfig(req)
	reconcileAllCollectors()

	jsonResp(w, req)
}

// ── API: container logs (for terminated/init containers) ──────────────────────

func handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ctxName := r.URL.Query().Get("ctx")
	ns := r.URL.Query().Get("ns")
	pod := r.URL.Query().Get("pod")
	container := r.URL.Query().Get("container")
	isInit := r.URL.Query().Get("init") == "true"

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
	if err := validateAuth(); err != nil {
		log.Fatal(err)
	}

	// Connect to PostgreSQL
	pool, err := initDB(ctx)
	if err != nil {
		log.Fatalf("failed to initialize database: %v", err)
	}
	dbPool = pool
	defer dbPool.Close()

	// Start cleanup goroutine
	startCleanup()

	// Start periodic config sync across DaemonSet pods
	startConfigSync(ctx)

	// Start node-local log collector (DaemonSet mode).
	// If NODE_NAME is empty it falls back to collecting from all nodes.
	clusters, err := configuredClusters(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if nodeName != "" && (len(clusters) > 1 || kubeconfig != "") {
		log.Fatal("kubeconfig/multi-cluster collection requires NODE_NAME unset and one collector replica")
	}
	for _, cluster := range clusters {
		go runLogCollector(ctx, cluster)
	}

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
	log.Printf("Log retention: %d days", retentionDays)
	if err := http.ListenAndServe(":"+port, protect(mux)); err != nil {
		log.Fatal(err)
	}
}
