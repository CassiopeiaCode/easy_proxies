package subscription

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/boxmgr"
	"easy_proxies/internal/config"
	"easy_proxies/internal/logx"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
)

const (
	storeWriteTimeout = 10 * time.Minute
	storeReadTimeout  = 120 * time.Second
	storeWriteBatch   = 100
)

// Logger defines logging interface.
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

// Manager handles periodic subscription refresh.
type Manager struct {
	mu sync.RWMutex

	baseCfg *config.Config
	boxMgr  *boxmgr.Manager
	store   store.Store
	logger  Logger

	status        monitor.SubscriptionStatus
	ctx           context.Context
	cancel        context.CancelFunc
	refreshMu     sync.Mutex // prevents concurrent refreshes
	manualRefresh chan struct{}

	// Track nodes.txt content hash to detect modifications
	lastSubHash      string    // Hash of nodes.txt content after last subscription refresh
	lastNodesModTime time.Time // Last known modification time of nodes.txt
}

// New creates a SubscriptionManager.
func New(cfg *config.Config, boxMgr *boxmgr.Manager, st store.Store, opts ...Option) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		baseCfg:       cfg,
		boxMgr:        boxMgr,
		store:         st,
		ctx:           ctx,
		cancel:        cancel,
		manualRefresh: make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.logger == nil {
		m.logger = defaultLogger{}
	}
	return m
}

// Start begins the periodic refresh loop.
func (m *Manager) Start() {
	if !m.baseCfg.SubscriptionRefresh.Enabled {
		m.logger.Infof("subscription refresh disabled")
		return
	}
	if len(m.baseCfg.Subscriptions) == 0 {
		m.logger.Infof("no subscriptions configured, refresh disabled")
		return
	}

	interval := m.baseCfg.SubscriptionRefresh.Interval
	m.logger.Infof("starting subscription refresh, interval: %s", interval)

	go m.refreshLoop(interval)

	// Bootstrap: trigger an immediate refresh asynchronously on startup,
	// so users don't have to wait for the first ticker interval.
	go m.doRefresh()
}

// Stop stops the periodic refresh.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

// RefreshNow triggers an immediate refresh.
func (m *Manager) RefreshNow() error {
	select {
	case m.manualRefresh <- struct{}{}:
	default:
		// Already a refresh pending
	}

	// Wait for refresh to complete or timeout
	timeout := m.baseCfg.SubscriptionRefresh.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(m.ctx, timeout+m.baseCfg.SubscriptionRefresh.HealthCheckTimeout)
	defer cancel()

	// Poll status until refresh completes
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	startCount := m.Status().RefreshCount
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("refresh timeout")
		case <-ticker.C:
			status := m.Status()
			if status.RefreshCount > startCount {
				if status.LastError != "" {
					return fmt.Errorf("refresh failed: %s", status.LastError)
				}
				return nil
			}
		}
	}
}

// Status returns the current refresh status.
func (m *Manager) Status() monitor.SubscriptionStatus {
	m.mu.RLock()
	status := m.status
	m.mu.RUnlock()

	// Check if nodes have been modified since last refresh
	status.NodesModified = m.CheckNodesModified()
	return status
}

// refreshLoop runs the periodic refresh.
func (m *Manager) refreshLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Update next refresh time
	m.mu.Lock()
	m.status.NextRefresh = time.Now().Add(interval)
	m.mu.Unlock()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.doRefresh()
			m.mu.Lock()
			m.status.NextRefresh = time.Now().Add(interval)
			m.mu.Unlock()
		case <-m.manualRefresh:
			m.doRefresh()
			// Reset ticker after manual refresh
			ticker.Reset(interval)
			m.mu.Lock()
			m.status.NextRefresh = time.Now().Add(interval)
			m.mu.Unlock()
		}
	}
}

// doRefresh performs a single refresh operation.
func (m *Manager) doRefresh() {
	// Prevent concurrent refreshes
	if !m.refreshMu.TryLock() {
		m.logger.Warnf("refresh already in progress, skipping")
		return
	}
	defer m.refreshMu.Unlock()

	m.mu.Lock()
	m.status.IsRefreshing = true
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.status.IsRefreshing = false
		m.status.RefreshCount++
		m.mu.Unlock()
	}()

	m.logger.Infof("starting subscription refresh")

	// Fetch nodes from all subscriptions
	nodes, err := m.fetchAllSubscriptions()
	if err != nil {
		m.logger.Errorf("fetch subscriptions failed: %v", err)
		m.mu.Lock()
		m.status.LastError = err.Error()
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		return
	}

	if len(nodes) == 0 {
		m.logger.Warnf("no nodes fetched from subscriptions")
		m.mu.Lock()
		m.status.LastError = "no nodes fetched"
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		return
	}

	m.logger.Infof("fetched %d nodes from subscriptions", len(nodes))

	// Store is the source of truth:
	// - upsert nodes by host:port (dedup)
	// - then reload strictly from store active nodes
	// If store write/read fails, do not reload.
	if m.store == nil {
		m.mu.Lock()
		m.status.LastError = "store unavailable: skip reload"
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		m.logger.Errorf("store unavailable, skip reload")
		return
	}

	upCtx, cancelUp := context.WithTimeout(m.ctx, storeWriteTimeout)
	defer cancelUp()

	inputs := make([]store.UpsertNodeInput, 0, len(nodes))
	invalidCount := 0
	invalidExamples := make([]string, 0, 5)
	for _, n := range nodes {
		if _, _, _, hpErr := store.HostPortFromURI(n.URI); hpErr != nil {
			invalidCount++
			if len(invalidExamples) < cap(invalidExamples) {
				invalidExamples = append(invalidExamples, n.URI)
			}
			continue
		}
		inputs = append(inputs, store.UpsertNodeInput{
			URI:  n.URI,
			Name: n.Name,
		})
	}
	if invalidCount > 0 {
		m.logger.Warnf("drop invalid nodes before store upsert: %d (examples=%v)", invalidCount, invalidExamples)
	}
	if len(inputs) == 0 {
		m.mu.Lock()
		m.status.LastError = "store write failed: 0 valid nodes after filtering"
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		m.logger.Errorf("store write failed, skip reload: no valid nodes")
		return
	}

	successCount := 0
	failCount := 0
	for i := 0; i < len(inputs); i += storeWriteBatch {
		if upCtx.Err() != nil {
			m.mu.Lock()
			m.status.LastError = fmt.Sprintf("store write failed: %v", upCtx.Err())
			m.status.LastRefresh = time.Now()
			m.mu.Unlock()
			m.logger.Errorf("store write failed, skip reload: %v", upCtx.Err())
			return
		}
		
		end := i + storeWriteBatch
		if end > len(inputs) {
			end = len(inputs)
		}
		chunk := inputs[i:end]
		upErr := m.store.UpsertNodesByHostPortBatch(upCtx, chunk)
		if upErr == nil {
			successCount += len(chunk)
			m.logger.Infof("store upsert partially success: %d nodes", successCount)
			continue
		}

		// Degrade gracefully on chunk failure: isolate bad records and keep progress.
		m.logger.Warnf("store batch upsert failed for chunk [%d,%d), fallback to per-node: %v", i, end, upErr)
		for _, in := range chunk {
			if upCtx.Err() != nil {
				break
			}
			if _, oneErr := m.store.UpsertNodeByHostPort(upCtx, in); oneErr != nil {
				failCount++
				continue
			}
			successCount++
		}
	}

	if successCount == 0 {
		m.mu.Lock()
		m.status.LastError = "store write failed: 0 nodes written"
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		m.logger.Errorf("store write failed, skip reload: 0 nodes written")
		return
	}
	if failCount > 0 {
		m.logger.Warnf("store upsert partial success: success=%d fail=%d", successCount, failCount)
	} else {
		m.logger.Infof("store upsert success: %d nodes", successCount)
	}

	readStart := time.Now()
	loadCtx, cancelLoad := context.WithTimeout(m.ctx, storeReadTimeout)
	active, listErr := m.store.ListActiveNodes(loadCtx)
	cancelLoad()
	if listErr != nil {
		m.mu.Lock()
		m.status.LastError = fmt.Sprintf("store read failed: %v", listErr)
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		m.logger.Errorf("store read failed, skip reload: %v (timeout=%s, elapsed=%s)", listErr, storeReadTimeout, time.Since(readStart))
		return
	}
	m.logger.Infof("store read active nodes completed: %d nodes in %s", len(active), time.Since(readStart))
	if len(active) == 0 {
		m.mu.Lock()
		m.status.LastError = "store read returned 0 active nodes: skip reload"
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		m.logger.Warnf("store read returned 0 active nodes, skip reload")
		return
	}

	merged := make([]config.NodeConfig, 0, len(active))
	for _, n := range active {
		merged = append(merged, config.NodeConfig{
			Name: n.Name,
			URI:  n.URI,
		})
	}

	// nodes.txt is read-only in this repo. Do not write any cache file.
	// Still update hash/mod time so UI can show a stable "not modified" state.
	newHash := m.computeNodesHash(merged)
	m.mu.Lock()
	m.lastSubHash = newHash
	m.lastNodesModTime = time.Now()
	m.status.NodesModified = false
	m.mu.Unlock()

	// Get current port mapping to preserve existing node ports
	portMap := m.boxMgr.CurrentPortMap()

	// Create new config with updated nodes
	newCfg := m.createNewConfig(merged)

	// Trigger BoxManager reload with port preservation
	if err := m.boxMgr.ReloadWithPortMap(newCfg, portMap); err != nil {
		m.logger.Errorf("reload failed: %v", err)
		m.mu.Lock()
		m.status.LastError = err.Error()
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		return
	}

	m.mu.Lock()
	m.status.LastRefresh = time.Now()
	m.status.NodeCount = len(merged)
	m.status.LastError = ""
	m.mu.Unlock()

	m.logger.Infof("subscription refresh completed, %d nodes active", len(merged))
}

// getNodesFilePath returns the path to nodes.txt.
func (m *Manager) getNodesFilePath() string {
	if m.baseCfg.NodesFile != "" {
		return m.baseCfg.NodesFile
	}
	return filepath.Join(filepath.Dir(m.baseCfg.FilePath()), "nodes.txt")
}

// writeNodesToFile writes nodes to a file (one URI per line).
func (m *Manager) writeNodesToFile(path string, nodes []config.NodeConfig) error {
	var lines []string
	for _, node := range nodes {
		lines = append(lines, node.URI)
	}
	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// computeNodesHash computes a hash of node URIs for change detection.
func (m *Manager) computeNodesHash(nodes []config.NodeConfig) string {
	var uris []string
	for _, node := range nodes {
		uris = append(uris, node.URI)
	}
	content := strings.Join(uris, "\n")
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:])
}

// CheckNodesModified checks if nodes.txt has been modified since last refresh.
// Uses file modification time as a fast path to avoid unnecessary file reads.
func (m *Manager) CheckNodesModified() bool {
	m.mu.RLock()
	lastHash := m.lastSubHash
	lastMod := m.lastNodesModTime
	m.mu.RUnlock()

	if lastHash == "" {
		return false // No previous refresh, can't determine modification
	}

	nodesFilePath := m.getNodesFilePath()

	// Fast path: check modification time first
	info, err := os.Stat(nodesFilePath)
	if err != nil {
		return false // File doesn't exist or can't stat
	}
	modTime := info.ModTime()
	if !modTime.After(lastMod) {
		return false // File hasn't been modified
	}

	// Slow path: file was modified, compute hash
	data, err := os.ReadFile(nodesFilePath)
	if err != nil {
		return false // File doesn't exist or can't read
	}

	// Parse nodes from file content
	var nodes []config.NodeConfig
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if isProxyURI(line) {
			nodes = append(nodes, config.NodeConfig{URI: line})
		}
	}

	currentHash := m.computeNodesHash(nodes)
	changed := currentHash != lastHash

	// Update cached mod time
	m.mu.Lock()
	m.lastNodesModTime = modTime
	m.mu.Unlock()

	return changed
}

// MarkNodesModified updates the modification status.
func (m *Manager) MarkNodesModified() {
	m.mu.Lock()
	m.status.NodesModified = true
	m.mu.Unlock()
}

// fetchAllSubscriptions fetches nodes from all configured subscription URLs.
func (m *Manager) fetchAllSubscriptions() ([]config.NodeConfig, error) {
	var allNodes []config.NodeConfig
	var lastErr error

	timeout := m.baseCfg.SubscriptionRefresh.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	for _, subURL := range m.baseCfg.Subscriptions {
		nodes, sizeBytes, errorNodes, err := m.fetchSubscription(subURL, timeout)
		if err != nil {
			m.logger.Warnf("sub_refresh_result url=%q size_bytes=%d parsed_nodes=%d error_nodes=%d err=%q", subURL, sizeBytes, 0, 0, err.Error())
			m.logger.Warnf("failed to fetch %s: %v", subURL, err)
			lastErr = err
			continue
		}
		m.logger.Infof("sub_refresh_result url=%q size_bytes=%d parsed_nodes=%d error_nodes=%d", subURL, sizeBytes, len(nodes), errorNodes)
		m.logger.Infof("fetched %d nodes from subscription", len(nodes))
		allNodes = append(allNodes, nodes...)
	}

	if len(allNodes) == 0 && lastErr != nil {
		return nil, lastErr
	}

	return allNodes, nil
}

// fetchSubscription fetches and parses a single subscription URL.
func (m *Manager) fetchSubscription(subURL string, timeout time.Duration) ([]config.NodeConfig, int, int, error) {
	ctx, cancel := context.WithTimeout(m.ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", subURL, nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "*/*")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, 0, 0, fmt.Errorf("status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("read body: %w", err)
	}

	nodes, stats, err := config.ParseSubscriptionContentWithStats(string(body))
	if err != nil {
		return nil, len(body), 0, err
	}
	return nodes, len(body), stats.ErrorNodes, nil
}

// createNewConfig creates a new config with updated nodes while preserving other settings.
func (m *Manager) createNewConfig(nodes []config.NodeConfig) *config.Config {
	// Deep copy base config
	newCfg := *m.baseCfg

	// Assign port numbers to nodes in multi-port mode
	if newCfg.Mode == "multi-port" {
		portCursor := newCfg.MultiPort.BasePort
		for i := range nodes {
			nodes[i].Port = portCursor
			portCursor++
			// Apply default credentials
			if nodes[i].Username == "" {
				nodes[i].Username = newCfg.MultiPort.Username
				nodes[i].Password = newCfg.MultiPort.Password
			}
		}
	}

	// Process node names
	for i := range nodes {
		nodes[i].Name = strings.TrimSpace(nodes[i].Name)
		nodes[i].URI = strings.TrimSpace(nodes[i].URI)

		// Extract name from URI fragment if not provided
		if nodes[i].Name == "" {
			if parsed, err := url.Parse(nodes[i].URI); err == nil && parsed.Fragment != "" {
				if decoded, err := url.QueryUnescape(parsed.Fragment); err == nil {
					nodes[i].Name = decoded
				} else {
					nodes[i].Name = parsed.Fragment
				}
			}
		}
		if nodes[i].Name == "" {
			nodes[i].Name = fmt.Sprintf("node-%d", i)
		}
	}

	newCfg.Nodes = nodes
	return &newCfg
}

// isProxyURI is used for nodes.txt modification detection (fast parsing path).
// Keep it in sync with config's supported schemes so "NodesModified" UI hint is accurate.
func isProxyURI(s string) bool {
	schemes := []string{
		"vmess://",
		"vless://",
		"trojan://",
		"ss://",
		"ssr://",
		"hysteria://",
		"hysteria2://",
		"hy2://",
		"socks://",
		"socks5://",
		"socks4://",
		"socks4a://",
		"http://",
		"https://",
	}
	lower := strings.ToLower(strings.TrimSpace(s))
	// Support "URI 备注..." lines: only look at the first token.
	if fields := strings.Fields(lower); len(fields) > 0 {
		lower = fields[0]
	}
	for _, scheme := range schemes {
		if strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	return false
}

type defaultLogger struct{}

func (defaultLogger) Infof(format string, args ...any) {
	logx.Printf("[subscription] "+format, args...)
}

func (defaultLogger) Warnf(format string, args ...any) {
	logx.Printf("[subscription] WARN: "+format, args...)
}

func (defaultLogger) Errorf(format string, args ...any) {
	logx.Printf("[subscription] ERROR: "+format, args...)
}
