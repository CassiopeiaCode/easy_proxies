package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"easy_proxies/internal/boxmgr"
	"easy_proxies/internal/config"
	"easy_proxies/internal/logx"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
	pebblestore "easy_proxies/internal/store/pebble"
	"easy_proxies/internal/subscription"
)

func startPprofServer(ctx context.Context, cfg *config.Config) {
	if cfg == nil || !cfg.Pprof.Enabled {
		return
	}

	addr := fmt.Sprintf("%s:%d", cfg.Pprof.Host, cfg.Pprof.Port)
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	logx.Printf("starting pprof server on http://%s/debug/pprof/", addr)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logx.Printf("⚠️  pprof server failed: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
}

// Run builds the runtime components from config and blocks until shutdown.
func Run(ctx context.Context, cfg *config.Config) error {
	// Open Pebble store and use it as the primary persistence layer.
	// Backward-compatible config normalization already resolves cfg.Store.Type/Dir.
	st, err := pebblestore.Open(ctx, pebblestore.Options{Dir: cfg.Store.Dir})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// Import nodes from config into store, then load active nodes as runtime node list.
	importCtx, cancelImport := context.WithTimeout(ctx, 10*time.Second)
	for _, n := range cfg.Nodes {
		if _, upErr := st.UpsertNodeByHostPort(importCtx, store.UpsertNodeInput{URI: n.URI, Name: n.Name}); upErr != nil {
			logx.Printf("⚠️  drop node (cannot extract host:port): %s (%v)", n.Name, upErr)
		}
	}
	cancelImport()

	portMap := cfg.BuildPortMap()

	loadCtx, cancelLoad := context.WithTimeout(ctx, 2*time.Minute)
	active, listErr := st.ListActiveNodes(loadCtx)
	cancelLoad()
	if listErr != nil {
		logx.Printf("⚠️  list active nodes from store failed, continue with config nodes: %v", listErr)
	} else if len(active) > 0 {
		// Prefer store-driven active nodes, but de-duplicate by egress IP and cap runtime nodes to 5000.
		// Unknown egress_ip nodes are kept as-is (not grouped).
		type score struct {
			rate       float64
			total      int64
			latencyMs  int64
			hasLatency bool
			key        string // host:port for stable tie-break
		}
		rateByKey := make(map[string]store.NodeRate, 0)
		rateCtx, cancelRate := context.WithTimeout(ctx, 5*time.Second)
		rates, rerr := st.QueryNodeRatesSince(rateCtx, time.Now().UTC().Add(-24*time.Hour))
		cancelRate()
		if rerr == nil {
			rateByKey = make(map[string]store.NodeRate, len(rates))
			for _, r := range rates {
				rateByKey[strings.TrimSpace(r.Host)+":"+strconv.Itoa(r.Port)] = r
			}
		}

		bestByGroup := make(map[string]store.Node, len(active))
		bestScore := make(map[string]score, len(active))

		for _, n := range active {
			host := strings.TrimSpace(n.Host)
			if host == "" || n.Port <= 0 {
				continue
			}
			hp := host + ":" + strconv.Itoa(n.Port)
			group := hp
			if ip := strings.TrimSpace(n.EgressIP); ip != "" {
				group = ip
			}

			sc := score{rate: 0, total: 0, latencyMs: 1<<62 - 1, hasLatency: false, key: hp}
			if r, ok := rateByKey[hp]; ok {
				sc.rate = r.Rate
				sc.total = r.SuccessCount + r.FailCount
			}
			if n.LastLatencyMs != nil {
				sc.latencyMs = *n.LastLatencyMs
				sc.hasLatency = true
			}

			cur, exists := bestByGroup[group]
			if !exists {
				bestByGroup[group] = n
				bestScore[group] = sc
				continue
			}
			curSc := bestScore[group]

			// Pick representative within group:
			// - higher 24h success rate
			// - higher sample count
			// - lower last latency (if present)
			// - stable host:port
			replace := false
			if sc.rate > curSc.rate {
				replace = true
			} else if sc.rate == curSc.rate && sc.total > curSc.total {
				replace = true
			} else if sc.rate == curSc.rate && sc.total == curSc.total {
				if sc.hasLatency && !curSc.hasLatency {
					replace = true
				} else if sc.hasLatency == curSc.hasLatency && sc.latencyMs < curSc.latencyMs {
					replace = true
				} else if sc.hasLatency == curSc.hasLatency && sc.latencyMs == curSc.latencyMs && sc.key < curSc.key {
					replace = true
				}
			}

			if replace {
				_ = cur
				bestByGroup[group] = n
				bestScore[group] = sc
			}
		}

		// Flatten + sort deterministically, then cap to 5000.
		selected := make([]store.Node, 0, len(bestByGroup))
		for group, n := range bestByGroup {
			_ = group
			selected = append(selected, n)
		}

		sByHP := make(map[string]score, len(selected))
		for _, n := range selected {
			host := strings.TrimSpace(n.Host)
			hp := host + ":" + strconv.Itoa(n.Port)
			group := hp
			if ip := strings.TrimSpace(n.EgressIP); ip != "" {
				group = ip
			}
			sByHP[hp] = bestScore[group]
		}

		sort.Slice(selected, func(i, j int) bool {
			hi := strings.TrimSpace(selected[i].Host) + ":" + strconv.Itoa(selected[i].Port)
			hj := strings.TrimSpace(selected[j].Host) + ":" + strconv.Itoa(selected[j].Port)
			si := sByHP[hi]
			sj := sByHP[hj]
			if si.rate != sj.rate {
				return si.rate > sj.rate
			}
			if si.total != sj.total {
				return si.total > sj.total
			}
			if si.hasLatency != sj.hasLatency {
				return si.hasLatency
			}
			if si.latencyMs != sj.latencyMs {
				return si.latencyMs < sj.latencyMs
			}
			return si.key < sj.key
		})

		const maxRuntimeNodes = 5000
		// Reserve slots for never-tested nodes so they can enter runtime and get probed.
		// "Never-tested" is defined as HealthCheckCount == 0 (no recorded health checks).
		const reserveNeverTested = 3000
		if len(selected) > maxRuntimeNodes {
			tested := make([]store.Node, 0, len(selected))
			neverTested := make([]store.Node, 0, len(selected))
			for _, n := range selected {
				if n.HealthCheckCount == 0 {
					neverTested = append(neverTested, n)
				} else {
					tested = append(tested, n)
				}
			}

			neverTake := 0
			if len(neverTested) >= reserveNeverTested {
				neverTake = reserveNeverTested
			} else {
				neverTake = len(neverTested)
			}
			testedCap := maxRuntimeNodes - neverTake
			testedTake := testedCap
			if len(tested) < testedTake {
				testedTake = len(tested)
			}
			remaining := maxRuntimeNodes - (testedTake + neverTake)
			if remaining > 0 {
				if len(neverTested) > neverTake {
					extra := len(neverTested) - neverTake
					if extra > remaining {
						extra = remaining
					}
					neverTake += extra
					remaining -= extra
				}
			}
			if remaining > 0 {
				if len(tested) > testedTake {
					extra := len(tested) - testedTake
					if extra > remaining {
						extra = remaining
					}
					testedTake += extra
					remaining -= extra
				}
			}
			selected = append(tested[:testedTake], neverTested[:neverTake]...)
		}
		if len(selected) == 0 {
			// Fallback: keep at least one node so the service can start.
			selected = active[:1]
		}

		nodes := make([]config.NodeConfig, 0, len(selected))
		for _, n := range selected {
			nodes = append(nodes, config.NodeConfig{Name: n.Name, URI: n.URI})
		}
		cfg.Nodes = nodes
		if err := cfg.NormalizeWithPortMap(portMap); err != nil {
			logx.Printf("⚠️  normalize nodes after store load failed: %v (continue)", err)
		}
		neverTestedCount := 0
		for i := range selected {
			if selected[i].HealthCheckCount == 0 {
				neverTestedCount++
			}
		}
		logx.Printf("✅ loaded %d active nodes from store (egress groups=%d, runtime=%d, never_tested=%d)", len(active), len(bestByGroup), len(cfg.Nodes), neverTestedCount)
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
			Enabled: true,
			Path:    cfg.Store.Dir,
		},
	}

	// Create and start BoxManager
	boxMgr := boxmgr.New(cfg, monitorCfg, st)
	if err := boxMgr.Start(ctx); err != nil {
		return fmt.Errorf("start box manager: %w", err)
	}
	defer boxMgr.Close()
	startPprofServer(ctx, cfg)

	// Wire up config to monitor server for settings API
	if server := boxMgr.MonitorServer(); server != nil {
		server.SetConfig(cfg)
	}

	// Create and start SubscriptionManager if enabled
	var subMgr *subscription.Manager
	if cfg.SubscriptionRefresh.Enabled && len(cfg.Subscriptions) > 0 {
		subMgr = subscription.New(cfg, boxMgr, st)
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
