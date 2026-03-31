package monitor

import (
	"context"
	"testing"
	"time"

	"easy_proxies/internal/store"
)

type fakeStore struct {
	rates           []store.NodeRate
	activeNodes     []store.Node
	getNodeCalls    int
	listActiveCalls int
}

func (f *fakeStore) Close() error { return nil }
func (f *fakeStore) UpsertNodeByHostPort(context.Context, store.UpsertNodeInput) (store.Node, error) {
	return store.Node{}, nil
}
func (f *fakeStore) UpsertNodesByHostPortBatch(context.Context, []store.UpsertNodeInput) error {
	return nil
}
func (f *fakeStore) DeleteNodeByURI(context.Context, string) error { return nil }
func (f *fakeStore) DeleteNodeByHostPort(context.Context, string, int) error {
	return nil
}
func (f *fakeStore) GetNodeByHostPort(context.Context, string, int) (store.Node, bool, error) {
	f.getNodeCalls++
	return store.Node{}, false, nil
}
func (f *fakeStore) ListNodes(context.Context) ([]store.Node, error) {
	return nil, nil
}
func (f *fakeStore) ListActiveNodes(context.Context) ([]store.Node, error) {
	f.listActiveCalls++
	return f.activeNodes, nil
}
func (f *fakeStore) IsNodeDamaged(context.Context, string, int) (bool, error) {
	return false, nil
}
func (f *fakeStore) MarkNodeDamaged(context.Context, string, int, string) error { return nil }
func (f *fakeStore) UpdateNodeEgressIP(context.Context, string, int, string, time.Time) error {
	return nil
}
func (f *fakeStore) RecordHealthCheck(context.Context, store.HealthCheckUpdate) error { return nil }
func (f *fakeStore) QueryNodeRatesSince(context.Context, time.Time) ([]store.NodeRate, error) {
	return f.rates, nil
}
func (f *fakeStore) CleanupHealthStatsBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func TestApplyHealthThresholdUsesWarmEgressCache(t *testing.T) {
	st := &fakeStore{
		rates: []store.NodeRate{
			{Host: "node1.example", Port: 8080, SuccessCount: 9, FailCount: 1, Rate: 0.9},
			{Host: "node2.example", Port: 8081, SuccessCount: 7, FailCount: 3, Rate: 0.7},
		},
		activeNodes: []store.Node{
			{Host: "node1.example", Port: 8080, EgressIP: "203.0.113.10"},
			{Host: "node2.example", Port: 8081, EgressIP: "203.0.113.10"},
		},
	}

	mgr, err := NewManager(Config{}, st)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	mgr.Register(NodeInfo{Tag: "node1", URI: "http://node1.example:8080"})
	mgr.Register(NodeInfo{Tag: "node2", URI: "http://node2.example:8081"})

	if err := mgr.applyHealthThresholdFromDB(); err != nil {
		t.Fatalf("apply health threshold: %v", err)
	}
	if st.listActiveCalls == 0 {
		t.Fatalf("expected active nodes preload to run")
	}
	if st.getNodeCalls != 0 {
		t.Fatalf("expected no point lookups after warm cache, got %d", st.getNodeCalls)
	}

	snaps := mgr.Snapshot()
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snaps))
	}

	availableByTag := make(map[string]bool, len(snaps))
	for _, snap := range snaps {
		availableByTag[snap.Tag] = snap.Available
	}
	if !availableByTag["node1"] {
		t.Fatalf("expected representative node to remain available")
	}
	if availableByTag["node2"] {
		t.Fatalf("expected duplicate egress node to be filtered out")
	}
}
