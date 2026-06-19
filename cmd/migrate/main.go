// One-off migration script: import existing flat-file logs into PostgreSQL.
//
// Usage:
//   export DATABASE_URL=postgres://user:pass@host:5432/logal
//   export LOG_DIR=/data/logs
//   cd cmd/migrate
//   go run .
//
// This reads files like /data/logs/{cluster}/{namespace}/{workload}/YYYY-MM-DD.log
// and inserts them into the PostgreSQL logs table.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx := context.Background()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://postgres:postgres@localhost:5432/logal"
	}
	logDir := os.Getenv("LOG_DIR")
	if logDir == "" {
		logDir = "/data/logs"
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Fatalf("connect to db: %v", err)
	}
	defer pool.Close()

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
	`)
	if err != nil {
		log.Fatalf("create schema: %v", err)
	}

	total := 0
	err = filepath.Walk(logDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".log") {
			return nil
		}

		rel, err := filepath.Rel(logDir, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 4 {
			return nil
		}
		cluster := parts[0]
		namespace := parts[1]
		workload := parts[2]

		file, err := os.Open(path)
		if err != nil {
			log.Printf("open %s: %v", path, err)
			return nil
		}
		defer file.Close()

		batch := make([][]interface{}, 0, 1000)
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			l, ok := parseLogLine(line)
			if !ok {
				continue
			}
			l.Cluster = cluster
			l.Namespace = namespace
			l.Workload = workload
			batch = append(batch, []interface{}{
				l.Cluster, l.Namespace, l.Workload, l.Pod, l.Container,
				l.Level, l.Ts.UTC(), l.Message,
			})
			if len(batch) >= 1000 {
				copyBatch(ctx, pool, batch)
				total += len(batch)
				batch = batch[:0]
			}
		}
		if len(batch) > 0 {
			copyBatch(ctx, pool, batch)
			total += len(batch)
		}
		log.Printf("migrated %s: %d rows", path, total)
		return nil
	})
	if err != nil {
		log.Fatalf("walk log dir: %v", err)
	}
	fmt.Printf("Migration complete. Total rows imported: %d\n", total)
}

func copyBatch(ctx context.Context, pool *pgxpool.Pool, rows [][]interface{}) {
	_, err := pool.CopyFrom(ctx,
		pgx.Identifier{"logs"},
		[]string{"cluster", "namespace", "workload", "pod", "container", "level", "ts", "message"},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		log.Printf("copy batch error: %v", err)
	}
}

// parseLogLine parses the same format produced by LogaL.
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

	msgLower := strings.ToLower(l.Message)
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
	return l, true
}

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
