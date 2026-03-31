package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"easy_proxies/internal/store"

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
const healthStatsCacheTTL = 30 * time.Second
const dbThresholdMinTotal = 2
const egressCacheTTL = 10 * time.Minute

// Snapshot is a runtime view of a proxy node.
type Snapshot struct {
	NodeInfo
	FailureCount      int             `json:"failure_count"`
	SuccessCount      int64           `json:"success_count"`
	TotalCount        int64           `json:"total_count"`
	SuccessRate       float64         `json:"success_rate"`
	DB24hSuccessCount int64           `json:"db_24h_success_count"`
	DB24hFailureCount int64           `json:"db_24h_failure_count"`
	DB24hTotalCount   int64           `json:"db_24h_total_count"`
	DB24hSuccessRate  float64         `json:"db_24h_success_rate"`
	DB24hFailureRate  float64         `json:"db_24h_failure_rate"`
	EgressIP          string          `json:"egress_ip,omitempty"`
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
	host             string
	port             int
	hostPortValid    bool
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

	mgr *Manager

	// dbEventMinInterval throttles RecordFailure/RecordSuccess persistence so high QPS traffic
	// doesn't turn into high QPS SQLite writes. Periodic probes still persist separately.
	lastDBEventUnix atomic.Int64

	mu sync.RWMutex
}

// Manager aggregates all node states for the UI/API.
type Manager struct {
	cfg Config

	probeDst        M.Socksaddr
	probeReady      bool
	probeUseTLS     bool
	probeHostHeader string // Host header value (may include :port)
	probeServerName string // TLS SNI server name (hostname only)
	probePath       string // HTTP request path (includes query)
	probeInsecure   bool   // TLS insecure (skip verify)

	mu     sync.RWMutex
	nodes  map[string]*entry
	ctx    context.Context
	cancel context.CancelFunc
	logger Logger

	// Periodic health-check rounds:
	// If a new round starts while the previous one is still running, we cancel the previous round
	// and start the new one. Partial results already recorded are preserved (per-node updates happen
	// as probes complete).
	probeRoundMu       sync.Mutex
	probeRoundCancel   context.CancelFunc
	probeRoundActiveID int64
	probeRoundID       atomic.Int64

	// throttle health recompute triggered by real traffic.
	lastHealthRecomputeUnix atomic.Int64
	healthApplyRunning      atomic.Bool

	db   store.Store
	dbMu sync.Mutex

	healthStatsLoadMu  sync.Mutex
	healthStatsCache   healthStatsCacheEntry
	healthStatsCacheMu sync.RWMutex
	healthStatsLoading atomic.Bool

	egressMu    sync.RWMutex
	egressCache map[string]egressCacheEntry // key: host:port
}

type healthStatsCacheEntry struct {
	at              time.Time
	rows            []nodeRate
	rateByKey       map[string]nodeRate
	healthByKey     map[string]bool
	threshold       float64
	p95             float64
	eligibleKeys    int
	eligibleNonZero int
}

type egressCacheEntry struct {
	ip string
	at time.Time
}

type runtimeNodeRef struct {
	host  string
	port  int
	valid bool
}

// Logger interface for logging
type Logger interface {
	Info(args ...any)
	Warn(args ...any)
}

// NewManager constructs a manager and pre-validates the probe target.
// Store lifecycle is managed by the app and injected here.
func NewManager(cfg Config, st store.Store) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:         cfg,
		nodes:       make(map[string]*entry),
		ctx:         ctx,
		cancel:      cancel,
		db:          st,
		egressCache: make(map[string]egressCacheEntry),
	}

	if cfg.ProbeTarget != "" {
		dst, useTLS, hostHeader, serverName, path, insecure, ok := parseProbeTarget(cfg.ProbeTarget, cfg.SkipCertVerify)
		if ok {
			m.probeDst = dst
			m.probeUseTLS = useTLS
			m.probeHostHeader = hostHeader
			m.probeServerName = serverName
			m.probePath = path
			m.probeInsecure = insecure
			m.probeReady = true
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

	// Capture the current context for the lifetime of this periodic loop.
	// Important: ResetRuntime() replaces m.ctx with a fresh context. If the loop
	// reads m.ctx dynamically, it may miss the cancellation of the old context and
	// leak (causing multiple concurrent periodic probe loops after reloads).
	loopCtx := m.ctx
	if loopCtx == nil {
		loopCtx = context.Background()
	}

	go func() {
		// 启动后立即进行一次检查（可抢占：新一轮会取消旧一轮）
		m.startProbeAllNodesWithCtx(loopCtx, timeout)

		// 启动后做一次历史统计清理（仅清理健康检查 hourly 聚合表，保留最近 7 天）
		m.cleanupOldHealthStatsWithCtx(loopCtx)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// In DB mode, recompute availability periodically even if probes are infrequent
		// or traffic-triggered recomputes are throttled.
		recomputeTicker := time.NewTicker(5 * time.Minute)
		defer recomputeTicker.Stop()

		debugTicker := time.NewTicker(120 * time.Second)
		defer debugTicker.Stop()

		cleanupTicker := time.NewTicker(12 * time.Hour)
		defer cleanupTicker.Stop()

		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				// Do not queue up; cancel the previous round and start a new one.
				m.startProbeAllNodesWithCtx(loopCtx, timeout)
			case <-recomputeTicker.C:
				if m.shouldPersistHealth() {
					if err := m.applyHealthThresholdFromDB(); err != nil && m.logger != nil {
						m.logger.Warn("apply health threshold failed: ", err)
					}
				}
			case <-debugTicker.C:
				m.logDebugState()
			case <-cleanupTicker.C:
				m.cleanupOldHealthStatsWithCtx(loopCtx)
			}
		}
	}()

	if m.logger != nil {
		m.logger.Info("periodic health check started, interval: ", interval)
	}
}

func (m *Manager) logDebugState() {
	if m == nil || m.logger == nil {
		return
	}

	now := time.Now()
	dbEnabled := m.shouldPersistHealth()
	cacheAt := time.Time{}
	cacheRows := 0
	cachedThreshold := 0.0
	cachedP95 := 0.0
	cachedEligibleKeys := 0
	cachedEligibleNonZero := 0
	threshold := 0.0
	p95 := 0.0
	eligibleKeys := 0
	eligibleNonZero := 0
	var rateByKey map[string]nodeRate

	if dbEnabled {
		m.dbMu.Lock()
		db := m.db
		m.dbMu.Unlock()
		if db != nil {
			stats, err := m.load24hStatsCached(db, false)
			if err != nil {
				m.logger.Warn("debug: load 24h stats failed: ", err)
			} else {
				cacheAt = stats.at
				cacheRows = len(stats.rows)
				cachedThreshold = stats.threshold
				cachedP95 = stats.p95
				cachedEligibleKeys = stats.eligibleKeys
				cachedEligibleNonZero = stats.eligibleNonZero
				rateByKey = stats.rateByKey
				// egress IP is resolved via point-lookup cache (avoid full node scan).
			}
		}
	}

	snaps := m.Snapshot()
	total := len(snaps)
	readyCount := 0
	availableCount := 0
	schedulableCount := 0
	notReadyCount := 0
	blacklistedCount := 0
	activeConns := int64(0)

	for _, s := range snaps {
		if s.Blacklisted {
			blacklistedCount++
		}
		activeConns += int64(s.ActiveConnections)
		if s.InitialCheckDone {
			readyCount++
		} else {
			notReadyCount++
		}
		if s.Available {
			availableCount++
		}
		if s.InitialCheckDone && s.Available {
			schedulableCount++
		}
	}

	if dbEnabled && rateByKey != nil && total > 0 {
		// Avoid heavy recompute in debug logger: use cached p95/threshold (derived from DB 24h stats).
		threshold = cachedThreshold
		p95 = cachedP95
		eligibleKeys = cachedEligibleKeys
		eligibleNonZero = cachedEligibleNonZero
	}

	probeRoundActiveID := func() int64 {
		m.probeRoundMu.Lock()
		defer m.probeRoundMu.Unlock()
		return m.probeRoundActiveID
	}()
	lastRecomputeUnix := m.lastHealthRecomputeUnix.Load()
	lastRecompute := ""
	if lastRecomputeUnix > 0 {
		lastRecompute = time.Unix(lastRecomputeUnix, 0).Format(time.RFC3339)
	}
	cacheAge := ""
	if !cacheAt.IsZero() {
		cacheAge = now.Sub(cacheAt).Truncate(time.Millisecond).String()
	}

	m.logger.Info(
		"debug: nodes_total=", total,
		" ready=", readyCount,
		" not_ready=", notReadyCount,
		" available=", availableCount,
		" schedulable=", schedulableCount,
		" blacklisted=", blacklistedCount,
		" active_conns=", activeConns,
		" db=", dbEnabled,
		" stats_rows=", cacheRows,
		" threshold=", fmt.Sprintf("%.4f", threshold),
		" p95=", fmt.Sprintf("%.4f", p95),
		" eligible_keys=", eligibleKeys,
		" eligible_nonzero=", eligibleNonZero,
		" cached_threshold=", fmt.Sprintf("%.4f", cachedThreshold),
		" cache_age=", cacheAge,
		" probe_round=", probeRoundActiveID,
		" last_recompute=", lastRecompute,
		" probe_ready=", m.probeReady,
	)

	if total == 0 {
		return
	}

	// Sample nodes and log details:
	// - Pick up to 5 schedulable nodes (init && avail)
	// - Pick up to 5 unschedulable nodes (everything else)
	// This avoids log bias when schedulable set is tiny but total runtime nodes is large.
	schedulableSnaps := make([]Snapshot, 0, 5)
	unschedulableSnaps := make([]Snapshot, 0, 5)

	for i := range snaps {
		s := snaps[i]
		if s.InitialCheckDone && s.Available {
			schedulableSnaps = append(schedulableSnaps, s)
		} else {
			unschedulableSnaps = append(unschedulableSnaps, s)
		}
	}

	rng := rand.New(rand.NewSource(now.UnixNano()))
	rng.Shuffle(len(schedulableSnaps), func(i, j int) { schedulableSnaps[i], schedulableSnaps[j] = schedulableSnaps[j], schedulableSnaps[i] })
	rng.Shuffle(len(unschedulableSnaps), func(i, j int) {
		unschedulableSnaps[i], unschedulableSnaps[j] = unschedulableSnaps[j], unschedulableSnaps[i]
	})

	nSched := 5
	if len(schedulableSnaps) < nSched {
		nSched = len(schedulableSnaps)
	}
	nUnsched := 5
	if len(unschedulableSnaps) < nUnsched {
		nUnsched = len(unschedulableSnaps)
	}

	sample := make([]Snapshot, 0, nSched+nUnsched)
	sample = append(sample, schedulableSnaps[:nSched]...)
	sample = append(sample, unschedulableSnaps[:nUnsched]...)

	for i := range sample {
		s := sample[i]
		hostPort := ""
		db24h := ""
		if dbEnabled && rateByKey != nil && s.URI != "" {
			host, port, _, hpErr := store.HostPortFromURI(s.URI)
			if hpErr == nil && host != "" && port > 0 {
				hostPort = fmt.Sprintf("%s:%d", host, port)
				if row, ok := rateByKey[nodeRateKey(host, port)]; ok {
					totalCnt := row.success + row.fail
					db24h = fmt.Sprintf(" db24h=%d rate=%.2f%%", totalCnt, row.rate*100)
				} else {
					db24h = " db24h=0"
				}
			}
		}
		if hostPort != "" {
			hostPort = " hp=" + hostPort
		}
		m.logger.Info(
			"debug: node tag=", s.Tag,
			" name=", s.Name,
			hostPort,
			" init=", s.InitialCheckDone,
			" avail=", s.Available,
			" bl=", s.Blacklisted,
			" active=", s.ActiveConnections,
			" last_lat_ms=", s.LastLatencyMs,
			" last_err=", s.LastError,
			db24h,
		)
	}
}

func (m *Manager) startProbeAllNodes(timeout time.Duration) {
	m.startProbeAllNodesWithCtx(m.ctx, timeout)
}

func (m *Manager) startProbeAllNodesWithCtx(baseCtx context.Context, timeout time.Duration) {
	if m == nil {
		return
	}
	if baseCtx == nil {
		baseCtx = context.Background()
	}

	// Each call starts a new round; cancel the previous one (if still running).
	roundID := m.probeRoundID.Add(1)

	m.probeRoundMu.Lock()
	if m.probeRoundCancel != nil {
		m.probeRoundCancel()
	}
	roundCtx, cancel := context.WithCancel(baseCtx)
	m.probeRoundCancel = cancel
	m.probeRoundActiveID = roundID
	m.probeRoundMu.Unlock()

	go func(id int64) {
		defer func() {
			m.probeRoundMu.Lock()
			if m.probeRoundActiveID == id {
				m.probeRoundCancel = nil
				m.probeRoundActiveID = 0
			}
			m.probeRoundMu.Unlock()
		}()
		m.probeAllNodes(roundCtx, timeout)
	}(roundID)
}

func (m *Manager) cleanupOldHealthStats() {
	m.cleanupOldHealthStatsWithCtx(m.ctx)
}

func (m *Manager) cleanupOldHealthStatsWithCtx(baseCtx context.Context) {
	if !m.shouldPersistHealth() {
		return
	}
	if baseCtx == nil {
		baseCtx = context.Background()
	}

	// 保留最近 7 天（按 hour_ts 存储；这里用 UTC 截断到整点）
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour).Truncate(time.Hour)

	m.dbMu.Lock()
	db := m.db
	m.dbMu.Unlock()
	if db == nil {
		return
	}

	cctx, cancel := context.WithTimeout(baseCtx, 5*time.Second)
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
func (m *Manager) probeAllNodes(roundCtx context.Context, timeout time.Duration) {
	m.mu.RLock()
	entries := make([]*entry, 0, len(m.nodes))
	for _, e := range m.nodes {
		entries = append(entries, e)
	}
	m.mu.RUnlock()

	if len(entries) == 0 {
		if m.logger != nil {
			m.logger.Info("health check tick: 0 nodes registered, skipping probe")
		}
		return
	}

	// Randomize probe order each round to avoid fixed-order bias.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	rng.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })

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

scheduleLoop:
	for _, e := range entries {
		e.mu.RLock()
		probeFn := e.probe
		tag := e.info.Tag
		info := e.info
		e.mu.RUnlock()

		if probeFn == nil {
			continue
		}

		select {
		case sem <- struct{}{}:
		case <-roundCtx.Done():
			break scheduleLoop
		}
		if roundCtx.Err() != nil {
			<-sem
			break scheduleLoop
		}
		wg.Add(1)
		go func(entry *entry, probe probeFunc, tag string, info NodeInfo) {
			defer wg.Done()
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(roundCtx, timeout)
			latency, err := probe(ctx)
			cancel()

			// Persist health check into DB (hourly aggregation) if enabled.
			if m.shouldPersistHealth() {
				host, port, _, hpErr := store.HostPortFromURI(info.URI)
				if hpErr == nil && host != "" && port > 0 {
					var latencyMs *int64
					if err == nil {
						ms := latency.Milliseconds()
						if ms == 0 && latency > 0 {
							ms = 1
						}
						latencyMs = &ms
					}
					u := store.HealthCheckUpdate{
						Host:      host,
						Port:      port,
						Success:   err == nil,
						LatencyMs: latencyMs,
					}
					// Probe rounds are preemptible (a new round cancels the previous one). Persisting
					// health stats should not be coupled to the round context; otherwise DB stats may
					// be written but runtime availability recompute can be perpetually skipped.
					saveCtx, cancelSave := context.WithTimeout(m.ctx, 3*time.Second)
					_ = m.recordHealthCheck(saveCtx, u)
					cancelSave()
					// Apply DB-derived availability soon so pool scheduling reflects the latest stats.
					m.scheduleHealthRecompute()
				}
			}

			entry.mu.Lock()
			if err != nil {
				failedCount.Add(1)
				entry.lastError = err.Error()
				entry.lastFail = time.Now()
				// When DB is enabled, Available is derived only from DB (p95-5%) so do not set it here.
				if !m.shouldPersistHealth() {
					entry.available = false
					entry.initialCheckDone = true
				}
			} else {
				availableCount.Add(1)
				entry.lastOK = time.Now()
				entry.lastProbe = latency
				// When DB is enabled, Available is derived only from DB (p95-5%) so do not set it here.
				if !m.shouldPersistHealth() {
					entry.available = true
					entry.initialCheckDone = true
				}
			}
			entry.mu.Unlock()

			if err == nil && m.logger != nil {
				latencyMs := latency.Milliseconds()
				if latencyMs == 0 && latency > 0 {
					latencyMs = 1
				}
				m.logger.Info("probe success for ", tag, ", latency: ", latencyMs, "ms")
			}
			if err != nil && m.logger != nil {
				m.logger.Warn("probe failed for ", tag, ": ", err)
			}
		}(e, probeFn, tag, info)
	}
	wg.Wait()

	// If the round was cancelled (superseded by a new round / shutdown), do not apply final recompute/log.
	// Partial per-node updates and DB hourly rows already recorded are preserved.
	if roundCtx.Err() != nil {
		if m.logger != nil {
			m.logger.Warn("health check round cancelled: ", roundCtx.Err())
		}
		return
	}

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

// ResetRuntime cancels in-flight probes/periodic checks and clears runtime node registry.
// It keeps the DB connection and Config intact so the monitor HTTP server can continue serving.
func (m *Manager) ResetRuntime() {
	if m == nil {
		return
	}

	// Cancel any in-flight probes / periodic loops.
	if m.cancel != nil {
		m.cancel()
	}

	// Also cancel the current probe round (if any).
	m.probeRoundMu.Lock()
	if m.probeRoundCancel != nil {
		m.probeRoundCancel()
		m.probeRoundCancel = nil
		m.probeRoundActiveID = 0
	}
	m.probeRoundMu.Unlock()

	// Create a fresh context for subsequent periodic checks / DB ops.
	ctx, cancel := context.WithCancel(context.Background())
	m.ctx = ctx
	m.cancel = cancel

	// Clear registered nodes (new sing-box instance will re-register).
	m.mu.Lock()
	m.nodes = make(map[string]*entry)
	m.mu.Unlock()
}

// Stop stops the periodic health check.
// Shared store lifecycle is managed by app.Run and must not be closed here.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}

	m.probeRoundMu.Lock()
	if m.probeRoundCancel != nil {
		m.probeRoundCancel()
		m.probeRoundCancel = nil
		m.probeRoundActiveID = 0
	}
	m.probeRoundMu.Unlock()

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
			mgr:      m,
		}
		host, port, _, hpErr := store.HostPortFromURI(info.URI)
		if hpErr == nil && host != "" && port > 0 {
			e.host = host
			e.port = port
			e.hostPortValid = true
		}
		// In DB mode, "InitialCheckDone" is not a startup probe marker; scheduling is derived from DB stats.
		// Set it to true immediately so runtime gating effectively depends on DB-derived Available only.
		if m.shouldPersistHealth() {
			e.initialCheckDone = true
		}
		m.nodes[info.Tag] = e
	} else {
		e.info = info
		if e.mgr == nil {
			e.mgr = m
		}
		host, port, _, hpErr := store.HostPortFromURI(info.URI)
		if hpErr == nil && host != "" && port > 0 {
			e.host = host
			e.port = port
			e.hostPortValid = true
		} else {
			e.host = ""
			e.port = 0
			e.hostPortValid = false
		}
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

// ProbeHTTPInfo exposes the configured HTTP probe settings.
// - If probe_target has https scheme, UseTLS will be true and TLS handshake is required.
// - HostHeader is used for the HTTP Host header (may include port).
// - Path is the HTTP request path (includes query if configured).
func (m *Manager) ProbeHTTPInfo() (dst M.Socksaddr, useTLS bool, hostHeader, path, serverName string, insecure bool, ok bool) {
	if !m.probeReady {
		return M.Socksaddr{}, false, "", "", "", false, false
	}
	return m.probeDst, m.probeUseTLS, m.probeHostHeader, m.probePath, m.probeServerName, m.probeInsecure, true
}

const defaultProbePath = "/generate_204"

// parseProbeTarget parses probe_target as either:
// - URL: http(s)://host[:port][/path][?query]
// - host[:port] (treated as http, path defaultProbePath)
func parseProbeTarget(raw string, skipCertVerify bool) (dst M.Socksaddr, useTLS bool, hostHeader, serverName, path string, insecure bool, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return M.Socksaddr{}, false, "", "", "", false, false
	}

	// Prefer URL parsing when scheme is present.
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return M.Socksaddr{}, false, "", "", "", false, false
		}
		scheme := strings.ToLower(u.Scheme)
		switch scheme {
		case "http":
			useTLS = false
		case "https":
			useTLS = true
		default:
			// Unsupported scheme: do not enable probing.
			return M.Socksaddr{}, false, "", "", "", false, false
		}

		host := u.Hostname()
		if host == "" {
			return M.Socksaddr{}, false, "", "", "", false, false
		}
		portStr := u.Port()
		if portStr == "" {
			if useTLS {
				portStr = "443"
			} else {
				portStr = "80"
			}
		}

		dst = M.ParseSocksaddrHostPort(host, parsePort(portStr))

		// Host header: include explicit port only if present in original URL.
		if u.Port() != "" {
			hostHeader = net.JoinHostPort(host, u.Port())
		} else {
			hostHeader = host
		}

		serverName = host
		path = u.EscapedPath()
		if path == "" || path == "/" {
			path = defaultProbePath
		}
		if u.RawQuery != "" {
			path = path + "?" + u.RawQuery
		}
		insecure = skipCertVerify
		return dst, useTLS, hostHeader, serverName, path, insecure, true
	}

	// Fallback: host[:port] (http)
	target := raw
	// Strip any accidental path component.
	if idx := strings.Index(target, "/"); idx != -1 {
		target = target[:idx]
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		host = target
		port = "80"
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return M.Socksaddr{}, false, "", "", "", false, false
	}

	dst = M.ParseSocksaddrHostPort(host, parsePort(port))
	// Preserve port in Host header only when explicitly provided.
	if _, _, err := net.SplitHostPort(target); err == nil {
		hostHeader = target
	} else {
		hostHeader = host
	}
	serverName = host
	path = defaultProbePath
	insecure = skipCertVerify
	return dst, false, hostHeader, serverName, path, insecure, true
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
		TotalCount:        e.success + int64(e.failure),
		SuccessRate:       calcSuccessRate(e.success, e.failure),
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

func calcSuccessRate(success int64, failure int) float64 {
	total := success + int64(failure)
	if total <= 0 {
		return 0
	}
	return float64(success) / float64(total) * 100
}

func (e *entry) recordFailure(err error) {
	now := time.Now()

	e.mu.Lock()
	errStr := err.Error()
	e.failure++
	e.lastError = errStr
	e.lastFail = now
	e.appendTimelineLocked(false, 0, errStr)
	mgr := e.mgr
	uri := e.info.URI
	e.mu.Unlock()

	if mgr != nil && mgr.shouldPersistHealth() && mgr.throttleDBEvent(e) {
		host, port, _, hpErr := store.HostPortFromURI(uri)
		if hpErr == nil && host != "" && port > 0 {
			u := store.HealthCheckUpdate{
				Host:      host,
				Port:      port,
				Success:   false,
				LatencyMs: nil,
			}
			saveCtx, cancelSave := context.WithTimeout(mgr.ctx, 3*time.Second)
			_ = mgr.recordHealthCheck(saveCtx, u)
			cancelSave()
			mgr.scheduleHealthRecompute()
		}
	}
}

func (e *entry) recordSuccess() {
	now := time.Now()

	e.mu.Lock()
	e.success++
	e.lastOK = now
	e.appendTimelineLocked(true, 0, "")
	mgr := e.mgr
	uri := e.info.URI
	e.mu.Unlock()

	if mgr != nil && mgr.shouldPersistHealth() && mgr.throttleDBEvent(e) {
		host, port, _, hpErr := store.HostPortFromURI(uri)
		if hpErr == nil && host != "" && port > 0 {
			u := store.HealthCheckUpdate{
				Host:      host,
				Port:      port,
				Success:   true,
				LatencyMs: nil,
			}
			saveCtx, cancelSave := context.WithTimeout(mgr.ctx, 3*time.Second)
			_ = mgr.recordHealthCheck(saveCtx, u)
			cancelSave()
			mgr.scheduleHealthRecompute()
		}
	}
}

func (e *entry) recordSuccessWithLatency(latency time.Duration) {
	now := time.Now()

	e.mu.Lock()
	e.success++
	e.lastOK = now
	e.lastProbe = latency
	latencyMs := latency.Milliseconds()
	if latencyMs == 0 && latency > 0 {
		latencyMs = 1
	}
	e.appendTimelineLocked(true, latencyMs, "")
	mgr := e.mgr
	uri := e.info.URI
	e.mu.Unlock()

	if mgr != nil && mgr.shouldPersistHealth() && mgr.throttleDBEvent(e) {
		host, port, _, hpErr := store.HostPortFromURI(uri)
		if hpErr == nil && host != "" && port > 0 {
			ms := latencyMs
			u := store.HealthCheckUpdate{
				Host:      host,
				Port:      port,
				Success:   true,
				LatencyMs: &ms,
			}
			saveCtx, cancelSave := context.WithTimeout(mgr.ctx, 3*time.Second)
			_ = mgr.recordHealthCheck(saveCtx, u)
			cancelSave()
			mgr.scheduleHealthRecompute()
		}
	}
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
	// In DB mode, availability is derived only from DB (p95-5%), so ignore external writes.
	if h.ref.mgr != nil && h.ref.mgr.shouldPersistHealth() {
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
	// In DB mode, availability is derived only from DB (p95-5%), so ignore external writes.
	if h.ref.mgr != nil && h.ref.mgr.shouldPersistHealth() {
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

func (m *Manager) ListStoreNodes(ctx context.Context) ([]store.Node, error) {
	m.dbMu.Lock()
	db := m.db
	m.dbMu.Unlock()
	if db != nil {
		return db.ListNodes(ctx)
	}
	return nil, errors.New("store not available")
}

const (
	dbEventMinInterval         = 5 * time.Second
	healthRecomputeMinInterval = 5 * time.Second
)

func (m *Manager) throttleDBEvent(e *entry) bool {
	if m == nil || e == nil {
		return false
	}
	nowUnix := time.Now().Unix()
	prev := e.lastDBEventUnix.Load()
	if prev != 0 && time.Unix(prev, 0).Add(dbEventMinInterval).After(time.Now()) {
		return false
	}
	return e.lastDBEventUnix.CompareAndSwap(prev, nowUnix)
}

// scheduleHealthRecompute triggers a DB-based availability recompute (p95-5% threshold)
// after real-traffic health events are persisted, so runtime Available reflects DB quickly.
// It is throttled globally to avoid frequent full-table scans.
func (m *Manager) scheduleHealthRecompute() {
	if m == nil || !m.shouldPersistHealth() {
		return
	}
	nowUnix := time.Now().Unix()
	prev := m.lastHealthRecomputeUnix.Load()
	if prev != 0 && time.Unix(prev, 0).Add(healthRecomputeMinInterval).After(time.Now()) {
		return
	}
	if !m.lastHealthRecomputeUnix.CompareAndSwap(prev, nowUnix) {
		return
	}
	go func() {
		_ = m.applyHealthThresholdFromDB()
	}()
}

// RefreshHealthFromDB applies DB-derived health threshold (24h success-rate) to runtime entries.
// This makes API/UI availability reflect persisted health stats immediately after node registration,
// without waiting for the first periodic probe cycle.
func (m *Manager) RefreshHealthFromDB() {
	if m == nil {
		return
	}
	if !m.shouldPersistHealth() {
		return
	}
	_ = m.applyHealthThresholdFromDB()
}

func (m *Manager) recordHealthCheck(ctx context.Context, u store.HealthCheckUpdate) error {
	m.dbMu.Lock()
	db := m.db
	m.dbMu.Unlock()
	if db == nil {
		return nil
	}
	if err := db.RecordHealthCheck(ctx, u); err != nil {
		return err
	}
	return nil
}

// UpdateNodeEgressIP updates the persisted egress IP (public exit IP) for the node identified by URI.
// It is best-effort and is safe to call even when persistence is disabled.
func (m *Manager) UpdateNodeEgressIP(ctx context.Context, uri string, egressIP string) error {
	if m == nil {
		return nil
	}
	uri = strings.TrimSpace(uri)
	egressIP = strings.TrimSpace(egressIP)
	if uri == "" || egressIP == "" {
		return errors.New("uri/egress_ip is empty")
	}

	m.dbMu.Lock()
	db := m.db
	m.dbMu.Unlock()
	if db == nil {
		return nil
	}

	host, port, _, hpErr := store.HostPortFromURI(uri)
	if hpErr != nil || host == "" || port <= 0 {
		return errors.New("invalid uri host:port")
	}

	if ctx == nil {
		ctx = m.ctx
	}
	saveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := db.UpdateNodeEgressIP(saveCtx, host, port, egressIP, time.Now().UTC()); err != nil {
		return err
	}

	// Update in-memory cache opportunistically to avoid per-recompute DB point-gets.
	k := nodeRateKey(host, port)
	m.egressMu.Lock()
	if m.egressCache == nil {
		m.egressCache = make(map[string]egressCacheEntry)
	}
	m.egressCache[k] = egressCacheEntry{ip: egressIP, at: time.Now()}
	m.egressMu.Unlock()

	return nil
}

type nodeRate struct {
	host    string
	port    int
	rate    float64
	success int64
	fail    int64
}

func (m *Manager) applyHealthThresholdFromDB() error {
	if !m.healthApplyRunning.CompareAndSwap(false, true) {
		return nil
	}
	defer m.healthApplyRunning.Store(false)

	m.dbMu.Lock()
	db := m.db
	m.dbMu.Unlock()
	if db == nil {
		return nil
	}
	m.warmEgressCache(db)

	// Do not force refresh for every recompute: QueryNodeRatesSince scans Pebble hourly aggregates
	// and is expensive. Use cache and let refresh happen at most once per TTL.
	//
	// Cold-start safety: if cache returns 0 rows, do one forced refresh to avoid being stuck with
	// an empty cached window during the first TTL period after stats start being written.
	stats, err := m.load24hStatsCached(db, false)
	if err != nil {
		return err
	}
	if len(stats.rows) == 0 {
		stats, err = m.load24hStatsCached(db, true)
		if err != nil {
			return err
		}
	}
	rows := stats.rows
	if len(rows) == 0 {
		// DB enabled but no 24h stats => mark all nodes unavailable so only DB path determines availability.
		m.mu.RLock()
		entries := make([]*entry, 0, len(m.nodes))
		for _, e := range m.nodes {
			entries = append(entries, e)
		}
		m.mu.RUnlock()

		for _, e := range entries {
			e.mu.Lock()
			e.available = false
			e.initialCheckDone = true
			e.mu.Unlock()
		}
		return nil
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

	runtimeNodes := make([]runtimeNodeRef, 0, len(entries))
	for _, e := range entries {
		runtimeNodes = append(runtimeNodes, runtimeNodeRef{
			host:  e.host,
			port:  e.port,
			valid: e.hostPortValid,
		})
	}
	threshold, _, _, _, repByKey := computeDBThresholdForNodes(runtimeNodes, stats.rateByKey, func(host string, port int) (string, bool) {
		return m.getEgressIPForKey(m.ctx, host, port)
	})

	for _, e := range entries {
		e.mu.Lock()
		host, port := e.host, e.port
		if !e.hostPortValid || host == "" || port <= 0 {
			// No host:port mapping => cannot join 24h stats => not schedulable.
			e.available = false
			e.initialCheckDone = true
			e.mu.Unlock()
			continue
		}

		row, ok := stats.rateByKey[nodeRateKey(host, port)]
		if !ok {
			// No 24h stats => not schedulable.
			e.available = false
			e.initialCheckDone = true
			e.mu.Unlock()
			continue
		}

		total := row.success + row.fail
		if total < dbThresholdMinTotal {
			// Not enough samples => treat as not schedulable (equivalent to "no 24h stats").
			e.available = false
			e.initialCheckDone = true
			e.mu.Unlock()
			continue
		}

		k := nodeRateKey(host, port)
		if repByKey != nil && !repByKey[k] {
			// Same egress_ip group exists; only the representative node is schedulable.
			e.available = false
			e.initialCheckDone = true
			e.mu.Unlock()
			continue
		}

		e.available = row.rate >= threshold
		e.initialCheckDone = true
		e.mu.Unlock()
	}
	return nil
}

func dbInternalQueryRates(ctx context.Context, db store.Store, cutoff time.Time) ([]nodeRate, error) {
	rates, err := db.QueryNodeRatesSince(ctx, cutoff)
	if err != nil {
		return nil, err
	}
	out := make([]nodeRate, 0, len(rates))
	for _, r := range rates {
		out = append(out, nodeRate{
			host:    r.Host,
			port:    r.Port,
			rate:    r.Rate,
			success: r.SuccessCount,
			fail:    r.FailCount,
		})
	}
	return out, nil
}

func (m *Manager) load24hStatsCached(db store.Store, forceRefresh bool) (healthStatsCacheEntry, error) {
	if m == nil || db == nil {
		return healthStatsCacheEntry{}, nil
	}

	// forceRefresh is used by explicit refresh paths and remains blocking.
	if forceRefresh {
		return m.refresh24hStatsBlocking(db, true)
	}

	m.healthStatsCacheMu.RLock()
	cached := m.healthStatsCache
	m.healthStatsCacheMu.RUnlock()

	// Cold start: no cache yet. Block and wait for one single-flight refresh.
	if cached.at.IsZero() {
		return m.refresh24hStatsBlocking(db, false)
	}

	// Fresh cache: return immediately.
	if time.Since(cached.at) <= healthStatsCacheTTL {
		return cached, nil
	}

	// Stale cache: return immediately, refresh in background (single-flight).
	m.refresh24hStatsAsync(db)
	return cached, nil
}

func (m *Manager) refresh24hStatsAsync(db store.Store) {
	if m == nil || db == nil {
		return
	}
	if !m.healthStatsLoading.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer m.healthStatsLoading.Store(false)
		_, _ = m.refresh24hStatsBlocking(db, true)
	}()
}

func (m *Manager) refresh24hStatsBlocking(db store.Store, force bool) (healthStatsCacheEntry, error) {
	if m == nil || db == nil {
		return healthStatsCacheEntry{}, nil
	}

	m.healthStatsLoadMu.Lock()
	defer m.healthStatsLoadMu.Unlock()

	if !force {
		m.healthStatsCacheMu.RLock()
		cached := m.healthStatsCache
		m.healthStatsCacheMu.RUnlock()
		if !cached.at.IsZero() && time.Since(cached.at) <= healthStatsCacheTTL {
			return cached, nil
		}
	}

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	rows, err := dbInternalQueryRates(m.ctx, db, cutoff)
	if err != nil {
		return healthStatsCacheEntry{}, err
	}

	rateByKey := make(map[string]nodeRate, len(rows))
	rates := make([]float64, 0, len(rows))
	eligibleKeys := 0
	eligibleNonZero := 0
	for _, row := range rows {
		rateByKey[nodeRateKey(row.host, row.port)] = row
		total := row.success + row.fail
		if total >= dbThresholdMinTotal {
			eligibleKeys++
			if row.rate > 0 {
				eligibleNonZero++
			}
		}
		if row.rate > 0 {
			rates = append(rates, row.rate)
		}
	}

	p95 := 0.0
	if len(rates) > 0 {
		p95 = percentile(rates, 0.95)
	}
	threshold := p95 - 0.05
	if threshold < 0 {
		threshold = 0
	}
	if threshold > 1 {
		threshold = 1
	}

	healthByKey := make(map[string]bool, len(rows))
	for _, row := range rows {
		healthByKey[nodeRateKey(row.host, row.port)] = row.rate >= threshold
	}

	entry := healthStatsCacheEntry{
		at:              time.Now(),
		rows:            rows,
		rateByKey:       rateByKey,
		healthByKey:     healthByKey,
		threshold:       threshold,
		p95:             p95,
		eligibleKeys:    eligibleKeys,
		eligibleNonZero: eligibleNonZero,
	}

	m.healthStatsCacheMu.Lock()
	m.healthStatsCache = entry
	m.healthStatsCacheMu.Unlock()
	return entry, nil
}

// Attach24hStats annotates snapshots with DB-derived 24h success/failure stats.
func (m *Manager) Attach24hStats(snaps []Snapshot) []Snapshot {
	if len(snaps) == 0 || m == nil || !m.shouldPersistHealth() {
		return snaps
	}

	m.dbMu.Lock()
	db := m.db
	m.dbMu.Unlock()
	if db == nil {
		return snaps
	}
	m.warmEgressCache(db)

	stats, err := m.load24hStatsCached(db, false)
	if err != nil {
		return snaps
	}

	for i := range snaps {
		host, port, _, hpErr := store.HostPortFromURI(snaps[i].URI)
		if hpErr != nil || host == "" || port <= 0 {
			continue
		}
		k := nodeRateKey(host, port)
		// Best-effort: only read in-memory cache (avoid per-request DB lookups here).
		m.egressMu.RLock()
		if m.egressCache != nil {
			if v, ok := m.egressCache[k]; ok && v.ip != "" && time.Since(v.at) <= egressCacheTTL {
				snaps[i].EgressIP = v.ip
			}
		}
		m.egressMu.RUnlock()
		if row, ok := stats.rateByKey[k]; ok {
			total := row.success + row.fail
			snaps[i].DB24hSuccessCount = row.success
			snaps[i].DB24hFailureCount = row.fail
			snaps[i].DB24hTotalCount = total
			snaps[i].DB24hSuccessRate = row.rate * 100
			if total > 0 {
				snaps[i].DB24hFailureRate = float64(row.fail) / float64(total) * 100
			}
		}
	}

	return snaps
}

// Compute24hHealthMap computes DB-derived 24h health flags by host:port.
// health=true means rate >= threshold where threshold = p95(non-zero rates) - 0.05.
func (m *Manager) Compute24hHealthMap() map[string]bool {
	out := make(map[string]bool)
	if m == nil || !m.shouldPersistHealth() {
		return out
	}

	m.dbMu.Lock()
	db := m.db
	m.dbMu.Unlock()
	if db == nil {
		return out
	}

	stats, err := m.load24hStatsCached(db, false)
	if err != nil || len(stats.healthByKey) == 0 {
		return out
	}

	for k, v := range stats.healthByKey {
		out[k] = v
	}
	return out
}

func nodeRateKey(host string, port int) string {
	return host + ":" + strconv.Itoa(port)
}

func (m *Manager) getEgressIPForKey(ctx context.Context, host string, port int) (string, bool) {
	if m == nil {
		return "", false
	}
	host = strings.TrimSpace(host)
	if host == "" || port <= 0 {
		return "", false
	}
	k := nodeRateKey(host, port)

	m.egressMu.RLock()
	if m.egressCache != nil {
		if v, ok := m.egressCache[k]; ok && v.ip != "" && time.Since(v.at) <= egressCacheTTL {
			m.egressMu.RUnlock()
			return v.ip, true
		}
	}
	m.egressMu.RUnlock()

	m.dbMu.Lock()
	db := m.db
	m.dbMu.Unlock()
	if db == nil {
		return "", false
	}
	m.warmEgressCache(db)

	m.egressMu.RLock()
	if m.egressCache != nil {
		if v, ok := m.egressCache[k]; ok && v.ip != "" && time.Since(v.at) <= egressCacheTTL {
			m.egressMu.RUnlock()
			return v.ip, true
		}
	}
	m.egressMu.RUnlock()

	if ctx == nil {
		ctx = m.ctx
	}
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	n, ok, err := db.GetNodeByHostPort(qctx, host, port)
	if err != nil || !ok {
		return "", false
	}
	ip := strings.TrimSpace(n.EgressIP)
	if ip == "" {
		return "", false
	}

	m.egressMu.Lock()
	if m.egressCache == nil {
		m.egressCache = make(map[string]egressCacheEntry)
	}
	m.egressCache[k] = egressCacheEntry{ip: ip, at: time.Now()}
	m.egressMu.Unlock()
	return ip, true
}

func (m *Manager) warmEgressCache(db store.Store) {
	if m == nil || db == nil {
		return
	}

	m.egressMu.RLock()
	cacheWarm := len(m.egressCache) > 0
	cacheFresh := false
	if cacheWarm {
		for _, v := range m.egressCache {
			if v.ip != "" && time.Since(v.at) <= egressCacheTTL {
				cacheFresh = true
				break
			}
		}
	}
	m.egressMu.RUnlock()
	if cacheFresh {
		return
	}

	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	loadCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	nodes, err := db.ListActiveNodes(loadCtx)
	if err != nil {
		return
	}

	now := time.Now()
	cache := make(map[string]egressCacheEntry, len(nodes))
	for _, n := range nodes {
		ip := strings.TrimSpace(n.EgressIP)
		if ip == "" || n.Host == "" || n.Port <= 0 {
			continue
		}
		cache[nodeRateKey(n.Host, n.Port)] = egressCacheEntry{ip: ip, at: now}
	}
	if len(cache) == 0 {
		return
	}

	m.egressMu.Lock()
	if m.egressCache == nil {
		m.egressCache = make(map[string]egressCacheEntry, len(cache))
	}
	for k, v := range cache {
		m.egressCache[k] = v
	}
	m.egressMu.Unlock()
}

// computeDBThresholdForNodes computes the DB-mode scheduling threshold for the given runtime nodes.
// It implements "p95(non-zero) - 5%" but scopes percentile computation to the current runtime set
// and ignores low-sample keys to avoid stale/one-hit stats dominating threshold.
//
// When egressLookup is provided, nodes sharing the same egress IP are treated as a single group:
// only the representative (highest 24h success rate; tie-break by higher sample count) contributes
// to percentile computation and is considered schedulable.
func computeDBThresholdForNodes(
	nodes []runtimeNodeRef,
	rateByKey map[string]nodeRate,
	egressLookup func(host string, port int) (string, bool),
) (threshold, p95 float64, eligibleKeys, eligibleNonZero int, repByKey map[string]bool) {
	if len(nodes) == 0 || len(rateByKey) == 0 {
		return 1, 0, 0, 0, nil
	}

	type rep struct {
		key   string
		rate  float64
		total int64
	}

	groupRep := make(map[string]rep, len(nodes))
	seen := make(map[string]struct{}, len(nodes)) // host:port
	for _, node := range nodes {
		host, port := node.host, node.port
		if !node.valid || host == "" || port <= 0 {
			continue
		}
		k := nodeRateKey(host, port)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}

		row, ok := rateByKey[k]
		if !ok {
			continue
		}
		total := row.success + row.fail
		if total < dbThresholdMinTotal {
			continue
		}
		eligibleKeys++
		if row.rate > 0 {
			eligibleNonZero++
		}

		groupKey := k
		if egressLookup != nil {
			if ip, ok := egressLookup(host, port); ok {
				if ip = strings.TrimSpace(ip); ip != "" {
					groupKey = ip
				}
			}
		}

		cur, exists := groupRep[groupKey]
		if !exists {
			groupRep[groupKey] = rep{key: k, rate: row.rate, total: total}
			continue
		}

		if row.rate > cur.rate ||
			(row.rate == cur.rate && total > cur.total) ||
			(row.rate == cur.rate && total == cur.total && k < cur.key) {
			groupRep[groupKey] = rep{key: k, rate: row.rate, total: total}
		}
	}

	rates := make([]float64, 0, len(groupRep))
	repByKey = make(map[string]bool, len(groupRep))
	for _, r := range groupRep {
		repByKey[r.key] = true
		if r.rate > 0 {
			rates = append(rates, r.rate)
		}
	}

	// No eligible non-zero representative rates => schedule none.
	if len(rates) == 0 {
		return 1, 0, eligibleKeys, 0, repByKey
	}

	p95 = percentile(rates, 0.95)
	threshold = p95 - 0.05
	if threshold < 0 {
		threshold = 0
	}
	if threshold > 1 {
		threshold = 1
	}
	return threshold, p95, eligibleKeys, eligibleNonZero, repByKey
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
