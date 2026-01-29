package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"easy_proxies/internal/database"

	M "github.com/sagernet/sing/common/metadata"
)

// Config mirrors user settings needed by the monitoring server.
type Config struct {
	Enabled        bool
	Listen         string
	ProbeTarget    string
	Password       string
	ProxyUsername  string // 代理池的用户名（用于导出）
	ProxyPassword  string // 代理池的密码（用于导出）
	ExternalIP     string // 外部 IP 地址，用于导出时替换 0.0.0.0
	SkipCertVerify bool   // 全局跳过 SSL 证书验证
	Database       struct {
		Enabled bool
		Path    string
	}
}

// NodeInfo is static metadata about a proxy entry.
type NodeInfo struct {
	Tag           string `json:"tag"`
	Name          string `json:"name"`
	URI           string `json:"uri"`
	Mode          string `json:"mode"`
	ListenAddress string `json:"listen_address,omitempty"`
	Port          uint16 `json:"port,omitempty"`
}

// TimelineEvent represents a single usage event for debug tracking.
type TimelineEvent struct {
	Time      time.Time `json:"time"`
	Success   bool      `json:"success"`
	LatencyMs int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
}

const maxTimelineSize = 20

// Snapshot is a runtime view of a proxy node.
type Snapshot struct {
	NodeInfo
	FailureCount      int             `json:"failure_count"`
	SuccessCount      int64           `json:"success_count"`
	Blacklisted       bool            `json:"blacklisted"`
	BlacklistedUntil  time.Time       `json:"blacklisted_until"`
	ActiveConnections int32           `json:"active_connections"`
	LastError         string          `json:"last_error,omitempty"`
	LastFailure       time.Time       `json:"last_failure,omitempty"`
	LastSuccess       time.Time       `json:"last_success,omitempty"`
	LastProbeLatency  time.Duration   `json:"last_probe_latency,omitempty"`
	LastLatencyMs     int64           `json:"last_latency_ms"`
	Available         bool            `json:"available"`
	InitialCheckDone  bool            `json:"initial_check_done"`
	Timeline          []TimelineEvent `json:"timeline,omitempty"`
}

type probeFunc func(ctx context.Context) (time.Duration, error)
type releaseFunc func()

type EntryHandle struct {
	ref *entry
}

type entry struct {
	info             NodeInfo
	failure          int
	success          int64
	timeline         []TimelineEvent
	blacklist        bool
	until            time.Time
	lastError        string
	lastFail         time.Time
	lastOK           time.Time
	lastProbe        time.Duration
	active           atomic.Int32
	probe            probeFunc
	release          releaseFunc
	initialCheckDone bool
	available        bool
	mu               sync.RWMutex
}

// Manager aggregates all node states for the UI/API.
type Manager struct {
	cfg        Config
	probeDst   M.Socksaddr
	probeReady bool
	mu         sync.RWMutex
	nodes      map[string]*entry
	ctx        context.Context
	cancel     context.CancelFunc
	logger     Logger

	db   *database.DB
	dbMu sync.Mutex
}

// Logger interface for logging
type Logger interface {
	Info(args ...any)
	Warn(args ...any)
}

// NewManager constructs a manager and pre-validates the probe target.
func NewManager(cfg Config) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:    cfg,
		nodes:  make(map[string]*entry),
		ctx:    ctx,
		cancel: cancel,
	}
	if cfg.ProbeTarget != "" {
		target := cfg.ProbeTarget
		// Strip URL scheme if present (e.g., "https://www.google.com:443" -> "www.google.com:443")
		if strings.HasPrefix(target, "https://") {
			target = strings.TrimPrefix(target, "https://")
		} else if strings.HasPrefix(target, "http://") {
			target = strings.TrimPrefix(target, "http://")
		}
		// Remove trailing path if present
		if idx := strings.Index(target, "/"); idx != -1 {
			target = target[:idx]
		}
		host, port, err := net.SplitHostPort(target)
		if err != nil {
			// If no port specified, use default based on original scheme
			if strings.HasPrefix(cfg.ProbeTarget, "https://") {
				host = target
				port = "443"
			} else {
				host = target
				port = "80"
			}
		}
		parsed := M.ParseSocksaddrHostPort(host, parsePort(port))
		m.probeDst = parsed
		m.probeReady = true
	}
	if cfg.Database.Enabled && strings.TrimSpace(cfg.Database.Path) != "" {
		openCtx, cancelOpen := context.WithTimeout(ctx, 5*time.Second)
		db, err := database.Open(openCtx, cfg.Database.Path)
		cancelOpen()
		if err != nil {
			// Best-effort: monitoring can still work without DB.
			if m.logger != nil {
				m.logger.Warn("open database failed, health persistence disabled: ", err)
			}
		} else {
			m.db = db
		}
	}
	return m, nil
}

// SetLogger sets the logger for the manager.
func (m *Manager) SetLogger(logger Logger) {
	m.logger = logger
}

// StartPeriodicHealthCheck starts a background goroutine that periodically checks all nodes.
// interval: how often to check (e.g., 30 * time.Second)
// timeout: timeout for each probe (e.g., 10 * time.Second)
func (m *Manager) StartPeriodicHealthCheck(interval, timeout time.Duration) {
	if !m.probeReady {
		if m.logger != nil {
			m.logger.Warn("probe target not configured, periodic health check disabled")
		}
		return
	}

	go func() {
		// 启动后立即进行一次检查
		m.probeAllNodes(timeout)

		// 启动后做一次历史统计清理（仅清理健康检查 hourly 聚合表，保留最近 7 天）
		m.cleanupOldHealthStats()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		cleanupTicker := time.NewTicker(12 * time.Hour)
		defer cleanupTicker.Stop()

		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				m.probeAllNodes(timeout)
			case <-cleanupTicker.C:
				m.cleanupOldHealthStats()
			}
		}
	}()

	if m.logger != nil {
		m.logger.Info("periodic health check started, interval: ", interval)
	}
}

func (m *Manager) cleanupOldHealthStats() {
	if !m.shouldPersistHealth() {
		return
	}

	// 保留最近 7 天（按 hour_ts 存储；这里用 UTC 截断到整点）
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour).Truncate(time.Hour)

	m.dbMu.Lock()
	db := m.db
	m.dbMu.Unlock()
	if db == nil {
		return
	}

	cctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	deleted, err := db.CleanupHealthStatsBefore(cctx, cutoff)
	cancel()

	if err != nil {
		if m.logger != nil {
			m.logger.Warn("cleanup old health stats failed: ", err)
		}
		return
	}
	if deleted > 0 && m.logger != nil {
		m.logger.Info("cleanup old health stats deleted rows: ", deleted)
	}
}

// probeAllNodes checks all registered nodes concurrently.
func (m *Manager) probeAllNodes(timeout time.Duration) {
	m.mu.RLock()
	entries := make([]*entry, 0, len(m.nodes))
	for _, e := range m.nodes {
		entries = append(entries, e)
	}
	m.mu.RUnlock()

	if len(entries) == 0 {
		return
	}

	if m.logger != nil {
		m.logger.Info("starting health check for ", len(entries), " nodes")
	}

	workerLimit := runtime.NumCPU() * 2
	if workerLimit < 8 {
		workerLimit = 8
	}
	sem := make(chan struct{}, workerLimit)
	var wg sync.WaitGroup
	var availableCount atomic.Int32
	var failedCount atomic.Int32

	for _, e := range entries {
		e.mu.RLock()
		probeFn := e.probe
		tag := e.info.Tag
		info := e.info
		e.mu.RUnlock()

		if probeFn == nil {
			continue
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(entry *entry, probe probeFunc, tag string, info NodeInfo) {
			defer wg.Done()
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(m.ctx, timeout)
			latency, err := probe(ctx)
			cancel()

			// Persist health check into DB (hourly aggregation) if enabled.
			if m.shouldPersistHealth() {
				host, port, _, hpErr := database.HostPortFromURI(info.URI)
				if hpErr == nil && host != "" && port > 0 {
					var latencyMs *int64
					if err == nil {
						ms := latency.Milliseconds()
						if ms == 0 && latency > 0 {
							ms = 1
						}
						latencyMs = &ms
					}
					u := database.HealthCheckUpdate{
						Host:      host,
						Port:      port,
						Success:   err == nil,
						LatencyMs: latencyMs,
					}
					saveCtx, cancelSave := context.WithTimeout(m.ctx, 3*time.Second)
					_ = m.recordHealthCheck(saveCtx, u)
					cancelSave()
				}
			}

			entry.mu.Lock()
			if err != nil {
				failedCount.Add(1)
				entry.lastError = err.Error()
				entry.lastFail = time.Now()
				entry.available = false
				entry.initialCheckDone = true
			} else {
				availableCount.Add(1)
				entry.lastOK = time.Now()
				entry.lastProbe = latency
				entry.available = true
				entry.initialCheckDone = true
			}
			entry.mu.Unlock()

			if err != nil && m.logger != nil {
				m.logger.Warn("probe failed for ", tag, ": ", err)
			}
		}(e, probeFn, tag, info)
	}
	wg.Wait()

	// After recording all probes, recompute the 24h success-rate threshold (p95 - 5%)
	// and mark nodes as schedulable based on hourly stats. If DB isn't enabled, keep
	// existing "available" semantics.
	if m.shouldPersistHealth() {
		if err := m.applyHealthThresholdFromDB(); err != nil && m.logger != nil {
			m.logger.Warn("apply health threshold failed: ", err)
		}
	}

	if m.logger != nil {
		m.logger.Info("health check completed: ", availableCount.Load(), " available, ", failedCount.Load(), " failed")
	}
}

	// Stop stops the periodic health check.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.dbMu.Lock()
	if m.db != nil {
		_ = m.db.Close()
		m.db = nil
	}
	m.dbMu.Unlock()
}

func parsePort(value string) uint16 {
	p, err := strconv.Atoi(value)
	if err != nil || p <= 0 || p > 65535 {
		return 80
	}
	return uint16(p)
}

// Register ensures a node is tracked and returns its entry.
func (m *Manager) Register(info NodeInfo) *EntryHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.nodes[info.Tag]
	if !ok {
		e = &entry{
			info:     info,
			timeline: make([]TimelineEvent, 0, maxTimelineSize),
		}
		m.nodes[info.Tag] = e
	} else {
		e.info = info
	}
	return &EntryHandle{ref: e}
}

// DestinationForProbe exposes the configured destination for health checks.
func (m *Manager) DestinationForProbe() (M.Socksaddr, bool) {
	if !m.probeReady {
		return M.Socksaddr{}, false
	}
	return m.probeDst, true
}

// Snapshot returns a sorted copy of current node states.
// If onlyAvailable is true, only returns nodes that passed initial health check.
func (m *Manager) Snapshot() []Snapshot {
	return m.SnapshotFiltered(false)
}

// SnapshotFiltered returns a sorted copy of current node states.
// If onlyAvailable is true, only returns nodes that passed initial health check.
// Nodes that haven't been checked yet are also included (they will be checked on first use).
func (m *Manager) SnapshotFiltered(onlyAvailable bool) []Snapshot {
	m.mu.RLock()
	list := make([]*entry, 0, len(m.nodes))
	for _, e := range m.nodes {
		list = append(list, e)
	}
	m.mu.RUnlock()
	snapshots := make([]Snapshot, 0, len(list))
	for _, e := range list {
		snap := e.snapshot()
		// 如果只要可用节点：
		// - 跳过已完成检查但不可用的节点
		// - 保留未完成检查的节点（它们会在首次使用时被检查）
		if onlyAvailable && snap.InitialCheckDone && !snap.Available {
			continue
		}
		snapshots = append(snapshots, snap)
	}
	// 按延迟排序（延迟小的在前面，未测试的排在最后）
	sort.Slice(snapshots, func(i, j int) bool {
		latencyI := snapshots[i].LastLatencyMs
		latencyJ := snapshots[j].LastLatencyMs
		// -1 表示未测试，排在最后
		if latencyI < 0 && latencyJ < 0 {
			return snapshots[i].Name < snapshots[j].Name // 都未测试时按名称排序
		}
		if latencyI < 0 {
			return false // i 未测试，排在后面
		}
		if latencyJ < 0 {
			return true // j 未测试，i 排在前面
		}
		if latencyI == latencyJ {
			return snapshots[i].Name < snapshots[j].Name // 延迟相同时按名称排序
		}
		return latencyI < latencyJ
	})
	return snapshots
}

// Probe triggers a manual health check.
func (m *Manager) Probe(ctx context.Context, tag string) (time.Duration, error) {
	e, err := m.entry(tag)
	if err != nil {
		return 0, err
	}
	if e.probe == nil {
		return 0, errors.New("probe not available for this node")
	}
	latency, err := e.probe(ctx)
	if err != nil {
		return 0, err
	}
	e.recordProbeLatency(latency)
	return latency, nil
}

// Release clears blacklist state for the given node.
func (m *Manager) Release(tag string) error {
	e, err := m.entry(tag)
	if err != nil {
		return err
	}
	if e.release == nil {
		return errors.New("release not available for this node")
	}
	e.release()
	return nil
}

func (m *Manager) entry(tag string) (*entry, error) {
	m.mu.RLock()
	e, ok := m.nodes[tag]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("node %s not found", tag)
	}
	return e, nil
}

func (e *entry) snapshot() Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()

	latencyMs := int64(-1)
	if e.lastProbe > 0 {
		latencyMs = e.lastProbe.Milliseconds()
		if latencyMs == 0 {
			latencyMs = 1
		}
	}

	var timelineCopy []TimelineEvent
	if len(e.timeline) > 0 {
		timelineCopy = make([]TimelineEvent, len(e.timeline))
		copy(timelineCopy, e.timeline)
	}

	return Snapshot{
		NodeInfo:          e.info,
		FailureCount:      e.failure,
		SuccessCount:      e.success,
		Blacklisted:       e.blacklist,
		BlacklistedUntil:  e.until,
		ActiveConnections: e.active.Load(),
		LastError:         e.lastError,
		LastFailure:       e.lastFail,
		LastSuccess:       e.lastOK,
		LastProbeLatency:  e.lastProbe,
		LastLatencyMs:     latencyMs,
		Available:         e.available,
		InitialCheckDone:  e.initialCheckDone,
		Timeline:          timelineCopy,
	}
}

func (e *entry) recordFailure(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	errStr := err.Error()
	e.failure++
	e.lastError = errStr
	e.lastFail = time.Now()
	e.appendTimelineLocked(false, 0, errStr)
}

func (e *entry) recordSuccess() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.success++
	e.lastOK = time.Now()
	e.appendTimelineLocked(true, 0, "")
}

func (e *entry) recordSuccessWithLatency(latency time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.success++
	e.lastOK = time.Now()
	e.lastProbe = latency
	latencyMs := latency.Milliseconds()
	if latencyMs == 0 && latency > 0 {
		latencyMs = 1
	}
	e.appendTimelineLocked(true, latencyMs, "")
}

func (e *entry) appendTimelineLocked(success bool, latencyMs int64, errStr string) {
	evt := TimelineEvent{
		Time:      time.Now(),
		Success:   success,
		LatencyMs: latencyMs,
		Error:     errStr,
	}
	if len(e.timeline) >= maxTimelineSize {
		copy(e.timeline, e.timeline[1:])
		e.timeline[len(e.timeline)-1] = evt
	} else {
		e.timeline = append(e.timeline, evt)
	}
}

func (e *entry) blacklistUntil(until time.Time) {
	e.mu.Lock()
	e.blacklist = true
	e.until = until
	e.mu.Unlock()
}

func (e *entry) clearBlacklist() {
	e.mu.Lock()
	e.blacklist = false
	e.until = time.Time{}
	e.mu.Unlock()
}

func (e *entry) incActive() {
	e.active.Add(1)
}

func (e *entry) decActive() {
	e.active.Add(-1)
}

func (e *entry) setProbe(fn probeFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.probe = fn
}

func (e *entry) setRelease(fn releaseFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.release = fn
}

func (e *entry) recordProbeLatency(d time.Duration) {
	e.mu.Lock()
	e.lastProbe = d
	e.mu.Unlock()
}

// RecordFailure updates failure counters.
func (h *EntryHandle) RecordFailure(err error) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.recordFailure(err)
}

// RecordSuccess updates the last success timestamp.
func (h *EntryHandle) RecordSuccess() {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.recordSuccess()
}

// RecordSuccessWithLatency updates the last success timestamp and latency.
func (h *EntryHandle) RecordSuccessWithLatency(latency time.Duration) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.recordSuccessWithLatency(latency)
}

// Blacklist marks the node unavailable until the given deadline.
func (h *EntryHandle) Blacklist(until time.Time) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.blacklistUntil(until)
}

// ClearBlacklist removes the blacklist flag.
func (h *EntryHandle) ClearBlacklist() {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.clearBlacklist()
}

// IncActive increments the active connection counter.
func (h *EntryHandle) IncActive() {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.incActive()
}

// DecActive decrements the active connection counter.
func (h *EntryHandle) DecActive() {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.decActive()
}

// SetProbe assigns a probe function.
func (h *EntryHandle) SetProbe(fn func(ctx context.Context) (time.Duration, error)) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.setProbe(fn)
}

// SetRelease assigns a release function.
func (h *EntryHandle) SetRelease(fn func()) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.setRelease(fn)
}

// MarkInitialCheckDone marks the initial health check as completed.
func (h *EntryHandle) MarkInitialCheckDone(available bool) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.mu.Lock()
	h.ref.initialCheckDone = true
	h.ref.available = available
	h.ref.mu.Unlock()
}

// MarkAvailable updates the availability status.
func (h *EntryHandle) MarkAvailable(available bool) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.mu.Lock()
	h.ref.available = available
	h.ref.mu.Unlock()
}

func (h *EntryHandle) Snapshot() Snapshot {
	if h == nil || h.ref == nil {
		return Snapshot{}
	}
	return h.ref.snapshot()
}

func (m *Manager) shouldPersistHealth() bool {
	m.dbMu.Lock()
	defer m.dbMu.Unlock()
	return m.db != nil
}

func (m *Manager) recordHealthCheck(ctx context.Context, u database.HealthCheckUpdate) error {
	m.dbMu.Lock()
	db := m.db
	m.dbMu.Unlock()
	if db == nil {
		return nil
	}
	return db.RecordHealthCheck(ctx, u)
}

type nodeRate struct {
	host string
	port int
	rate float64
}

func (m *Manager) applyHealthThresholdFromDB() error {
	m.dbMu.Lock()
	db := m.db
	m.dbMu.Unlock()
	if db == nil {
		return nil
	}

	// Build rates map from node_health_hourly in last 24h.
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	rows, err := dbInternalQueryRates(m.ctx, db, cutoff)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	// Compute p95 of rates (0..1).
	rates := make([]float64, 0, len(rows))
	for _, r := range rows {
		rates = append(rates, r.rate)
	}
	p95 := percentile(rates, 0.95)
	threshold := p95 - 0.05
	if threshold < 0 {
		threshold = 0
	}
	if threshold > 1 {
		threshold = 1
	}

	// Apply availability based on threshold:
	// - Must have 24h stats to be schedulable
	// - rate >= threshold means healthy schedulable
	m.mu.RLock()
	entries := make([]*entry, 0, len(m.nodes))
	for _, e := range m.nodes {
		entries = append(entries, e)
	}
	m.mu.RUnlock()

	for _, e := range entries {
		e.mu.Lock()
		host, port, _, hpErr := database.HostPortFromURI(e.info.URI)
		if hpErr != nil || host == "" || port <= 0 {
			// No host:port mapping => cannot join 24h stats => not schedulable.
			e.available = false
			e.initialCheckDone = true
			e.mu.Unlock()
			continue
		}

		rate := lookupRate(rows, host, port)
		if rate < 0 {
			// No 24h stats => not schedulable.
			e.available = false
			e.initialCheckDone = true
			e.mu.Unlock()
			continue
		}

		e.available = rate >= threshold
		e.initialCheckDone = true
		e.mu.Unlock()
	}
	return nil
}

func lookupRate(rows []nodeRate, host string, port int) float64 {
	for _, r := range rows {
		if r.host == host && r.port == port {
			return r.rate
		}
	}
	return -1
}

func dbInternalQueryRates(ctx context.Context, db *database.DB, cutoff time.Time) ([]nodeRate, error) {
	rates, err := db.QueryNodeRatesSince(ctx, cutoff)
	if err != nil {
		return nil, err
	}
	out := make([]nodeRate, 0, len(rates))
	for _, r := range rates {
		out = append(out, nodeRate{
			host: r.Host,
			port: r.Port,
			rate: r.Rate,
		})
	}
	return out, nil
}

func percentile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if q <= 0 {
		return minFloat(values)
	}
	if q >= 1 {
		return maxFloat(values)
	}
	sort.Float64s(values)
	pos := q * float64(len(values)-1)
	lower := int(math.Floor(pos))
	upper := int(math.Ceil(pos))
	if lower == upper {
		return values[lower]
	}
	weight := pos - float64(lower)
	return values[lower]*(1-weight) + values[upper]*weight
}

func minFloat(values []float64) float64 {
	min := values[0]
	for _, v := range values[1:] {
		if v < min {
			min = v
		}
	}
	return min
}

func maxFloat(values []float64) float64 {
	max := values[0]
	for _, v := range values[1:] {
		if v > max {
			max = v
		}
	}
	return max
}
