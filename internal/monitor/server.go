package monitor

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"runtime"
	"strings"
	"time"
)

//go:embed assets/index.html
var embeddedFS embed.FS

// Server exposes HTTP endpoints for monitoring.
type Server struct {
	cfg          Config
	mgr          *Manager
	srv          *http.Server
	logger       *log.Logger
	sessionToken string // 简单的 session token，重启后失效
}

// NewServer constructs a server; it can be nil when disabled.
func NewServer(cfg Config, mgr *Manager, logger *log.Logger) *Server {
	if !cfg.Enabled || mgr == nil {
		return nil
	}
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{cfg: cfg, mgr: mgr, logger: logger}

	// 生成随机 session token
	tokenBytes := make([]byte, 32)
	rand.Read(tokenBytes)
	s.sessionToken = hex.EncodeToString(tokenBytes)

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/auth", s.handleAuth)
	mux.HandleFunc("/api/nodes", s.withAuth(s.handleNodes))
	mux.HandleFunc("/api/nodes/", s.withAuth(s.handleNodeAction))
	mux.HandleFunc("/api/export", s.withAuth(s.handleExport))
	mux.HandleFunc("/api/stats", s.withAuth(s.handleRuntimeStats))

	// pprof 端点 - 用于运行时性能分析
	mux.HandleFunc("/debug/pprof/", s.withAuth(pprof.Index))
	mux.HandleFunc("/debug/pprof/cmdline", s.withAuth(pprof.Cmdline))
	mux.HandleFunc("/debug/pprof/profile", s.withAuth(pprof.Profile))
	mux.HandleFunc("/debug/pprof/symbol", s.withAuth(pprof.Symbol))
	mux.HandleFunc("/debug/pprof/trace", s.withAuth(pprof.Trace))
	mux.HandleFunc("/debug/pprof/heap", s.withAuth(pprof.Handler("heap").ServeHTTP))
	mux.HandleFunc("/debug/pprof/goroutine", s.withAuth(pprof.Handler("goroutine").ServeHTTP))
	mux.HandleFunc("/debug/pprof/allocs", s.withAuth(pprof.Handler("allocs").ServeHTTP))
	mux.HandleFunc("/debug/pprof/block", s.withAuth(pprof.Handler("block").ServeHTTP))
	mux.HandleFunc("/debug/pprof/mutex", s.withAuth(pprof.Handler("mutex").ServeHTTP))
	mux.HandleFunc("/debug/pprof/threadcreate", s.withAuth(pprof.Handler("threadcreate").ServeHTTP))

	s.srv = &http.Server{Addr: cfg.Listen, Handler: mux}
	return s
}

// Start launches the HTTP server.
func (s *Server) Start(ctx context.Context) {
	if s == nil || s.srv == nil {
		return
	}
	s.logger.Printf("Starting monitor server on %s", s.cfg.Listen)
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Printf("❌ Monitor server error: %v", err)
		}
	}()
	// Give server a moment to start and check for immediate errors
	time.Sleep(100 * time.Millisecond)
	s.logger.Printf("✅ Monitor server started on http://%s", s.cfg.Listen)

	go func() {
		<-ctx.Done()
		s.Shutdown(context.Background())
	}()
}

// Shutdown stops the server gracefully.
func (s *Server) Shutdown(ctx context.Context) {
	if s == nil || s.srv == nil {
		return
	}
	_ = s.srv.Shutdown(ctx)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := embeddedFS.ReadFile("assets/index.html")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// 只返回初始检查通过的可用节点，同时附带整体节点统计信息，便于前端展示更多维度的状态。
	nodes := s.mgr.SnapshotFiltered(true)
	stats := s.mgr.Stats()
	payload := map[string]any{
		"nodes": nodes,
		"stats": stats,
	}
	writeJSON(w, payload)
}

func (s *Server) handleNodeAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/nodes/"), "/")
	if len(parts) < 1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	tag := parts[0]
	if tag == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch action {
	case "probe":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		latency, err := s.mgr.Probe(ctx, tag)
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"message": "探测成功", "latency_ms": latency.Milliseconds()})
	case "release":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := s.mgr.Release(tag); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"message": "已解除拉黑"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

// withAuth 认证中间件，如果配置了密码则需要验证
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 如果没有配置密码，直接放行
		if s.cfg.Password == "" {
			next(w, r)
			return
		}

		// 检查 Cookie 中的 session token
		cookie, err := r.Cookie("session_token")
		if err == nil && cookie.Value == s.sessionToken {
			next(w, r)
			return
		}

		// 检查 Authorization header (Bearer token)
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token == s.sessionToken {
				next(w, r)
				return
			}
		}

		// 未授权
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "未授权，请先登录"})
	}
}

// handleAuth 处理登录认证
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	// 如果没有配置密码，直接返回成功（不需要token）
	if s.cfg.Password == "" {
		writeJSON(w, map[string]any{"message": "无需密码", "no_password": true})
		return
	}

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}

	// 验证密码
	if req.Password != s.cfg.Password {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "密码错误"})
		return
	}

	// 设置 cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    s.sessionToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400 * 7, // 7天
	})

	writeJSON(w, map[string]any{
		"message": "登录成功",
		"token":   s.sessionToken,
	})
}

// handleRuntimeStats 返回运行时内存和 goroutine 统计信息
func (s *Server) handleRuntimeStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	stats := map[string]any{
		"goroutines": runtime.NumGoroutine(),
		"memory": map[string]any{
			"alloc_mb":        float64(m.Alloc) / 1024 / 1024,
			"total_alloc_mb":  float64(m.TotalAlloc) / 1024 / 1024,
			"sys_mb":          float64(m.Sys) / 1024 / 1024,
			"heap_alloc_mb":   float64(m.HeapAlloc) / 1024 / 1024,
			"heap_sys_mb":     float64(m.HeapSys) / 1024 / 1024,
			"heap_idle_mb":    float64(m.HeapIdle) / 1024 / 1024,
			"heap_inuse_mb":   float64(m.HeapInuse) / 1024 / 1024,
			"heap_released_mb": float64(m.HeapReleased) / 1024 / 1024,
			"heap_objects":    m.HeapObjects,
			"stack_inuse_mb":  float64(m.StackInuse) / 1024 / 1024,
			"stack_sys_mb":    float64(m.StackSys) / 1024 / 1024,
			"gc_cycles":       m.NumGC,
			"gc_pause_total_ms": float64(m.PauseTotalNs) / 1e6,
		},
		"gc": map[string]any{
			"next_gc_mb":     float64(m.NextGC) / 1024 / 1024,
			"last_gc_time":   time.Unix(0, int64(m.LastGC)).Format(time.RFC3339),
			"gc_cpu_percent": m.GCCPUFraction * 100,
		},
	}

	writeJSON(w, stats)
}

// handleExport 导出所有可用代理池节点的 HTTP 代理 URI，每行一个
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// 只导出初始检查通过的可用节点
	snapshots := s.mgr.SnapshotFiltered(true)
	var lines []string

	for _, snap := range snapshots {
		// 只导出有监听地址和端口的节点（代理池节点）
		if snap.ListenAddress != "" && snap.Port > 0 {
			// 使用外部 IP 替换 0.0.0.0
			listenAddr := snap.ListenAddress
			if (listenAddr == "0.0.0.0" || listenAddr == "::") && s.cfg.ExternalIP != "" {
				listenAddr = s.cfg.ExternalIP
			}

			var proxyURI string
			if s.cfg.ProxyUsername != "" && s.cfg.ProxyPassword != "" {
				proxyURI = fmt.Sprintf("http://%s:%s@%s:%d",
					s.cfg.ProxyUsername, s.cfg.ProxyPassword,
					listenAddr, snap.Port)
			} else {
				proxyURI = fmt.Sprintf("http://%s:%d", listenAddr, snap.Port)
			}
			lines = append(lines, proxyURI)
		}
	}

	// 返回纯文本，每行一个 URI
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=proxy_pool.txt")
	_, _ = w.Write([]byte(strings.Join(lines, "\n")))
}
