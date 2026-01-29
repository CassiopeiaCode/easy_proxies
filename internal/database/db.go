package database

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	sql *sql.DB
}

type Node struct {
	ID                      int64
	Host                    string
	Port                    int
	URI                     string
	Name                    string
	Protocol                string
	IsDamaged               bool
	DamageReason            string
	HealthCheckCount        int64
	HealthCheckSuccessCount int64
	LastCheckAt             *time.Time
	LastSuccessAt           *time.Time
	LastLatencyMs           *int64
	FailureCount            int64
	BlacklistedUntil        *time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

type NodeRate struct {
	Host         string
	Port         int
	SuccessCount int64
	FailCount    int64
	Rate         float64 // 0..1; when total==0 -> 0
}

func Open(ctx context.Context, path string) (*DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("db path is empty")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Clean(path)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", path)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqldb.SetMaxOpenConns(1)
	sqldb.SetMaxIdleConns(1)

	db := &DB{sql: sqldb}
	if err := db.Ping(ctx); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	if err := db.Migrate(ctx); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	return db, nil
}

func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	return d.sql.Close()
}

func (d *DB) Ping(ctx context.Context) error {
	if d == nil || d.sql == nil {
		return errors.New("db not initialized")
	}
	if err := d.sql.PingContext(ctx); err != nil {
		return fmt.Errorf("ping sqlite: %w", err)
	}
	return nil
}

func (d *DB) Migrate(ctx context.Context) error {
	if d == nil || d.sql == nil {
		return errors.New("db not initialized")
	}
	const schema = `
CREATE TABLE IF NOT EXISTS nodes (
	 id INTEGER PRIMARY KEY AUTOINCREMENT,
	 host TEXT NOT NULL,
	 port INTEGER NOT NULL,
	 uri TEXT NOT NULL,
	 name TEXT NOT NULL DEFAULT '',
	 protocol TEXT NOT NULL DEFAULT '',
	 is_damaged INTEGER NOT NULL DEFAULT 0,
	 damage_reason TEXT NOT NULL DEFAULT '',
	 health_check_count INTEGER NOT NULL DEFAULT 0,
	 health_check_success_count INTEGER NOT NULL DEFAULT 0,
	 last_check_at DATETIME NULL,
	 last_success_at DATETIME NULL,
	 last_latency_ms INTEGER NULL,
	 failure_count INTEGER NOT NULL DEFAULT 0,
	 blacklisted_until DATETIME NULL,
	 created_at DATETIME NOT NULL DEFAULT (datetime('now')),
	 updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
	 UNIQUE(host, port)
);

-- Hourly aggregated health check stats per node.
-- hour_ts is truncated to the hour (UTC).
CREATE TABLE IF NOT EXISTS node_health_hourly (
	 host TEXT NOT NULL,
	 port INTEGER NOT NULL,
	 hour_ts DATETIME NOT NULL,
	 success_count INTEGER NOT NULL DEFAULT 0,
	 fail_count INTEGER NOT NULL DEFAULT 0,
	 latency_sum_ms INTEGER NOT NULL DEFAULT 0,
	 latency_count INTEGER NOT NULL DEFAULT 0,
	 updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
	 PRIMARY KEY(host, port, hour_ts)
);

CREATE INDEX IF NOT EXISTS idx_nodes_damaged ON nodes(is_damaged);
CREATE INDEX IF NOT EXISTS idx_nodes_health_count ON nodes(health_check_count);
CREATE INDEX IF NOT EXISTS idx_nodes_blacklisted_until ON nodes(blacklisted_until);

CREATE INDEX IF NOT EXISTS idx_node_health_hourly_hour ON node_health_hourly(hour_ts);
CREATE INDEX IF NOT EXISTS idx_node_health_hourly_hostport ON node_health_hourly(host, port);
`
	_, err := d.sql.ExecContext(ctx, schema)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

type UpsertNodeInput struct {
	URI  string
	Name string
}

func (d *DB) UpsertNodeByHostPort(ctx context.Context, in UpsertNodeInput) (Node, error) {
	if d == nil || d.sql == nil {
		return Node{}, errors.New("db not initialized")
	}
	in.URI = strings.TrimSpace(in.URI)
	in.Name = strings.TrimSpace(in.Name)
	if in.URI == "" {
		return Node{}, errors.New("uri is empty")
	}

	host, port, protocol, err := HostPortFromURI(in.URI)
	if err != nil {
		// Try to mark damaged by best-effort host/port empty => cannot upsert by host:port.
		return Node{}, err
	}
	if in.Name == "" {
		in.Name = NameFromURI(in.URI)
	}

	now := time.Now().UTC()
	const q = `
INSERT INTO nodes (host, port, uri, name, protocol, is_damaged, damage_reason, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 0, '', datetime('now'), datetime('now'))
ON CONFLICT(host, port) DO UPDATE SET
  uri = excluded.uri,
  name = CASE WHEN excluded.name != '' THEN excluded.name ELSE nodes.name END,
  protocol = excluded.protocol,
  updated_at = datetime('now')
RETURNING
  id, host, port, uri, name, protocol, is_damaged, damage_reason,
  health_check_count, health_check_success_count, last_check_at, last_success_at, last_latency_ms,
  failure_count, blacklisted_until, created_at, updated_at
`
	row := d.sql.QueryRowContext(ctx, q, host, port, in.URI, in.Name, protocol)
	n, err := scanNode(row)
	if err != nil {
		return Node{}, fmt.Errorf("upsert node: %w", err)
	}

	_ = now
	return n, nil
}

func (d *DB) MarkNodeDamaged(ctx context.Context, host string, port int, reason string) error {
	if d == nil || d.sql == nil {
		return errors.New("db not initialized")
	}
	host = strings.TrimSpace(host)
	reason = strings.TrimSpace(reason)
	if host == "" || port <= 0 {
		return errors.New("host/port invalid")
	}
	_, err := d.sql.ExecContext(ctx, `
UPDATE nodes
SET is_damaged = 1, damage_reason = ?, updated_at = datetime('now')
WHERE host = ? AND port = ?
`, reason, host, port)
	if err != nil {
		return fmt.Errorf("mark damaged: %w", err)
	}
	return nil
}

type HealthCheckUpdate struct {
	Host      string
	Port      int
	Success   bool
	LatencyMs *int64
	ErrText   string
}

func (d *DB) RecordHealthCheck(ctx context.Context, u HealthCheckUpdate) error {
	if d == nil || d.sql == nil {
		return errors.New("db not initialized")
	}
	u.Host = strings.TrimSpace(u.Host)
	u.ErrText = strings.TrimSpace(u.ErrText)
	if u.Host == "" || u.Port <= 0 {
		return errors.New("host/port invalid")
	}

	// Record into hourly aggregation (last 24h logic is query-time).
	if err := d.recordHourlyHealthCheck(ctx, u); err != nil {
		return err
	}

	if u.Success {
		_, err := d.sql.ExecContext(ctx, `
UPDATE nodes
SET health_check_count = health_check_count + 1,
    health_check_success_count = health_check_success_count + 1,
    last_check_at = datetime('now'),
    last_success_at = datetime('now'),
    last_latency_ms = ?,
    updated_at = datetime('now')
WHERE host = ? AND port = ?
`, u.LatencyMs, u.Host, u.Port)
		if err != nil {
			return fmt.Errorf("record health check (success): %w", err)
		}
		return nil
	}

	_, err := d.sql.ExecContext(ctx, `
UPDATE nodes
SET health_check_count = health_check_count + 1,
    last_check_at = datetime('now'),
    updated_at = datetime('now')
WHERE host = ? AND port = ?
`, u.Host, u.Port)
	if err != nil {
		return fmt.Errorf("record health check (fail): %w", err)
	}
	return nil
}

func (d *DB) recordHourlyHealthCheck(ctx context.Context, u HealthCheckUpdate) error {
	// Truncate to hour (UTC) to satisfy "each hour" aggregation.
	hour := time.Now().UTC().Truncate(time.Hour)

	var addSuccess, addFail int64
	if u.Success {
		addSuccess = 1
	} else {
		addFail = 1
	}

	var addLatencySum, addLatencyCount int64
	if u.Success && u.LatencyMs != nil && *u.LatencyMs > 0 {
		addLatencySum = *u.LatencyMs
		addLatencyCount = 1
	}

	_, err := d.sql.ExecContext(ctx, `
INSERT INTO node_health_hourly (
  host, port, hour_ts,
  success_count, fail_count,
  latency_sum_ms, latency_count,
  updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'))
ON CONFLICT(host, port, hour_ts) DO UPDATE SET
  success_count = node_health_hourly.success_count + excluded.success_count,
  fail_count = node_health_hourly.fail_count + excluded.fail_count,
  latency_sum_ms = node_health_hourly.latency_sum_ms + excluded.latency_sum_ms,
  latency_count = node_health_hourly.latency_count + excluded.latency_count,
  updated_at = datetime('now')
`, u.Host, u.Port, hour.Format("2006-01-02 15:04:05"), addSuccess, addFail, addLatencySum, addLatencyCount)
	if err != nil {
		return fmt.Errorf("record hourly health check: %w", err)
	}
	return nil
}

func (d *DB) QueryNodeRatesSince(ctx context.Context, since time.Time) ([]NodeRate, error) {
	if d == nil || d.sql == nil {
		return nil, errors.New("db not initialized")
	}
	since = since.UTC()
	sinceStr := since.Format("2006-01-02 15:04:05")

	rows, err := d.sql.QueryContext(ctx, `
SELECT
	 host,
	 port,
	 SUM(success_count) AS success_sum,
	 SUM(fail_count) AS fail_sum
FROM node_health_hourly
WHERE hour_ts >= ?
GROUP BY host, port
`, sinceStr)
	if err != nil {
		return nil, fmt.Errorf("query node rates: %w", err)
	}
	defer rows.Close()

	out := make([]NodeRate, 0)
	for rows.Next() {
		var r NodeRate
		if err := rows.Scan(&r.Host, &r.Port, &r.SuccessCount, &r.FailCount); err != nil {
			return nil, fmt.Errorf("scan node rate: %w", err)
		}
		total := r.SuccessCount + r.FailCount
		if total > 0 {
			r.Rate = float64(r.SuccessCount) / float64(total)
		} else {
			r.Rate = 0
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query node rates rows: %w", err)
	}
	return out, nil
}

// CleanupHealthStatsBefore deletes aggregated health check stats strictly before the given cutoff time.
// cutoff is treated as UTC and compared against hour_ts (stored as UTC "YYYY-MM-DD HH:MM:SS").
func (d *DB) CleanupHealthStatsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	if d == nil || d.sql == nil {
		return 0, errors.New("db not initialized")
	}
	cutoff = cutoff.UTC()
	cutoffStr := cutoff.Format("2006-01-02 15:04:05")

	res, err := d.sql.ExecContext(ctx, `
DELETE FROM node_health_hourly
WHERE hour_ts < ?
`, cutoffStr)
	if err != nil {
		return 0, fmt.Errorf("cleanup node_health_hourly: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (d *DB) ListActiveNodes(ctx context.Context) ([]Node, error) {
	if d == nil || d.sql == nil {
		return nil, errors.New("db not initialized")
	}
	const q = `
SELECT
  id, host, port, uri, name, protocol, is_damaged, damage_reason,
  health_check_count, health_check_success_count, last_check_at, last_success_at, last_latency_ms,
  failure_count, blacklisted_until, created_at, updated_at
FROM nodes
WHERE is_damaged = 0
ORDER BY updated_at DESC
`
	rows, err := d.sql.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()

	var out []Node
	for rows.Next() {
		var n Node
		var isDamaged int64
		var lastCheck, lastSuccess sql.NullTime
		var lastLatency sql.NullInt64
		var blacklistedUntil sql.NullTime
		if err := rows.Scan(
			&n.ID, &n.Host, &n.Port, &n.URI, &n.Name, &n.Protocol, &isDamaged, &n.DamageReason,
			&n.HealthCheckCount, &n.HealthCheckSuccessCount, &lastCheck, &lastSuccess, &lastLatency,
			&n.FailureCount, &blacklistedUntil, &n.CreatedAt, &n.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		n.IsDamaged = isDamaged != 0
		if lastCheck.Valid {
			t := lastCheck.Time
			n.LastCheckAt = &t
		}
		if lastSuccess.Valid {
			t := lastSuccess.Time
			n.LastSuccessAt = &t
		}
		if lastLatency.Valid {
			v := lastLatency.Int64
			n.LastLatencyMs = &v
		}
		if blacklistedUntil.Valid {
			t := blacklistedUntil.Time
			n.BlacklistedUntil = &t
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list nodes rows: %w", err)
	}
	return out, nil
}

func (d *DB) IsNodeDamaged(ctx context.Context, host string, port int) (bool, error) {
	if d == nil || d.sql == nil {
		return false, errors.New("db not initialized")
	}
	host = strings.TrimSpace(host)
	if host == "" || port <= 0 {
		return false, errors.New("host/port invalid")
	}

	var isDamaged int64
	err := d.sql.QueryRowContext(ctx, `
SELECT is_damaged
FROM nodes
WHERE host = ? AND port = ?
`, host, port).Scan(&isDamaged)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("query is_damaged: %w", err)
	}
	return isDamaged != 0, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanNode(r rowScanner) (Node, error) {
	var n Node
	var isDamaged int64
	var lastCheck, lastSuccess sql.NullTime
	var lastLatency sql.NullInt64
	var blacklistedUntil sql.NullTime
	if err := r.Scan(
		&n.ID, &n.Host, &n.Port, &n.URI, &n.Name, &n.Protocol, &isDamaged, &n.DamageReason,
		&n.HealthCheckCount, &n.HealthCheckSuccessCount, &lastCheck, &lastSuccess, &lastLatency,
		&n.FailureCount, &blacklistedUntil, &n.CreatedAt, &n.UpdatedAt,
	); err != nil {
		return Node{}, err
	}
	n.IsDamaged = isDamaged != 0
	if lastCheck.Valid {
		t := lastCheck.Time
		n.LastCheckAt = &t
	}
	if lastSuccess.Valid {
		t := lastSuccess.Time
		n.LastSuccessAt = &t
	}
	if lastLatency.Valid {
		v := lastLatency.Int64
		n.LastLatencyMs = &v
	}
	if blacklistedUntil.Valid {
		t := blacklistedUntil.Time
		n.BlacklistedUntil = &t
	}
	return n, nil
}

func NameFromURI(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		if idx := strings.LastIndex(raw, "#"); idx != -1 && idx < len(raw)-1 {
			return raw[idx+1:]
		}
		return ""
	}
	if u.Fragment == "" {
		return ""
	}
	decoded, err := url.QueryUnescape(u.Fragment)
	if err == nil && decoded != "" {
		return decoded
	}
	return u.Fragment
}

type vmessJSONHostPort struct {
	Add  string      `json:"add"`
	Port interface{} `json:"port"`
}

func (v vmessJSONHostPort) portInt() int {
	switch p := v.Port.(type) {
	case float64:
		return int(p)
	case int:
		return p
	case int64:
		return int(p)
	case string:
		n, _ := net.LookupPort("tcp", strings.TrimSpace(p))
		return n
	default:
		return 0
	}
}

// HostPortFromURI extracts the host/port used as dedup primary key.
//
// Rules:
// - Prefer URL host:port (u.Hostname/u.Port).
// - Default port when missing: 443 for most TLS-like schemes; 80 for http.
// - For vmess:// base64-json format, decode and extract (add, port) so it can participate in DB dedup/damaged/health.
func HostPortFromURI(raw string) (host string, port int, protocol string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, "", errors.New("uri empty")
	}

	// Special-case: vmess base64-json (vmess://<base64(json)>)
	// builder already supports this; DB must be able to extract host:port for dedup/tracking.
	if strings.HasPrefix(strings.ToLower(raw), "vmess://") {
		encoded := strings.TrimSpace(strings.TrimPrefix(raw, "vmess://"))
		if encoded != "" && !strings.Contains(encoded, "@") {
			var decoded []byte
			if b, decErr := base64.StdEncoding.DecodeString(encoded); decErr == nil {
				decoded = b
			} else if b, decErr := base64.RawStdEncoding.DecodeString(encoded); decErr == nil {
				decoded = b
			} else if b, decErr := base64.RawURLEncoding.DecodeString(encoded); decErr == nil {
				decoded = b
			} else if b, decErr := base64.URLEncoding.DecodeString(encoded); decErr == nil {
				decoded = b
			}

			if len(decoded) > 0 {
				var v vmessJSONHostPort
				if jErr := json.Unmarshal(decoded, &v); jErr == nil {
					h := strings.TrimSpace(v.Add)
					p := v.portInt()
					if h != "" && p > 0 && p <= 65535 {
						return h, p, "vmess", nil
					}
				}
			}
		}
	}

	u, parseErr := url.Parse(raw)
	if parseErr != nil {
		return "", 0, "", fmt.Errorf("parse uri: %w", parseErr)
	}

	protocol = strings.ToLower(u.Scheme)
	host = strings.TrimSpace(u.Hostname())
	portStr := strings.TrimSpace(u.Port())

	if host == "" {
		return "", 0, protocol, errors.New("missing host")
	}

	if portStr == "" {
		switch protocol {
		case "http":
			port = 80
		default:
			port = 443
		}
		return host, port, protocol, nil
	}

	p, convErr := net.LookupPort("tcp", portStr)
	if convErr != nil {
		return "", 0, protocol, fmt.Errorf("invalid port %q: %w", portStr, convErr)
	}
	return host, p, protocol, nil
}