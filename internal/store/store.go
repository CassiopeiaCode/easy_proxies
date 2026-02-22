package store

import (
	"context"
	"time"
)

// Store is the persistence layer for easy-proxies.
//
// Design goals:
// - embed-friendly (no external service)
// - support high write QPS for health stats (batch-friendly)
// - provide the minimum read APIs needed by builder/monitor/ui
//
// This interface is intended to fully replace the previous SQLite-based implementation.
// All times are expected to be UTC unless otherwise stated.
//
// NOTE: Implementations must be concurrency-safe unless documented otherwise.
//
//go:generate go test ./...

type Store interface {
	Close() error

	// Nodes
	UpsertNodeByHostPort(ctx context.Context, in UpsertNodeInput) (Node, error)
	DeleteNodeByURI(ctx context.Context, uri string) error
	DeleteNodeByHostPort(ctx context.Context, host string, port int) error
	ListActiveNodes(ctx context.Context) ([]Node, error)
	IsNodeDamaged(ctx context.Context, host string, port int) (bool, error)
	MarkNodeDamaged(ctx context.Context, host string, port int, reason string) error

	// Health
	RecordHealthCheck(ctx context.Context, u HealthCheckUpdate) error
	QueryNodeRatesSince(ctx context.Context, since time.Time) ([]NodeRate, error)
	CleanupHealthStatsBefore(ctx context.Context, cutoff time.Time) (int64, error)
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

type UpsertNodeInput struct {
	URI  string
	Name string
}

type HealthCheckUpdate struct {
	Host      string
	Port      int
	Success   bool
	LatencyMs *int64
	ErrText   string
}
