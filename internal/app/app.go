package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"easy_proxies/internal/builder"
	"easy_proxies/internal/config"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/outbound/pool"

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
func batchRemoveFailedNodes(err error, cfg *config.Config, result builder.Result) bool {
	outboundIndices := builder.ExtractOutboundIndices(err)
	if len(outboundIndices) == 0 {
		return false
	}
	nodeIndices := mapOutboundIndicesToNodeIndices(result, outboundIndices)
	if len(nodeIndices) == 0 {
		return false
	}
	if summary := summarizeNodeNames(cfg.Nodes, nodeIndices); summary != "" {
		log.Printf("⚠️  Removing unstable nodes after initialization failure: %s", summary)
	}
	cfg.Nodes = removeNodesByIndices(cfg.Nodes, nodeIndices)
	log.Printf("⚠️  Removed %d failed nodes, %d nodes remaining", len(nodeIndices), len(cfg.Nodes))
	return true
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

	monitorCfg := monitor.Config{
		Enabled:       cfg.ManagementEnabled(),
		Listen:        cfg.Management.Listen,
		ProbeTarget:   cfg.Management.ProbeTarget,
		Password:      cfg.Management.Password,
		ProxyUsername: proxyUsername,
		ProxyPassword: proxyPassword,
		ExternalIP:    cfg.ExternalIP,
	}
	monitorMgr, err := monitor.NewManager(monitorCfg)
	if err != nil {
		return fmt.Errorf("init monitor: %w", err)
	}

	workingCfg := *cfg
	workingCfg.Nodes = append([]config.NodeConfig(nil), cfg.Nodes...)

	var (
		instance    *box.Box
		buildResult builder.Result
	)

	baseCtx := ctx
	for {
		buildResult, err = builder.Build(&workingCfg)
		if err != nil {
			return err
		}

		// Pre-remove all nodes that failed during build phase
		// This prevents them from entering sing-box initialization, avoiding expensive rebuild loops
		if len(buildResult.FailedIndices) > 0 {
			indices := uniqueSortedDescending(buildResult.FailedIndices)
			log.Printf("⚠️  Removing %d nodes that failed during build phase", len(indices))
			if summary := summarizeNodeNames(workingCfg.Nodes, indices); summary != "" {
				log.Printf("    Affected nodes: %s", summary)
			}
			workingCfg.Nodes = removeNodesByIndices(workingCfg.Nodes, indices)
			log.Printf("⚠️  %d nodes remaining after removing build failures", len(workingCfg.Nodes))

			// Rebuild with cleaned node list
			continue
		}

		inboundRegistry := include.InboundRegistry()
		outboundRegistry := include.OutboundRegistry()
		pool.Register(outboundRegistry)
		endpointRegistry := include.EndpointRegistry()
		dnsRegistry := include.DNSTransportRegistry()
		serviceRegistry := include.ServiceRegistry()

		ctx = box.Context(baseCtx, inboundRegistry, outboundRegistry, endpointRegistry, dnsRegistry, serviceRegistry)
		ctx = monitor.ContextWith(ctx, monitorMgr)

		instance, err = box.New(box.Options{Context: ctx, Options: buildResult.Options})
		if err != nil {
			if batchRemoveFailedNodes(err, &workingCfg, buildResult) {
				continue
			}
			return fmt.Errorf("create sing-box instance: %w", err)
		}
		if err := instance.Start(); err != nil {
			_ = instance.Close()
			if batchRemoveFailedNodes(err, &workingCfg, buildResult) {
				continue
			}
			return fmt.Errorf("start sing-box: %w", err)
		}
		break
	}

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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-ctx.Done():
	case sig := <-sigCh:
		fmt.Printf("received %s, shutting down\n", sig)
	}
	return instance.Close()
}
