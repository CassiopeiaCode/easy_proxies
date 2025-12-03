package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"easy_proxies/internal/builder"
	"easy_proxies/internal/config"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/outbound/pool"
	"easy_proxies/internal/state"

	"github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
)

// stdLogger adapts standard log to monitor.Logger interface
type stdLogger struct{}

func (l *stdLogger) Info(args ...any) {
	log.Println(append([]any{"[health-check] "}, args...)...)
}

func (l *stdLogger) Warn(args ...any) {
	log.Println(append([]any{"[health-check] ⚠️ "}, args...)...)
}

const maxLoggedNodeNames = 5

// summarizeNodeNames returns a short list of node names for logging purposes.
func summarizeNodeNames(nodes []config.NodeConfig, indices []int) string {
	names := make([]string, 0, len(indices))
	for _, idx := range indices {
		if idx < 0 || idx >= len(nodes) {
			continue
		}
		name := nodes[idx].Name
		if name == "" {
			name = fmt.Sprintf("node-%d", idx)
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return ""
	}
	if len(names) > maxLoggedNodeNames {
		remaining := len(names) - maxLoggedNodeNames
		names = append(names[:maxLoggedNodeNames], fmt.Sprintf("... and %d more", remaining))
	}
	return strings.Join(names, ", ")
}

// removeNodesByIndices removes nodes at the provided indices (sorted desc) and returns the updated slice.
func removeNodesByIndices(nodes []config.NodeConfig, sortedDescending []int) []config.NodeConfig {
	for _, idx := range sortedDescending {
		if idx < 0 || idx >= len(nodes) {
			continue
		}
		nodes = append(nodes[:idx], nodes[idx+1:]...)
	}
	return nodes
}

// nodesByIndices returns node copies for indices.
func nodesByIndices(nodes []config.NodeConfig, sortedDescending []int) []config.NodeConfig {
	result := make([]config.NodeConfig, 0, len(sortedDescending))
	for _, idx := range sortedDescending {
		if idx < 0 || idx >= len(nodes) {
			continue
		}
		result = append(result, nodes[idx])
	}
	return result
}

// uniqueSortedDescending returns unique indices sorted from high to low.
func uniqueSortedDescending(indices []int) []int {
	if len(indices) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, len(indices))
	result := make([]int, 0, len(indices))
	for _, idx := range indices {
		if idx < 0 {
			continue
		}
		if _, exists := seen[idx]; exists {
			continue
		}
		seen[idx] = struct{}{}
		result = append(result, idx)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i] > result[j]
	})
	return result
}

// mapOutboundIndicesToNodeIndices converts outbound indices to node indices using builder metadata.
func mapOutboundIndicesToNodeIndices(result builder.Result, outboundIndices []int) []int {
	if len(outboundIndices) == 0 {
		return nil
	}
	nodeIndexSet := make(map[int]struct{})
	for _, outboundIdx := range outboundIndices {
		if outboundIdx < 0 || outboundIdx >= len(result.BaseTags) {
			continue
		}
		tag := result.BaseTags[outboundIdx]
		if nodeIdx, ok := result.TagToNodeIndex[tag]; ok {
			nodeIndexSet[nodeIdx] = struct{}{}
		}
	}
	if len(nodeIndexSet) == 0 {
		return nil
	}
	nodeIndices := make([]int, 0, len(nodeIndexSet))
	for idx := range nodeIndexSet {
		nodeIndices = append(nodeIndices, idx)
	}
	sort.Slice(nodeIndices, func(i, j int) bool {
		return nodeIndices[i] > nodeIndices[j]
	})
	return nodeIndices
}

// batchRemoveFailedNodes removes multiple failed nodes from config at once
// Returns true if any nodes were removed
func batchRemoveFailedNodes(err error, cfg *config.Config, result builder.Result) ([]config.NodeConfig, bool) {
	outboundIndices := builder.ExtractOutboundIndices(err)
	if len(outboundIndices) == 0 {
		return nil, false
	}
	nodeIndices := mapOutboundIndicesToNodeIndices(result, outboundIndices)
	if len(nodeIndices) == 0 {
		return nil, false
	}
	if summary := summarizeNodeNames(cfg.Nodes, nodeIndices); summary != "" {
		log.Printf("⚠️  Removing unstable nodes after initialization failure: %s", summary)
	}
	removedNodes := nodesByIndices(cfg.Nodes, nodeIndices)
	cfg.Nodes = removeNodesByIndices(cfg.Nodes, nodeIndices)
	log.Printf("⚠️  Removed %d failed nodes, %d nodes remaining", len(nodeIndices), len(cfg.Nodes))
	return removedNodes, true
}

// Run builds the runtime components from config and blocks until shutdown.
func Run(ctx context.Context, cfg *config.Config) error {
	// 根据模式选择代理用户名密码
	proxyUsername := cfg.Listener.Username
	proxyPassword := cfg.Listener.Password
	if cfg.Mode == "multi-port" {
		proxyUsername = cfg.MultiPort.Username
		proxyPassword = cfg.MultiPort.Password
	}

	stateStore, err := state.Open(cfg.StateFile, cfg.StateFlushInterval)
	if err != nil {
		return fmt.Errorf("init state store: %w", err)
	}
	defer stateStore.Close()
	stateStore.Start(ctx)

	monitorCfg := monitor.Config{
		Enabled:       cfg.ManagementEnabled(),
		Listen:        cfg.Management.Listen,
		ProbeTarget:   cfg.Management.ProbeTarget,
		Password:      cfg.Management.Password,
		ProxyUsername: proxyUsername,
		ProxyPassword: proxyPassword,
		ExternalIP:    cfg.ExternalIP,
	}
	monitorMgr, err := monitor.NewManager(monitorCfg, stateStore)
	if err != nil {
		return fmt.Errorf("init monitor: %w", err)
	}

	baseCtx := ctx
	var (
		instance     *box.Box
		instanceMu   sync.Mutex
		currentNodes []config.NodeConfig
	)

	currentNodes = append([]config.NodeConfig(nil), cfg.Nodes...)
	inst, nodesInUse, err := startBoxInstance(baseCtx, cfg, monitorMgr, stateStore, currentNodes)
	if err != nil {
		return err
	}
	instanceMu.Lock()
	instance = inst
	currentNodes = nodesInUse
	cfg.Nodes = append([]config.NodeConfig(nil), nodesInUse...)
	instanceMu.Unlock()

	var monitorServer *monitor.Server
	if monitorCfg.Enabled {
		monitorServer = monitor.NewServer(monitorCfg, monitorMgr, log.Default())
		monitorServer.Start(ctx)
		defer monitorServer.Shutdown(context.Background())

		monitorMgr.SetLogger(&stdLogger{})
		// 启动定期健康检查（每5分钟检查一次，每个节点超时5秒）
		monitorMgr.StartPeriodicHealthCheck(5*time.Minute, 5*time.Second)
		defer monitorMgr.Stop()
	}

	startSubscriptionRefresher(ctx, baseCtx, cfg, monitorMgr, stateStore, &instance, &currentNodes, &instanceMu)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-ctx.Done():
	case sig := <-sigCh:
		fmt.Printf("received %s, shutting down\n", sig)
	}

	instanceMu.Lock()
	defer instanceMu.Unlock()
	if instance != nil {
		return instance.Close()
	}
	return nil
}

func startBoxInstance(baseCtx context.Context, cfg *config.Config, monitorMgr *monitor.Manager, stateStore *state.Store, nodes []config.NodeConfig) (*box.Box, []config.NodeConfig, error) {
	workingCfg := *cfg
	filteredNodes, skipped := filterBlockedNodes(nodes, stateStore)
	if skipped > 0 {
		log.Printf("⚠️  跳过 %d 个已禁用节点，继续使用 %d 个节点", skipped, len(filteredNodes))
	}
	if len(filteredNodes) == 0 {
		return nil, nil, fmt.Errorf("no nodes available after filtering disabled entries")
	}
	workingCfg.Nodes = append([]config.NodeConfig(nil), filteredNodes...)
	for {
		buildResult, err := builder.Build(&workingCfg)
		if err != nil {
			return nil, nil, err
		}

		if len(buildResult.FailedIndices) > 0 {
			indices := uniqueSortedDescending(buildResult.FailedIndices)
			log.Printf("⚠️  Removing %d nodes that failed during build phase", len(indices))
			if summary := summarizeNodeNames(workingCfg.Nodes, indices); summary != "" {
				log.Printf("    Affected nodes: %s", summary)
			}
			disableNodes(stateStore, nodesByIndices(workingCfg.Nodes, indices), time.Time{})
			workingCfg.Nodes = removeNodesByIndices(workingCfg.Nodes, indices)
			log.Printf("⚠️  %d nodes remaining after removing build failures", len(workingCfg.Nodes))
			continue
		}

		inboundRegistry := include.InboundRegistry()
		outboundRegistry := include.OutboundRegistry()
		pool.Register(outboundRegistry)
		endpointRegistry := include.EndpointRegistry()
		dnsRegistry := include.DNSTransportRegistry()
		serviceRegistry := include.ServiceRegistry()

		ctx := box.Context(baseCtx, inboundRegistry, outboundRegistry, endpointRegistry, dnsRegistry, serviceRegistry)
		ctx = monitor.ContextWith(ctx, monitorMgr)

		instance, err := box.New(box.Options{Context: ctx, Options: buildResult.Options})
		if err != nil {
			if removedNodes, ok := batchRemoveFailedNodes(err, &workingCfg, buildResult); ok {
				disableNodes(stateStore, removedNodes, time.Time{})
				continue
			}
			return nil, nil, fmt.Errorf("create sing-box instance: %w", err)
		}
		if err := instance.Start(); err != nil {
			_ = instance.Close()
			if removedNodes, ok := batchRemoveFailedNodes(err, &workingCfg, buildResult); ok {
				disableNodes(stateStore, removedNodes, time.Time{})
				continue
			}
			return nil, nil, fmt.Errorf("start sing-box: %w", err)
		}
		if monitorMgr != nil && len(buildResult.BaseTags) > 0 {
			monitorMgr.PruneByTags(buildResult.BaseTags)
		}
		return instance, workingCfg.Nodes, nil
	}
}

func startSubscriptionRefresher(ctx, baseCtx context.Context, cfg *config.Config, monitorMgr *monitor.Manager, stateStore *state.Store, instance **box.Box, nodes *[]config.NodeConfig, mu *sync.Mutex) {
	if cfg.SubscriptionRefreshInterval <= 0 || len(cfg.Subscriptions) == 0 || cfg.ConfigPath == "" {
		return
	}
	ticker := time.NewTicker(cfg.SubscriptionRefreshInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				newCfg, err := config.Load(cfg.ConfigPath)
				if err != nil {
					log.Printf("❌ subscription refresh skipped: %v", err)
					continue
				}
				if newCfg.Mode != cfg.Mode {
					log.Printf("❌ subscription refresh skipped: mode changed from %s to %s", cfg.Mode, newCfg.Mode)
					continue
				}

				mu.Lock()
				prevNodes := append([]config.NodeConfig(nil), (*nodes)...)
				mu.Unlock()

				if nodesEqual(prevNodes, newCfg.Nodes) {
					continue
				}

				log.Printf("🔁 Refreshing subscriptions: %d -> %d nodes", len(prevNodes), len(newCfg.Nodes))

				mu.Lock()
				if *instance != nil {
					_ = (*instance).Close()
					*instance = nil
				}
				mu.Unlock()

				inst, updatedNodes, err := startBoxInstance(baseCtx, cfg, monitorMgr, stateStore, newCfg.Nodes)
				if err != nil {
					log.Printf("❌ Subscription reload failed: %v", err)
					inst, updatedNodes, err = startBoxInstance(baseCtx, cfg, monitorMgr, stateStore, prevNodes)
					if err != nil {
						log.Printf("❌ Failed to restore previous nodes: %v", err)
						continue
					}
				}

				mu.Lock()
				*instance = inst
				*nodes = append([]config.NodeConfig(nil), updatedNodes...)
				cfg.Nodes = append([]config.NodeConfig(nil), updatedNodes...)
				mu.Unlock()

				log.Printf("✅ Subscription refresh complete, %d nodes active", len(updatedNodes))
			}
		}
	}()
}

func nodesEqual(a, b []config.NodeConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].URI != b[i].URI ||
			a[i].Name != b[i].Name ||
			a[i].Port != b[i].Port ||
			a[i].Username != b[i].Username ||
			a[i].Password != b[i].Password {
			return false
		}
	}
	return true
}

func nodeToStateMeta(node config.NodeConfig) state.Meta {
	return state.Meta{
		ID:     node.EndpointID,
		Host:   node.EndpointHost,
		Port:   node.EndpointPort,
		Scheme: node.EndpointScheme,
		Name:   node.Name,
		URI:    node.URI,
	}
}

func disableNodes(store *state.Store, nodes []config.NodeConfig, until time.Time) {
	if store == nil {
		return
	}
	for _, node := range nodes {
		meta := nodeToStateMeta(node)
		if meta.ID == "" {
			continue
		}
		store.Disable(meta, until)
	}
}

func filterBlockedNodes(nodes []config.NodeConfig, store *state.Store) ([]config.NodeConfig, int) {
	if store == nil {
		return append([]config.NodeConfig(nil), nodes...), 0
	}
	now := time.Now()
	allowed := make([]config.NodeConfig, 0, len(nodes))
	skipped := 0
	for _, node := range nodes {
		meta := nodeToStateMeta(node)
		if meta.ID != "" {
			store.UpdateMeta(meta)
			if store.IsBlocked(meta.ID, now) {
				log.Printf("⚠️  节点 %s (%s) 已被禁用，启动时跳过", node.Name, meta.ID)
				skipped++
				continue
			}
		}
		allowed = append(allowed, node)
	}
	return allowed, skipped
}
