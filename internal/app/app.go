package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
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

// batchRemoveFailedNodes removes multiple failed nodes from config at once
// Returns true if any nodes were removed
func batchRemoveFailedNodes(err error, cfg *config.Config, result builder.Result) bool {
	// Collect all failed outbound indices from the error message
	failedIndices := make(map[int]bool)
	
	// First, try to extract single index from error
	if idx, ok := builder.ExtractOutboundIndex(err); ok && idx < len(result.BaseTags) {
		failedIndices[idx] = true
	}
	
	// If we couldn't extract any indices, return false
	if len(failedIndices) == 0 {
		return false
	}
	
	// Map outbound indices to node indices
	nodeIndicesToRemove := make(map[int]bool)
	for outboundIdx := range failedIndices {
		if outboundIdx >= len(result.BaseTags) {
			continue
		}
		tag := result.BaseTags[outboundIdx]
		if nodeIdx, ok := result.TagToNodeIndex[tag]; ok && nodeIdx < len(cfg.Nodes) {
			nodeIndicesToRemove[nodeIdx] = true
		}
	}
	
	if len(nodeIndicesToRemove) == 0 {
		return false
	}
	
	// Log all nodes being removed
	for nodeIdx := range nodeIndicesToRemove {
		node := cfg.Nodes[nodeIdx]
		log.Printf("⚠️  Removing unstable node '%s' after initialization failure", node.Name)
	}
	
	// Remove nodes in reverse order to maintain valid indices
	// Convert map to sorted slice
	indicesToRemove := make([]int, 0, len(nodeIndicesToRemove))
	for idx := range nodeIndicesToRemove {
		indicesToRemove = append(indicesToRemove, idx)
	}
	
	// Sort in descending order
	for i := 0; i < len(indicesToRemove); i++ {
		for j := i + 1; j < len(indicesToRemove); j++ {
			if indicesToRemove[i] < indicesToRemove[j] {
				indicesToRemove[i], indicesToRemove[j] = indicesToRemove[j], indicesToRemove[i]
			}
		}
	}
	
	// Remove nodes from highest index to lowest
	for _, idx := range indicesToRemove {
		cfg.Nodes = append(cfg.Nodes[:idx], cfg.Nodes[idx+1:]...)
	}
	
	log.Printf("⚠️  Removed %d failed nodes, %d nodes remaining", len(indicesToRemove), len(cfg.Nodes))
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
			log.Printf("⚠️  Removing %d nodes that failed during build phase", len(buildResult.FailedIndices))
			
			// Sort indices in descending order for safe removal
			indices := make([]int, len(buildResult.FailedIndices))
			copy(indices, buildResult.FailedIndices)
			for i := 0; i < len(indices); i++ {
				for j := i + 1; j < len(indices); j++ {
					if indices[i] < indices[j] {
						indices[i], indices[j] = indices[j], indices[i]
					}
				}
			}
			
			// Remove from highest to lowest index
			for _, idx := range indices {
				if idx < len(workingCfg.Nodes) {
					workingCfg.Nodes = append(workingCfg.Nodes[:idx], workingCfg.Nodes[idx+1:]...)
				}
			}
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
