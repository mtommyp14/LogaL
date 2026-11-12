package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

func validateAuth() error {
	if os.Getenv("AUTH_DISABLED") == "true" {
		return nil
	}
	if os.Getenv("AUTH_USERNAME") == "" || os.Getenv("AUTH_PASSWORD") == "" {
		return fmt.Errorf("set AUTH_USERNAME and AUTH_PASSWORD, or explicitly set AUTH_DISABLED=true for local development")
	}
	return nil
}

// HTTP Basic authentication also protects same-origin EventSource connections.
func protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if os.Getenv("AUTH_DISABLED") != "true" {
			user, pass, ok := r.BasicAuth()
			gotUser, gotPass := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(pass))
			wantUser, wantPass := sha256.Sum256([]byte(os.Getenv("AUTH_USERNAME"))), sha256.Sum256([]byte(os.Getenv("AUTH_PASSWORD")))
			validUser := subtle.ConstantTimeCompare(gotUser[:], wantUser[:])
			validPass := subtle.ConstantTimeCompare(gotPass[:], wantPass[:])
			if !ok || validUser&validPass != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="LogaL", charset="UTF-8"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Method == http.MethodPost && r.URL.Path == "/api/config" {
				// allowed for settings update
			} else {
				http.Error(w, "method not allowed", 405)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if ns := r.URL.Query().Get("ns"); ns != "" && !namespaceAllowed(ns) {
				http.Error(w, "namespace forbidden", 403)
				return
			}
			if r.URL.Path != "/api/config" && r.URL.Path != "/api/contexts" {
				cluster := r.URL.Query().Get("ctx")
				clusters, err := configuredClusters(r.Context())
				if err != nil {
					http.Error(w, "failed to list clusters", 502)
					return
				}
				found := false
				for _, name := range clusters {
					if name == cluster {
						found = true
					}
				}
				if !found {
					http.Error(w, "unknown cluster", 400)
					return
				}
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func namespaceAllowed(ns string) bool {
	if allowedNamespaces == "" {
		return true
	}
	for _, allowed := range strings.Split(allowedNamespaces, ",") {
		if strings.TrimSpace(allowed) == ns {
			return true
		}
	}
	return false
}

func configuredClusters(ctx context.Context) ([]string, error) {
	out, err := kubectl(ctx, "config", "get-contexts", "-o", "name")
	if err != nil {
		if kubeconfig != "" {
			return nil, fmt.Errorf("list kubeconfig contexts: %w", err)
		}
		return []string{"in-cluster"}, nil
	}
	clusters := strings.Fields(string(out))
	if len(clusters) == 0 {
		if kubeconfig != "" {
			return nil, fmt.Errorf("kubeconfig has no contexts")
		}
		return []string{"in-cluster"}, nil
	}
	return clusters, nil
}

type ownerReference struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Controller bool   `json:"controller"`
}

func workloadForPod(ctx context.Context, cluster, ns, pod string, owners []ownerReference) string {
	for _, owner := range owners {
		if !owner.Controller {
			continue
		}
		if owner.Kind != "ReplicaSet" {
			return owner.Name
		}
		args := []string{"get", "replicaset", owner.Name, "-n", ns, "-o", "json"}
		if cluster != "in-cluster" {
			args = append(args, "--context", cluster)
		}
		out, err := kubectl(ctx, args...)
		if err == nil {
			var rs struct {
				Metadata struct {
					Owners []ownerReference `json:"ownerReferences"`
				} `json:"metadata"`
			}
			if json.Unmarshal(out, &rs) == nil {
				for _, parent := range rs.Metadata.Owners {
					if parent.Controller {
						return parent.Name
					}
				}
			}
		}
		return owner.Name
	}
	return pod
}
