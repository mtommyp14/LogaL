package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestProtectMethodAndConfigAuth(t *testing.T) {
	t.Setenv("AUTH_DISABLED", "false")
	t.Setenv("AUTH_USERNAME", "admin")
	t.Setenv("AUTH_PASSWORD", "secret")

	h := protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// POST on endpoints other than /api/config must be rejected with 405
	req := httptest.NewRequest("POST", "/api/logs", nil)
	req.SetBasicAuth("admin", "secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/logs: got %d, want 405", w.Code)
	}

	// PUT on /api/config must be rejected with 405
	req = httptest.NewRequest("PUT", "/api/config", nil)
	req.SetBasicAuth("admin", "secret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/config: got %d, want 405", w.Code)
	}

	// POST on /api/config without credentials must be 401
	req = httptest.NewRequest("POST", "/api/config", bytes.NewBufferString("{}"))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/config unauth: got %d, want 401", w.Code)
	}

	// POST on /api/config with valid credentials must be allowed through
	req = httptest.NewRequest("POST", "/api/config", bytes.NewBufferString("{}"))
	req.SetBasicAuth("admin", "secret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/config auth: got %d, want 200", w.Code)
	}
}

func TestHandleConfigValidation(t *testing.T) {
	oldAllowed, oldConfig := allowedNamespaces, getCollectionConfig()
	t.Cleanup(func() {
		allowedNamespaces = oldAllowed
		setCollectionConfig(oldConfig)
	})
	allowedNamespaces = "production,staging"

	// Reset config
	setCollectionConfig(CollectionConfig{
		RetentionDays: 3,
		AllWorkloads:  true,
		Workloads:     []string{},
	})

	// GET returns current config
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/config", nil)
	handleConfig(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/config: got %d", w.Code)
	}
	var got CollectionConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.RetentionDays != 3 {
		t.Fatalf("GET /api/config payload invalid: %v", w.Body.String())
	}

	// POST invalid json
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/config", bytes.NewBufferString("invalid json"))
	handleConfig(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST invalid json: got %d, want 400", w.Code)
	}

	// POST invalid retentionDays
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/config", bytes.NewBufferString(`{"retentionDays":0}`))
	handleConfig(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST retentionDays 0: got %d, want 400", w.Code)
	}

	// POST reject DATABASE_URL injection
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/config", bytes.NewBufferString(`{"retentionDays":7,"database_url":"postgres://bad"}`))
	handleConfig(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST with database_url: got %d, want 400", w.Code)
	}

	// POST forbidden namespace in workloads
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/config", bytes.NewBufferString(`{"retentionDays":7,"allWorkloads":false,"workloads":["in-cluster/kube-system/coredns"]}`))
	handleConfig(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST forbidden namespace: got %d, want 403", w.Code)
	}

	// POST valid config
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/config", bytes.NewBufferString(`{"retentionDays":14,"allWorkloads":false,"workloads":["in-cluster/production/api","in-cluster/production/worker"]}`))
	handleConfig(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("POST valid config: got %d: %s", w.Code, w.Body.String())
	}

	cfg := getCollectionConfig()
	if cfg.RetentionDays != 14 || cfg.AllWorkloads != false || len(cfg.Workloads) != 2 {
		t.Fatalf("config state mismatch: %+v", cfg)
	}
}

func TestWorkloadFilteringAndDynamicReconciliation(t *testing.T) {
	oldConfig := getCollectionConfig()
	t.Cleanup(func() { setCollectionConfig(oldConfig) })
	setCollectionConfig(CollectionConfig{
		RetentionDays: 7,
		AllWorkloads:  false,
		Workloads:     []string{"in-cluster/production/api"},
	})

	if !isWorkloadSelected("in-cluster", "production", "api") {
		t.Fatal("expected production/api to be selected")
	}
	if isWorkloadSelected("cluster-b", "production", "api") || isWorkloadSelected("cluster-a", "default", "api") {
		t.Fatal("selection must match exact cluster, namespace, workload")
	}
	if isWorkloadSelected("in-cluster", "production", "db") {
		t.Fatal("expected production/db NOT to be selected")
	}

	// Reconcile test:
	// Setup a collector with known pods and spy stream starter
	var mu sync.Mutex
	started := make(map[string]int)

	state := &collectorState{
		ctx:      context.Background(),
		cluster:  "in-cluster",
		streams:  make(map[containerKey]context.CancelFunc),
		podNames: make(map[podKey]bool),
		pods:     make(map[podKey]podDetails),
		startStream: func(ctx context.Context, cluster, ns, workload, pod, container string) {
			mu.Lock()
			started[ns+"/"+pod+"/"+container]++
			mu.Unlock()
			<-ctx.Done()
		},
	}

	pod1Key := podKey{Namespace: "production", Name: "api-xxx"}
	pod2Key := podKey{Namespace: "production", Name: "db-yyy"}

	state.pods[pod1Key] = podDetails{
		workload:   "api",
		containers: []string{"app"},
	}
	state.pods[pod2Key] = podDetails{
		workload:   "db",
		containers: []string{"database"},
	}

	// First reconcile: only api should start, db should NOT start
	state.reconcile()

	c1 := containerKey{podKey: pod1Key, Container: "app"}
	c2 := containerKey{podKey: pod2Key, Container: "database"}

	if _, ok := state.streams[c1]; !ok {
		t.Fatal("expected stream for api-xxx to be running")
	}
	if _, ok := state.streams[c2]; ok {
		t.Fatal("expected stream for db-yyy to NOT be running")
	}

	// Now dynamic change: enable db, disable api
	setCollectionConfig(CollectionConfig{
		RetentionDays: 7,
		AllWorkloads:  false,
		Workloads:     []string{"in-cluster/production/db"},
	})

	state.reconcile()

	// api should have been cancelled and removed, db should have been started
	if _, ok := state.streams[c1]; ok {
		t.Fatal("expected stream for api-xxx to be stopped")
	}
	if _, ok := state.streams[c2]; !ok {
		t.Fatal("expected stream for db-yyy to be started")
	}
}
