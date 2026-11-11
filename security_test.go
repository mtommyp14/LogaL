package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeKubectl(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func TestAuthAndNamespaceBoundary(t *testing.T) {
	t.Setenv("AUTH_DISABLED", "false")
	t.Setenv("AUTH_USERNAME", "admin")
	t.Setenv("AUTH_PASSWORD", "secret")
	old := allowedNamespaces
	allowedNamespaces = "production, staging"
	defer func() { allowedNamespaces = old }()
	fakeKubectl(t, "printf 'east\\nwest\\n'")
	h := protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, tc := range []struct {
		path, password string
		status         int
	}{
		{"/", "", 401}, {"/api/logs?ctx=east&ns=production", "wrong", 401},
		{"/api/logs?ctx=east&ns=production", "secret", 204},
		{"/api/logs?ctx=west&ns=private", "secret", 403},
		{"/api/logs?ctx=unknown&ns=production", "secret", 400},
		{"/healthz", "", 200},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		if tc.password != "" {
			r.SetBasicAuth("admin", tc.password)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s: got %d want %d", tc.path, w.Code, tc.status)
		}
	}
}

func TestAuthConfiguration(t *testing.T) {
	t.Setenv("AUTH_DISABLED", "false")
	t.Setenv("AUTH_USERNAME", "")
	t.Setenv("AUTH_PASSWORD", "")
	if validateAuth() == nil {
		t.Fatal("missing credentials must fail startup")
	}
	t.Setenv("AUTH_DISABLED", "true")
	if err := validateAuth(); err != nil {
		t.Fatal(err)
	}
}

func TestClusterDiscovery(t *testing.T) {
	old := kubeconfig
	defer func() { kubeconfig = old }()
	kubeconfig = "/explicit/config"
	fakeKubectl(t, "printf 'east\\nwest\\n'")
	clusters, err := configuredClusters(context.Background())
	if err != nil || strings.Join(clusters, ",") != "east,west" {
		t.Fatalf("%v %v", clusters, err)
	}
	fakeKubectl(t, "exit 1")
	if _, err := configuredClusters(context.Background()); err == nil {
		t.Fatal("explicit kubeconfig errors must not silently select another cluster")
	}
	kubeconfig = ""
	clusters, err = configuredClusters(context.Background())
	if err != nil || len(clusters) != 1 || clusters[0] != "in-cluster" {
		t.Fatalf("%v %v", clusters, err)
	}
}

func TestWorkloadOwnerResolution(t *testing.T) {
	fakeKubectl(t, `case "$*" in
 *"--context east"*) printf '{"metadata":{"ownerReferences":[{"kind":"Deployment","name":"api","controller":true}]}}';;
 *) exit 1;;
 esac`)
	owner := []ownerReference{{Kind: "ReplicaSet", Name: "api-123", Controller: true}}
	if got := workloadForPod(context.Background(), "east", "production", "api-123-ab", owner); got != "api" {
		t.Fatal(got)
	}
	if got := workloadForPod(context.Background(), "east", "production", "worker-0", []ownerReference{{Kind: "StatefulSet", Name: "worker", Controller: true}}); got != "worker" {
		t.Fatal(got)
	}
}

func TestCollectorStopsCompletedPodsAndRejectsNamespaces(t *testing.T) {
	old := allowedNamespaces
	allowedNamespaces = "production"
	defer func() { allowedNamespaces = old }()
	state := &collectorState{streams: make(map[containerKey]context.CancelFunc), podNames: make(map[podKey]bool)}
	var e podWatchEvent
	e.Object.Metadata.Namespace = "private"
	e.Object.Metadata.Name = "api"
	e.Object.Status.Phase = "Running"
	state.handlePodEvent(context.Background(), e)
	if len(state.podNames) != 0 {
		t.Fatal("disallowed pod collected")
	}
	key := podKey{Namespace: "production", Name: "api"}
	cancelled := false
	state.streams[containerKey{podKey: key, Container: "app"}] = func() { cancelled = true }
	e.Object.Metadata.Namespace = "production"
	e.Object.Status.Phase = "Succeeded"
	state.handlePodEvent(context.Background(), e)
	if !cancelled || len(state.streams) != 0 {
		t.Fatal("completed pod stream not stopped")
	}
}

func TestInvalidLogRanges(t *testing.T) {
	for _, query := range []string{
		"from=invalid", "from=2026-01-02T00:00:00Z&to=invalid",
		"from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z",
		"to=2026-01-02T00:00:00Z", "since=invalid", "since=-1h",
	} {
		w := httptest.NewRecorder()
		handleLogs(w, httptest.NewRequest("GET", "/api/logs?ns=production&workload=api&"+query, nil))
		if w.Code != 400 {
			t.Errorf("%s: got %d", query, w.Code)
		}
		if strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
			t.Fatal("invalid request started SSE")
		}
	}
}

func TestWatchRequestsEventEnvelopes(t *testing.T) {
	fakeKubectl(t, `case "$*" in
 *"--context east"*) ;;
 *) exit 1;;
 esac
 case "$*" in
 *"--output-watch-events"*) printf '{"type":"DELETED","object":{"metadata":{"namespace":"production","name":"api"},"status":{"phase":"Running"}}}';;
 *) exit 1;;
 esac`)
	old := allowedNamespaces
	allowedNamespaces = ""
	defer func() { allowedNamespaces = old }()
	cancelled := false
	state := &collectorState{cluster: "east", streams: map[containerKey]context.CancelFunc{
		{podKey: podKey{Namespace: "production", Name: "api"}, Container: "app"}: func() { cancelled = true },
	}, podNames: make(map[podKey]bool)}
	watchPods(context.Background(), state)
	if !cancelled {
		t.Fatal("watch failed to consume event envelope in selected context")
	}
}
