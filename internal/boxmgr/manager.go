package boxmgr

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/builder"
	"easy_proxies/internal/config"
	"easy_proxies/internal/logx"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/outbound/pool"
	"easy_proxies/internal/store"

	"github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
)

// Ensure Manager implements monitor.NodeManager.
var _ monitor.NodeManager = (*Manager)(nil)

const (
	defaultDrainTimeout       = 10 * time.Second
	defaultHealthCheckTimeout = 30 * time.Second
	healthCheckPollInterval   = 500 * time.Millisecond
	periodicHealthInterval    = 5 * time.Minute
	periodicHealthTimeout     = 10 * time.Second
	rebuildHardTimeout        = 120 * time.Second
	maxRebuildTimeoutRetries  = 10
	maxSingBoxNodes           = 5000
	healthyNodeRatio          = 0.9
)

// Logger defines logging interface for the manager.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Option configures the Manager.
type Option func(*Manager)

// WithLogger sets a custom logger.
func WithLogger(l Logger) Option {
	return func(m *Manager) { m.logger = l }
}

// Manager owns the lifecycle of the active sing-box instance.
type Manager struct {
	mu sync.RWMutex

	currentBox    *box.Box
	monitorMgr    *monitor.Manager
	monitorServer *monitor.Server
	cfg           *config.Config
	monitorCfg    monitor.Config
	store         store.Store

	drainTimeout      time.Duration
	minAvailableNodes int
	logger            Logger

	baseCtx            context.Context
	healthCheckStarted bool
}

// New creates a BoxManager with the given config.
// Store lifecycle is managed by app.Run and injected here.
func New(cfg *config.Config, monitorCfg monitor.Config, st store.Store, opts ...Option) *Manager {
	m := &Manager{
		cfg:        cfg,
		monitorCfg: monitorCfg,
		store:      st,
	}
	m.applyConfigSettings(cfg)
	for _, opt := range opts {
		opt(m)
	}
	if m.logger == nil {
		m.logger = defaultLogger{}
	}
	if m.drainTimeout <= 0 {
		m.drainTimeout = defaultDrainTimeout
	}
	return m
}

// Start creates and starts the initial sing-box instance.
func (m *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.ensureMonitor(ctx); err != nil {
		return err
	}

	m.mu.Lock()
	if m.cfg == nil {
		m.mu.Unlock()
		return errors.New("box manager requires config")
	}
	if m.currentBox != nil {
		m.mu.Unlock()
		return errors.New("sing-box already running")
	}
	m.applyConfigSettings(m.cfg)
	m.baseCtx = ctx
	cfg := m.cfg
	m.mu.Unlock()

	// Try to start, with automatic port conflict resolution.
	// Additionally, if sing-box fails during outbound initialization (e.g. "initialize outbound[N]: ... unknown ..."),
	// we will best-effort mark that node as damaged and retry quickly.
	var instance *box.Box
	maxDamagedRetries := 50
	maxRetries := maxDamagedRetries + 10
	damagedRetries := 0
	timeoutRetries := 0
	var lastErr error
	started := false
	for retry := 0; retry < maxRetries; retry++ {
		// Hard-timeout each rebuild attempt so one stuck create/start cannot block forever.
		var err error
		var timedOut bool
		instance, err, timedOut = m.createAndStartWithTimeout(ctx, cfg, rebuildHardTimeout)
		if timedOut {
			timeoutRetries++
			m.logger.Warnf("create sing-box timed out (attempt %d/%d), hard-timeout=%s", timeoutRetries, maxRebuildTimeoutRetries, rebuildHardTimeout)
			if timeoutRetries >= maxRebuildTimeoutRetries {
				m.logger.Errorf("create sing-box timed out %d times, exiting with code 1", timeoutRetries)
				os.Exit(1)
			}
			lastErr = fmt.Errorf("create sing-box timed out after %s", rebuildHardTimeout)
			pool.ResetSharedStateStore()
			continue
		}
		if err != nil {
			// If this looks like an outbound init failure, createBox() already did best-effort damaged marking.
			// Retry to allow builder to skip damaged nodes on the next Build().
			if extractSingBoxInitOutboundIndex(err) >= 0 && damagedRetries < maxDamagedRetries {
				damagedRetries++
				m.logger.Warnf("create sing-box failed (init outbound), retrying after marking damaged (attempt %d/%d): %v", damagedRetries, maxDamagedRetries, err)
				pool.ResetSharedStateStore()
				time.Sleep(200 * time.Millisecond)
				continue
			}
			// Keep existing behavior for bind conflicts: reassign and retry.
			if conflictPort := extractPortFromBindError(err); conflictPort > 0 {
				m.logger.Warnf("port %d is in use, reassigning and retrying...", conflictPort)
				if reassigned := reassignConflictingPort(cfg, conflictPort); reassigned {
					pool.ResetSharedStateStore()
					continue
				}
			}
			lastErr = err
			return lastErr
		}
		started = true
		break // Success
	}
	if !started || instance == nil {
		if lastErr == nil {
			lastErr = errors.New("failed to create/start sing-box after retries")
		}
		return lastErr
	}

	m.mu.Lock()
	m.currentBox = instance
	m.mu.Unlock()

	// sing-box 的 outbound 初始化/Start 流程与我们这里的控制流并非严格同步：
	// 即使 instance.Start() 返回成功，monitor 注册可能还在后续阶段发生。
	// 如果我们过早启动健康检查或统计可用节点，会出现 0/0 的假象。
	m.waitForMonitorRegistration(30 * time.Second)

	// Apply persisted health stats to runtime entries immediately after registration.
	m.mu.RLock()
	if m.monitorMgr != nil {
		m.monitorMgr.RefreshHealthFromDB()
	}
	m.mu.RUnlock()

	// Start periodic health check after nodes are registered
	m.mu.Lock()
	if m.monitorMgr != nil && !m.healthCheckStarted {
		m.monitorMgr.StartPeriodicHealthCheck(periodicHealthInterval, periodicHealthTimeout)
		m.healthCheckStarted = true
	}
	m.mu.Unlock()

	// Wait for initial health check if min nodes configured
	if cfg.SubscriptionRefresh.MinAvailableNodes > 0 {
		timeout := cfg.SubscriptionRefresh.HealthCheckTimeout
		if timeout <= 0 {
			timeout = defaultHealthCheckTimeout
		}
		if err := m.waitForHealthCheck(timeout); err != nil {
			m.logger.Warnf("initial health check warning: %v", err)
			// Don't fail startup, just warn
		}
	}

	m.logger.Infof("sing-box instance started with %d nodes", len(cfg.Nodes))
	return nil
}

// Reload gracefully switches to a new configuration.
// For multi-port mode, we must stop the old instance first to release ports.
func (m *Manager) Reload(newCfg *config.Config) error {
	if newCfg == nil {
		return errors.New("new config is nil")
	}

	m.mu.Lock()
	if m.currentBox == nil {
		m.mu.Unlock()
		return errors.New("manager not started")
	}
	ctx := m.baseCtx
	oldBox := m.currentBox
	oldCfg := m.cfg
	m.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}

	m.logger.Infof("reloading with %d nodes", len(newCfg.Nodes))

	// Phase 1: create new box while old instance is still running.
	// This avoids hard interruptions where create/build hangs or times out.
	// On outbound init failures, best-effort mark damaged and retry quickly.
	var instance *box.Box
	maxDamagedRetries := 50
	maxRetries := maxDamagedRetries + 10
	damagedRetries := 0
	timeoutRetries := 0
	var lastErr error
	created := false
	for retry := 0; retry < maxRetries; retry++ {
		// Hard-timeout each rebuild attempt so one stuck create cannot block forever.
		var err error
		var timedOut bool
		instance, err, timedOut = m.createWithTimeout(ctx, newCfg, rebuildHardTimeout)
		if timedOut {
			timeoutRetries++
			m.logger.Warnf("create new box timed out (attempt %d/%d), hard-timeout=%s", timeoutRetries, maxRebuildTimeoutRetries, rebuildHardTimeout)
			if timeoutRetries >= maxRebuildTimeoutRetries {
				lastErr = fmt.Errorf("create new box timed out %d times, hard-timeout=%s", timeoutRetries, rebuildHardTimeout)
				break
			}
			pool.ResetSharedStateStore()
			continue
		}
		if err != nil {
			if extractSingBoxInitOutboundIndex(err) >= 0 && damagedRetries < maxDamagedRetries {
				damagedRetries++
				m.logger.Warnf("create new box failed (init outbound), retrying after marking damaged (attempt %d/%d): %v", damagedRetries, maxDamagedRetries, err)
				pool.ResetSharedStateStore()
				time.Sleep(200 * time.Millisecond)
				continue
			}
			lastErr = err
			break
		}
		created = true
		break // Success
	}
	if !created || instance == nil {
		if lastErr == nil {
			lastErr = errors.New("failed to create new box after retries")
		}
		return fmt.Errorf("create new box: %w", lastErr)
	}

	// Phase 2: cutover window. Stop old box to release ports, then start the prepared instance.
	// Any failure here must rollback to old config.
	if oldBox != nil {
		m.logger.Infof("stopping old instance to release ports...")
		if err := oldBox.Close(); err != nil {
			m.logger.Warnf("error closing old instance: %v", err)
		}
		// Give OS time to release ports
		time.Sleep(500 * time.Millisecond)
	}

	// Reload 期间如果 monitor 正在健康检查，会继续用旧 probe/旧 outbound 产生大量失败与脏数据。
	// 在切换窗口再重置，避免构建失败时影响旧实例运行期监控。
	m.mu.Lock()
	if m.monitorMgr != nil {
		m.monitorMgr.ResetRuntime()
		m.healthCheckStarted = false
	}
	m.currentBox = nil // Mark as switching
	m.mu.Unlock()

	startErr, startTimedOut := m.startWithTimeout(ctx, instance, rebuildHardTimeout)
	if startTimedOut {
		_ = instance.Close()
		m.rollbackToOldConfig(ctx, oldCfg)
		return fmt.Errorf("start new box timed out after %s", rebuildHardTimeout)
	}
	if startErr != nil {
		_ = instance.Close()
		// Keep existing behavior for bind conflicts: reassign and retry on full reload path.
		// Here old box is already closed, so rollback first to keep service available.
		m.rollbackToOldConfig(ctx, oldCfg)
		return fmt.Errorf("start new box: %w", startErr)
	}

	m.applyConfigSettings(newCfg)

	m.mu.Lock()
	m.currentBox = instance
	m.cfg = newCfg
	m.mu.Unlock()

	// Reload 后同样需要等待新实例完成注册，再启动周期健康检查，避免 0/0 与空列表。
	m.waitForMonitorRegistration(30 * time.Second)

	// Apply persisted health stats to runtime entries immediately after registration.
	m.mu.RLock()
	if m.monitorMgr != nil {
		m.monitorMgr.RefreshHealthFromDB()
	}
	m.mu.RUnlock()

	m.mu.Lock()
	if m.monitorMgr != nil && !m.healthCheckStarted {
		m.monitorMgr.StartPeriodicHealthCheck(periodicHealthInterval, periodicHealthTimeout)
		m.healthCheckStarted = true
	}
	m.mu.Unlock()

	m.logger.Infof("reload completed successfully with %d nodes", len(newCfg.Nodes))
	return nil
}

// rollbackToOldConfig attempts to restart with the previous configuration.
func (m *Manager) rollbackToOldConfig(ctx context.Context, oldCfg *config.Config) {
	if oldCfg == nil {
		return
	}
	m.logger.Warnf("attempting rollback to previous config...")
	instance, err := m.createBox(ctx, oldCfg)
	if err != nil {
		m.logger.Errorf("rollback failed to create box: %v", err)
		return
	}
	if err := instance.Start(); err != nil {
		_ = instance.Close()
		m.logger.Errorf("rollback failed to start box: %v", err)
		return
	}
	m.mu.Lock()
	m.currentBox = instance
	m.cfg = oldCfg
	m.mu.Unlock()
	m.logger.Infof("rollback successful")
}

// Close terminates the active instance and auxiliary components.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var err error
	if m.currentBox != nil {
		err = m.currentBox.Close()
		m.currentBox = nil
	}
	if m.monitorServer != nil {
		m.monitorServer.Shutdown(context.Background())
		m.monitorServer = nil
	}
	if m.monitorMgr != nil {
		m.monitorMgr.Stop()
		m.monitorMgr = nil
		m.healthCheckStarted = false
	}
	pool.SetDefaultMonitorManager(nil)
	m.baseCtx = nil
	return err
}

// MonitorManager returns the shared monitor manager.
func (m *Manager) MonitorManager() *monitor.Manager {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.monitorMgr
}

// MonitorServer returns the monitor HTTP server.
func (m *Manager) MonitorServer() *monitor.Server {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.monitorServer
}

// createBox builds a sing-box instance from config.
func (m *Manager) createBox(ctx context.Context, cfg *config.Config) (*box.Box, error) {
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	if m.monitorMgr == nil {
		return nil, errors.New("monitor manager not initialized")
	}

	effectiveCfg := *cfg
	effectiveCfg.Nodes = m.selectNodesForSingBox(ctx, cfg.Nodes)
	if len(effectiveCfg.Nodes) != len(cfg.Nodes) && m.logger != nil {
		m.logger.Infof("node load limited for sing-box: selected=%d total=%d", len(effectiveCfg.Nodes), len(cfg.Nodes))
	}

	opts, err := builder.Build(&effectiveCfg, m.store)
	if err != nil {
		return nil, fmt.Errorf("build sing-box options: %w", err)
	}

	inboundRegistry := include.InboundRegistry()
	outboundRegistry := include.OutboundRegistry()
	pool.Register(outboundRegistry)
	endpointRegistry := include.EndpointRegistry()
	dnsRegistry := include.DNSTransportRegistry()
	serviceRegistry := include.ServiceRegistry()

	// Pre-register nodes so monitor APIs can see them even if sing-box does not instantiate
	// our pool outbounds early (or context values are dropped internally).
	m.preRegisterMonitorNodes(&effectiveCfg)

	boxCtx := box.Context(ctx, inboundRegistry, outboundRegistry, endpointRegistry, dnsRegistry, serviceRegistry)
	boxCtx = monitor.ContextWith(boxCtx, m.monitorMgr)

	instance, err := box.New(box.Options{Context: boxCtx, Options: opts})
	if err != nil {
		// Best-effort: if sing-box fails while initializing outbounds, try to map the failing outbound
		// to a node URI and mark it as damaged, so next run can start with remaining nodes.
		m.tryMarkDamagedFromSingBoxInitError(&effectiveCfg, opts, err)
		return nil, fmt.Errorf("create sing-box instance: %w", err)
	}
	return instance, nil
}

func (m *Manager) selectNodesForSingBox(ctx context.Context, nodes []config.NodeConfig) []config.NodeConfig {
	if len(nodes) <= maxSingBoxNodes {
		return cloneNodes(nodes)
	}

	healthySet, err := m.computeHealthyNodeSet(ctx, nodes)
	if err != nil && m.logger != nil {
		m.logger.Warnf("compute healthy node set failed, fallback to first %d nodes: %v", maxSingBoxNodes, err)
	}

	healthyNodes := make([]config.NodeConfig, 0, len(nodes))
	otherNodes := make([]config.NodeConfig, 0, len(nodes))
	for _, node := range nodes {
		host, port, _, hpErr := store.HostPortFromURI(node.URI)
		if hpErr == nil {
			if _, ok := healthySet[nodeHostPortKey(host, port)]; ok {
				healthyNodes = append(healthyNodes, node)
				continue
			}
		}
		otherNodes = append(otherNodes, node)
	}

	healthyQuota := int(float64(maxSingBoxNodes) * healthyNodeRatio)
	if healthyQuota < 0 {
		healthyQuota = 0
	}
	if healthyQuota > maxSingBoxNodes {
		healthyQuota = maxSingBoxNodes
	}

	selectedHealthy := minInt(len(healthyNodes), healthyQuota)
	selectedOther := minInt(len(otherNodes), maxSingBoxNodes-selectedHealthy)

	selected := make([]config.NodeConfig, 0, maxSingBoxNodes)
	selected = append(selected, healthyNodes[:selectedHealthy]...)
	selected = append(selected, otherNodes[:selectedOther]...)

	remaining := maxSingBoxNodes - len(selected)
	if remaining > 0 && len(healthyNodes) > selectedHealthy {
		extra := minInt(remaining, len(healthyNodes)-selectedHealthy)
		selected = append(selected, healthyNodes[selectedHealthy:selectedHealthy+extra]...)
		remaining -= extra
	}
	if remaining > 0 && len(otherNodes) > selectedOther {
		extra := minInt(remaining, len(otherNodes)-selectedOther)
		selected = append(selected, otherNodes[selectedOther:selectedOther+extra]...)
	}

	if len(selected) > maxSingBoxNodes {
		selected = selected[:maxSingBoxNodes]
	}
	return selected
}

func (m *Manager) computeHealthyNodeSet(ctx context.Context, nodes []config.NodeConfig) (map[string]struct{}, error) {
	healthy := make(map[string]struct{})
	if len(nodes) == 0 || m.store == nil {
		return healthy, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	queryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	rows, err := m.store.QueryNodeRatesSince(queryCtx, cutoff)
	if err != nil {
		return healthy, err
	}
	if len(rows) == 0 {
		return healthy, nil
	}

	rates := make([]float64, 0, len(rows))
	rateByKey := make(map[string]float64, len(rows))
	for _, row := range rows {
		if row.Rate > 0 {
			rates = append(rates, row.Rate)
		}
		rateByKey[nodeHostPortKey(row.Host, row.Port)] = row.Rate
	}

	p95 := 0.0
	if len(rates) > 0 {
		p95 = percentileFloat(rates, 0.95)
	}
	threshold := p95 - 0.05
	if threshold < 0 {
		threshold = 0
	}
	if threshold > 1 {
		threshold = 1
	}

	for _, node := range nodes {
		host, port, _, hpErr := store.HostPortFromURI(node.URI)
		if hpErr != nil || host == "" || port <= 0 {
			continue
		}
		if rate, ok := rateByKey[nodeHostPortKey(host, port)]; ok && rate >= threshold {
			healthy[nodeHostPortKey(host, port)] = struct{}{}
		}
	}
	return healthy, nil
}

func nodeHostPortKey(host string, port int) string {
	return fmt.Sprintf("%s:%d", strings.TrimSpace(host), port)
}

func percentileFloat(values []float64, q float64) float64 {
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// gracefulSwitch swaps the current box with a new one.
func (m *Manager) gracefulSwitch(newBox *box.Box) error {
	if newBox == nil {
		return errors.New("new box is nil")
	}

	m.mu.Lock()
	old := m.currentBox
	m.currentBox = newBox
	drainTimeout := m.drainTimeout
	m.mu.Unlock()

	if old != nil {
		go m.drainOldBox(old, drainTimeout)
	}

	m.logger.Infof("switched to new instance, draining old for %s", drainTimeout)
	return nil
}

// drainOldBox waits for drain timeout then closes the old box.
func (m *Manager) drainOldBox(oldBox *box.Box, timeout time.Duration) {
	if oldBox == nil {
		return
	}
	if timeout > 0 {
		time.Sleep(timeout)
	}
	if err := oldBox.Close(); err != nil {
		m.logger.Errorf("failed to close old instance: %v", err)
		return
	}
	m.logger.Infof("old instance closed after %s drain", timeout)
}

// waitForHealthCheck polls until enough nodes are available or timeout.
func (m *Manager) waitForHealthCheck(timeout time.Duration) error {
	if m.monitorMgr == nil || m.minAvailableNodes <= 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = defaultHealthCheckTimeout
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(healthCheckPollInterval)
	defer ticker.Stop()

	for {
		available, total := m.availableNodeCount()
		if available >= m.minAvailableNodes {
			m.logger.Infof("health check passed: %d/%d nodes available", available, total)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout: %d/%d nodes available (need >= %d)", available, total, m.minAvailableNodes)
		}
		<-ticker.C
	}
}

func (m *Manager) waitForMonitorRegistration(timeout time.Duration) {
	m.mu.RLock()
	mgr := m.monitorMgr
	m.mu.RUnlock()
	if mgr == nil {
		return
	}

	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		total := len(mgr.Snapshot())
		if total > 0 {
			m.logger.Infof("monitor registered %d nodes", total)
			return
		}
		if time.Now().After(deadline) {
			m.logger.Warnf("monitor node registration not observed within %s (still 0 nodes)", timeout)
			return
		}
		<-ticker.C
	}
}

// availableNodeCount returns (available, total) node counts.
func (m *Manager) availableNodeCount() (int, int) {
	if m.monitorMgr == nil {
		return 0, 0
	}
	snapshots := m.monitorMgr.Snapshot()
	total := len(snapshots)
	available := 0
	for _, snap := range snapshots {
		if snap.InitialCheckDone && snap.Available {
			available++
		}
	}
	return available, total
}

// ensureMonitor initializes monitor manager and server if needed.
func (m *Manager) ensureMonitor(ctx context.Context) error {
	m.mu.Lock()
	if m.monitorMgr != nil {
		m.mu.Unlock()
		return nil
	}

	monitorMgr, err := monitor.NewManager(m.monitorCfg, m.store)
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("init monitor manager: %w", err)
	}
	monitorMgr.SetLogger(monitorLoggerAdapter{logger: m.logger})
	m.monitorMgr = monitorMgr
	pool.SetDefaultMonitorManager(monitorMgr)

	var serverToStart *monitor.Server
	if m.monitorCfg.Enabled {
		if m.monitorServer == nil {
			serverToStart = monitor.NewServer(m.monitorCfg, monitorMgr, log.Default())
			m.monitorServer = serverToStart
		}
		// Set NodeManager for config CRUD endpoints
		if m.monitorServer != nil {
			m.monitorServer.SetNodeManager(m)
		}
		// Note: StartPeriodicHealthCheck is called after nodes are registered in Start()
	}
	m.mu.Unlock()

	if serverToStart != nil {
		serverToStart.Start(ctx)
	}
	return nil
}

// applyConfigSettings extracts runtime settings from config.
func (m *Manager) applyConfigSettings(cfg *config.Config) {
	if cfg == nil {
		return
	}
	if cfg.SubscriptionRefresh.DrainTimeout > 0 {
		m.drainTimeout = cfg.SubscriptionRefresh.DrainTimeout
	} else if m.drainTimeout == 0 {
		m.drainTimeout = defaultDrainTimeout
	}
	m.minAvailableNodes = cfg.SubscriptionRefresh.MinAvailableNodes
}

// defaultLogger is the fallback logger using standard log.
type defaultLogger struct{}

func (defaultLogger) Infof(format string, args ...any) {
	logx.Printf("[boxmgr] "+format, args...)
}

func (defaultLogger) Warnf(format string, args ...any) {
	logx.Printf("[boxmgr] WARN: "+format, args...)
}

func (defaultLogger) Errorf(format string, args ...any) {
	logx.Printf("[boxmgr] ERROR: "+format, args...)
}

// monitorLoggerAdapter adapts Logger to monitor.Logger interface.
type monitorLoggerAdapter struct {
	logger Logger
}

func (a monitorLoggerAdapter) Info(args ...any) {
	if a.logger != nil {
		a.logger.Infof("%s", fmt.Sprint(args...))
	}
}

func (a monitorLoggerAdapter) Warn(args ...any) {
	if a.logger != nil {
		a.logger.Warnf("%s", fmt.Sprint(args...))
	}
}

// --- NodeManager interface implementation ---

var errConfigUnavailable = errors.New("config is not initialized")

// ListConfigNodes returns a copy of all configured nodes.
func (m *Manager) ListConfigNodes(ctx context.Context) ([]config.NodeConfig, error) {
	_ = ctx
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.cfg == nil {
		return nil, errConfigUnavailable
	}
	return cloneNodes(m.cfg.Nodes), nil
}

// CreateNode adds a new node to the config and saves it.
func (m *Manager) CreateNode(ctx context.Context, node config.NodeConfig) (config.NodeConfig, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return config.NodeConfig{}, err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cfg == nil {
		return config.NodeConfig{}, errConfigUnavailable
	}

	normalized, err := m.prepareNodeLocked(node, "")
	if err != nil {
		return config.NodeConfig{}, err
	}

	// Determine source: if subscriptions exist, new nodes go to nodes.txt (subscription source)
	// Otherwise, if nodes_file exists, use file source; else inline
	if len(m.cfg.Subscriptions) > 0 {
		normalized.Source = config.NodeSourceSubscription
	} else if m.cfg.NodesFile != "" {
		normalized.Source = config.NodeSourceFile
	} else {
		normalized.Source = config.NodeSourceInline
	}

	// Persist node into shared store.
	if m.store != nil {
		upCtx, cancelUp := context.WithTimeout(context.Background(), 5*time.Second)
		_, upErr := m.store.UpsertNodeByHostPort(upCtx, store.UpsertNodeInput{URI: normalized.URI, Name: normalized.Name})
		cancelUp()
		if upErr != nil {
			return config.NodeConfig{}, fmt.Errorf("upsert node to store: %w", upErr)
		}
	}

	m.cfg.Nodes = append(m.cfg.Nodes, normalized)
	if err := m.cfg.Save(); err != nil {
		m.cfg.Nodes = m.cfg.Nodes[:len(m.cfg.Nodes)-1]
		return config.NodeConfig{}, fmt.Errorf("save config: %w", err)
	}
	return normalized, nil
}

// UpdateNode updates an existing node by name and saves the config.
func (m *Manager) UpdateNode(ctx context.Context, name string, node config.NodeConfig) (config.NodeConfig, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return config.NodeConfig{}, err
		}
	}

	name = strings.TrimSpace(name)
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cfg == nil {
		return config.NodeConfig{}, errConfigUnavailable
	}

	idx := m.nodeIndexLocked(name)
	if idx == -1 {
		return config.NodeConfig{}, monitor.ErrNodeNotFound
	}

	normalized, err := m.prepareNodeLocked(node, name)
	if err != nil {
		return config.NodeConfig{}, err
	}

	// Preserve the original source
	normalized.Source = m.cfg.Nodes[idx].Source

	prev := m.cfg.Nodes[idx]

	// Persist updates into shared store.
	// If host:port changed, delete the old record and upsert the new one.
	if m.store != nil {
		// Best-effort delete old key if URI changed.
		if strings.TrimSpace(prev.URI) != "" && strings.TrimSpace(prev.URI) != strings.TrimSpace(normalized.URI) {
			delCtx, cancelDel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = m.store.DeleteNodeByURI(delCtx, prev.URI)
			cancelDel()
		}

		upCtx, cancelUp := context.WithTimeout(context.Background(), 5*time.Second)
		_, upErr := m.store.UpsertNodeByHostPort(upCtx, store.UpsertNodeInput{URI: normalized.URI, Name: normalized.Name})
		cancelUp()
		if upErr != nil {
			return config.NodeConfig{}, fmt.Errorf("upsert node to store: %w", upErr)
		}
	}

	m.cfg.Nodes[idx] = normalized
	if err := m.cfg.Save(); err != nil {
		m.cfg.Nodes[idx] = prev
		return config.NodeConfig{}, fmt.Errorf("save config: %w", err)
	}
	return normalized, nil
}

// DeleteNode removes a node by name and saves the config.
func (m *Manager) DeleteNode(ctx context.Context, name string) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	name = strings.TrimSpace(name)
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cfg == nil {
		return errConfigUnavailable
	}

	idx := m.nodeIndexLocked(name)
	if idx == -1 {
		return monitor.ErrNodeNotFound
	}

	uriToDelete := m.cfg.Nodes[idx].URI

	// Delete from shared store.
	if m.store != nil && strings.TrimSpace(uriToDelete) != "" {
		delCtx, cancelDel := context.WithTimeout(context.Background(), 5*time.Second)
		delErr := m.store.DeleteNodeByURI(delCtx, uriToDelete)
		cancelDel()
		if delErr != nil {
			return fmt.Errorf("delete node from store: %w", delErr)
		}
	}

	backup := cloneNodes(m.cfg.Nodes)
	m.cfg.Nodes = append(m.cfg.Nodes[:idx], m.cfg.Nodes[idx+1:]...)
	if err := m.cfg.Save(); err != nil {
		m.cfg.Nodes = backup
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

// TriggerReload reloads the sing-box instance with current config.
func (m *Manager) TriggerReload(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	m.mu.RLock()
	cfgCopy := m.copyConfigLocked()
	portMap := m.cfg.BuildPortMap() // Preserve existing port assignments
	m.mu.RUnlock()

	if cfgCopy == nil {
		return errConfigUnavailable
	}
	return m.ReloadWithPortMap(cfgCopy, portMap)
}

// ReloadWithPortMap gracefully switches to a new configuration, preserving port assignments.
func (m *Manager) ReloadWithPortMap(newCfg *config.Config, portMap map[string]uint16) error {
	if newCfg == nil {
		return errors.New("new config is nil")
	}

	// Apply port mapping to preserve existing node ports
	if portMap != nil && len(portMap) > 0 {
		if err := newCfg.NormalizeWithPortMap(portMap); err != nil {
			return fmt.Errorf("normalize config with port map: %w", err)
		}
	}

	return m.Reload(newCfg)
}

// CurrentPortMap returns the current port mapping from the active configuration.
func (m *Manager) CurrentPortMap() map[string]uint16 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cfg == nil {
		return nil
	}
	return m.cfg.BuildPortMap()
}

// --- Helper functions ---

var (
	// portBindErrorRegex matches "listen tcp4 0.0.0.0:24282: bind: address already in use"
	portBindErrorRegex = regexp.MustCompile(`listen tcp[46]? [^:]+:(\d+): bind: address already in use`)

	// singBoxInitOutboundIdxRegex matches sing-box init errors like:
	// "initialize outbound[107]: create client transport: none: unknown transport type: none"
	singBoxInitOutboundIdxRegex = regexp.MustCompile(`initialize outbound\[(\d+)\]`)
)

// extractPortFromBindError extracts the port number from a bind error message.
func extractPortFromBindError(err error) uint16 {
	if err == nil {
		return 0
	}
	matches := portBindErrorRegex.FindStringSubmatch(err.Error())
	if len(matches) < 2 {
		return 0
	}
	var port int
	fmt.Sscanf(matches[1], "%d", &port)
	if port > 0 && port <= 65535 {
		return uint16(port)
	}
	return 0
}

// isPortAvailable checks if a port is available for binding.
func isPortAvailable(address string, port uint16) bool {
	addr := fmt.Sprintf("%s:%d", address, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func extractSingBoxInitOutboundIndex(err error) int {
	if err == nil {
		return -1
	}
	matches := singBoxInitOutboundIdxRegex.FindStringSubmatch(err.Error())
	if len(matches) < 2 {
		return -1
	}
	var idx int
	_, _ = fmt.Sscanf(matches[1], "%d", &idx)
	if idx < 0 {
		return -1
	}
	return idx
}

func sanitizeTag(name string) string {
	lower := strings.ToLower(name)
	lower = strings.TrimSpace(lower)
	if lower == "" {
		return ""
	}
	segments := strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	result := strings.Join(segments, "-")
	result = strings.Trim(result, "-")
	return result
}

// buildTagToURIMap reconstructs the outbound tag assignment used by builder.Build() for node outbounds.
// It must stay consistent with builder's sanitizeTag + uniqueness rules.
func buildTagToURIMap(cfg *config.Config) map[string]string {
	out := make(map[string]string, len(cfg.Nodes))
	usedTags := make(map[string]int)

	for _, node := range cfg.Nodes {
		baseTag := sanitizeTag(node.Name)
		if baseTag == "" {
			baseTag = fmt.Sprintf("node-%d", len(out)+1)
		}

		tag := baseTag
		if count, exists := usedTags[baseTag]; exists {
			usedTags[baseTag] = count + 1
			tag = fmt.Sprintf("%s-%d", baseTag, count+1)
		} else {
			usedTags[baseTag] = 1
		}
		out[tag] = node.URI
	}
	return out
}

func (m *Manager) preRegisterMonitorNodes(cfg *config.Config) {
	if m == nil || m.monitorMgr == nil || cfg == nil {
		return
	}

	usedTags := make(map[string]int)
	for i := range cfg.Nodes {
		node := cfg.Nodes[i]

		baseTag := sanitizeTag(node.Name)
		if baseTag == "" {
			baseTag = fmt.Sprintf("node-%d", i+1)
		}

		tag := baseTag
		if count, exists := usedTags[baseTag]; exists {
			usedTags[baseTag] = count + 1
			tag = fmt.Sprintf("%s-%d", baseTag, count+1)
		} else {
			usedTags[baseTag] = 1
		}

		info := monitor.NodeInfo{
			Tag:  tag,
			Name: node.Name,
			URI:  node.URI,
			Mode: cfg.Mode,
		}

		if cfg.Mode == "multi-port" || cfg.Mode == "hybrid" {
			info.ListenAddress = cfg.MultiPort.Address
			info.Port = node.Port
		} else {
			info.ListenAddress = cfg.Listener.Address
			info.Port = cfg.Listener.Port
		}

		_ = m.monitorMgr.Register(info)
	}
}

func outboundTagAtIndex(opts any, idx int) string {
	if idx < 0 {
		return ""
	}
	v := reflect.ValueOf(opts)
	if !v.IsValid() {
		return ""
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	outbounds := v.FieldByName("Outbounds")
	if !outbounds.IsValid() || outbounds.Kind() != reflect.Slice || idx >= outbounds.Len() {
		return ""
	}
	ob := outbounds.Index(idx)
	if ob.Kind() == reflect.Pointer {
		if ob.IsNil() {
			return ""
		}
		ob = ob.Elem()
	}
	if ob.Kind() != reflect.Struct {
		return ""
	}
	tagField := ob.FieldByName("Tag")
	if !tagField.IsValid() || tagField.Kind() != reflect.String {
		return ""
	}
	return tagField.String()
}

func (m *Manager) tryMarkDamagedFromSingBoxInitError(cfg *config.Config, opts any, cause error) {
	if cfg == nil || cause == nil || m.store == nil {
		return
	}

	idx := extractSingBoxInitOutboundIndex(cause)
	if idx < 0 {
		return
	}

	tag := outboundTagAtIndex(opts, idx)
	if tag == "" {
		return
	}

	// Only mark if the tag belongs to a node outbound (not pool/proxy-pool-* etc.).
	tagToURI := buildTagToURIMap(cfg)
	rawURI := strings.TrimSpace(tagToURI[tag])
	if rawURI == "" {
		return
	}

	host, port, _, err := store.HostPortFromURI(rawURI)
	if err != nil || host == "" || port <= 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = m.store.MarkNodeDamaged(ctx, host, port, cause.Error())
}

// reassignConflictingPort finds the node using the conflicting port and assigns a new port.
func reassignConflictingPort(cfg *config.Config, conflictPort uint16) bool {
	// Build set of used ports
	usedPorts := make(map[uint16]bool)
	if cfg.Mode == "hybrid" {
		usedPorts[cfg.Listener.Port] = true
	}
	for _, node := range cfg.Nodes {
		usedPorts[node.Port] = true
	}

	// Find and reassign the conflicting node
	for idx := range cfg.Nodes {
		if cfg.Nodes[idx].Port == conflictPort {
			// Find next available port
			newPort := conflictPort + 1
			address := cfg.MultiPort.Address
			if address == "" {
				address = "0.0.0.0"
			}
			for usedPorts[newPort] || !isPortAvailable(address, newPort) {
				newPort++
				if newPort > 65535 {
					logx.Printf("❌ No available port found for node %q", cfg.Nodes[idx].Name)
					return false
				}
			}
			logx.Printf("⚠️  Port %d in use, reassigning node %q to port %d", conflictPort, cfg.Nodes[idx].Name, newPort)
			cfg.Nodes[idx].Port = newPort
			return true
		}
	}
	return false
}

func cloneNodes(nodes []config.NodeConfig) []config.NodeConfig {
	if len(nodes) == 0 {
		return []config.NodeConfig{} // Return empty slice, not nil, for proper JSON serialization
	}
	out := make([]config.NodeConfig, len(nodes))
	copy(out, nodes)
	return out
}

func (m *Manager) createAndStartWithTimeout(ctx context.Context, cfg *config.Config, timeout time.Duration) (*box.Box, error, bool) {
	instance, err, timedOut := m.createWithTimeout(ctx, cfg, timeout)
	if err != nil || timedOut {
		return nil, err, timedOut
	}
	startErr, startTimedOut := m.startWithTimeout(ctx, instance, timeout)
	if startErr != nil {
		_ = instance.Close()
		return nil, startErr, startTimedOut
	}
	return instance, nil, false
}

func (m *Manager) createWithTimeout(ctx context.Context, cfg *config.Config, timeout time.Duration) (*box.Box, error, bool) {
	if timeout <= 0 {
		timeout = rebuildHardTimeout
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		instance *box.Box
		err      error
	}
	done := make(chan result, 1)

	go func() {
		instance, err := m.createBox(attemptCtx, cfg)
		done <- result{instance: instance, err: err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			return nil, r.err, false
		}
		return r.instance, nil, false
	case <-attemptCtx.Done():
		if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("create timed out after %s", timeout), true
		}
		return nil, attemptCtx.Err(), false
	}
}

func (m *Manager) startWithTimeout(ctx context.Context, instance *box.Box, timeout time.Duration) (error, bool) {
	if instance == nil {
		return errors.New("instance is nil"), false
	}
	if timeout <= 0 {
		timeout = rebuildHardTimeout
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- instance.Start()
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("start sing-box: %w", err), false
		}
		return nil, false
	case <-attemptCtx.Done():
		if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("start timed out after %s", timeout), true
		}
		return attemptCtx.Err(), false
	}
}

func (m *Manager) copyConfigLocked() *config.Config {
	if m.cfg == nil {
		return nil
	}
	cloned := *m.cfg
	cloned.Nodes = cloneNodes(m.cfg.Nodes)
	cloned.SetFilePath(m.cfg.FilePath())
	return &cloned
}

func (m *Manager) nodeIndexLocked(name string) int {
	for idx, node := range m.cfg.Nodes {
		if node.Name == name {
			return idx
		}
	}
	return -1
}

func (m *Manager) portInUseLocked(port uint16, currentName string) bool {
	if port == 0 {
		return false
	}
	for _, node := range m.cfg.Nodes {
		if node.Name == currentName {
			continue
		}
		if node.Port == port {
			return true
		}
	}
	return false
}

func (m *Manager) nextAvailablePortLocked() uint16 {
	base := m.cfg.MultiPort.BasePort
	if base == 0 {
		base = 24000
	}
	used := make(map[uint16]struct{}, len(m.cfg.Nodes))
	for _, node := range m.cfg.Nodes {
		if node.Port > 0 {
			used[node.Port] = struct{}{}
		}
	}
	port := base
	for i := 0; i < 1<<16; i++ {
		if _, ok := used[port]; !ok && port != 0 {
			return port
		}
		port++
		if port == 0 {
			port = 1
		}
	}
	return base
}

func (m *Manager) prepareNodeLocked(node config.NodeConfig, currentName string) (config.NodeConfig, error) {
	node.Name = strings.TrimSpace(node.Name)
	node.URI = strings.TrimSpace(node.URI)

	if node.URI == "" {
		return config.NodeConfig{}, fmt.Errorf("%w: URI 不能为空", monitor.ErrInvalidNode)
	}

	// Extract name from URI fragment (#name) if not provided
	if node.Name == "" {
		if currentName != "" {
			node.Name = currentName
		} else if idx := strings.LastIndex(node.URI, "#"); idx != -1 && idx < len(node.URI)-1 {
			// Extract and URL-decode the fragment
			fragment := node.URI[idx+1:]
			if decoded, err := url.QueryUnescape(fragment); err == nil && decoded != "" {
				node.Name = decoded
			}
		}
		// Fallback to auto-generated name
		if node.Name == "" {
			node.Name = fmt.Sprintf("node-%d", len(m.cfg.Nodes)+1)
		}
	}

	// Check for name conflict (excluding current node when updating)
	if idx := m.nodeIndexLocked(node.Name); idx != -1 {
		if currentName == "" || m.cfg.Nodes[idx].Name != currentName {
			return config.NodeConfig{}, fmt.Errorf("%w: 节点 %s 已存在", monitor.ErrNodeConflict, node.Name)
		}
	}

	// Handle multi-port mode specifics
	if m.cfg.Mode == "multi-port" {
		if node.Port == 0 {
			node.Port = m.nextAvailablePortLocked()
		} else if m.portInUseLocked(node.Port, currentName) {
			return config.NodeConfig{}, fmt.Errorf("%w: 端口 %d 已被占用", monitor.ErrNodeConflict, node.Port)
		}
		if node.Username == "" {
			node.Username = m.cfg.MultiPort.Username
			node.Password = m.cfg.MultiPort.Password
		}
	}

	return node, nil
}
