package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"easy_proxies/internal/boxmgr"
	"easy_proxies/internal/config"
	"easy_proxies/internal/database"
	"easy_proxies/internal/logx"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/subscription"
)

// Run builds the runtime components from config and blocks until shutdown.
func Run(ctx context.Context, cfg *config.Config) error {
	// If database is enabled:
	// - import nodes from config (inline) and nodes.txt (nodes_file) into DB (best-effort)
	// - load active nodes from DB as the primary runtime node list
	// nodes.txt remains a data source; subscription refresh is handled asynchronously.
	if cfg != nil && cfg.Database.Enabled && strings.TrimSpace(cfg.Database.Path) != "" {
		openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		db, err := database.Open(openCtx, cfg.Database.Path)
		cancel()
		if err != nil {
			logx.Printf("⚠️  open database failed, continue with config nodes: %v", err)
		} else {
			importCtx, cancelImport := context.WithTimeout(ctx, 10*time.Second)
			for _, n := range cfg.Nodes {
				// Import into DB; if host:port can't be extracted, drop it (filtered out).
				if _, upErr := db.UpsertNodeByHostPort(importCtx, database.UpsertNodeInput{
					URI:  n.URI,
					Name: n.Name,
				}); upErr != nil {
					logx.Printf("⚠️  drop node (cannot extract host:port): %s (%v)", n.Name, upErr)
				}
			}
			cancelImport()

			loadCtx, cancelLoad := context.WithTimeout(ctx, 5*time.Second)
			active, listErr := db.ListActiveNodes(loadCtx)
			cancelLoad()
			_ = db.Close()

			if listErr != nil {
				logx.Printf("⚠️  list active nodes from database failed, continue with config nodes: %v", listErr)
			} else if len(active) > 0 {
				nodes := make([]config.NodeConfig, 0, len(active))
				for _, n := range active {
					nodes = append(nodes, config.NodeConfig{
						Name: n.Name,
						URI:  n.URI,
					})
				}
				cfg.Nodes = nodes
				logx.Printf("✅ loaded %d active nodes from database", len(active))
			}
		}
	}

	// Build monitor config
	proxyUsername := cfg.Listener.Username
	proxyPassword := cfg.Listener.Password
	if cfg.Mode == "multi-port" || cfg.Mode == "hybrid" {
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
		Database: struct {
			Enabled bool
			Path    string
		}{
			Enabled: cfg.Database.Enabled,
			Path:    cfg.Database.Path,
		},
	}

	// Create and start BoxManager
	boxMgr := boxmgr.New(cfg, monitorCfg)
	if err := boxMgr.Start(ctx); err != nil {
		return fmt.Errorf("start box manager: %w", err)
	}
	defer boxMgr.Close()

	// Wire up config to monitor server for settings API
	if server := boxMgr.MonitorServer(); server != nil {
		server.SetConfig(cfg)
	}

	// Create and start SubscriptionManager if enabled
	var subMgr *subscription.Manager
	if cfg.SubscriptionRefresh.Enabled && len(cfg.Subscriptions) > 0 {
		subMgr = subscription.New(cfg, boxMgr)
		subMgr.Start()
		defer subMgr.Stop()

		// Wire up subscription manager to monitor server for API endpoints
		if server := boxMgr.MonitorServer(); server != nil {
			server.SetSubscriptionRefresher(subMgr)
		}
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-ctx.Done():
	case sig := <-sigCh:
		fmt.Printf("received %s, shutting down\n", sig)
	}

	return nil
}
