package pool

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"easy_proxies/internal/monitor"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

const (
	// Type is the outbound type name exposed to sing-box.
	Type = "pool"
	// Tag is the default outbound tag used by builder.
	Tag = "proxy-pool"

	modeSequential = "sequential"
	modeRandom     = "random"
	modeBalance    = "balance"
)

// Options controls pool outbound behaviour.
type Options struct {
	Mode              string
	Members           []string
	FailureThreshold  int
	BlacklistDuration time.Duration
	Metadata          map[string]MemberMeta
	ExpectedStatuses  []int
}

// MemberMeta carries optional descriptive information for monitoring UI.
type MemberMeta struct {
	Name          string
	URI           string
	Mode          string
	ListenAddress string
	Port          uint16
}

// Register wires the pool outbound into the registry.
func Register(registry *outbound.Registry) {
	outbound.Register[Options](registry, Type, newPool)
}

var defaultMonitor atomic.Pointer[monitor.Manager]

// SetDefaultMonitorManager provides a fallback monitor manager when the sing-box context does not
// preserve context.Value entries.
func SetDefaultMonitorManager(m *monitor.Manager) {
	defaultMonitor.Store(m)
}

func getDefaultMonitorManager() *monitor.Manager {
	return defaultMonitor.Load()
}

type memberState struct {
	outbound adapter.Outbound
	tag      string
	entry    *monitor.EntryHandle
	shared   *sharedMemberState
}

type poolOutbound struct {
	outbound.Adapter
	ctx            context.Context
	logger         log.ContextLogger
	manager        adapter.OutboundManager
	options        Options
	mode           string
	members        []*memberState
	mu             sync.Mutex
	rrCounter      atomic.Uint32
	rng            *rand.Rand
	rngMu          sync.Mutex // protects rng for random mode
	monitor        *monitor.Manager
	candidatesPool sync.Pool
}

func newPool(ctx context.Context, _ adapter.Router, logger log.ContextLogger, tag string, options Options) (adapter.Outbound, error) {
	if len(options.Members) == 0 {
		return nil, E.New("pool requires at least one member")
	}
	manager := service.FromContext[adapter.OutboundManager](ctx)
	if manager == nil {
		return nil, E.New("missing outbound manager in context")
	}

	ctxMonitor := monitor.FromContext(ctx)
	defaultMonitor := getDefaultMonitorManager()
	monitorMgr := ctxMonitor
	if monitorMgr == nil {
		monitorMgr = defaultMonitor
		if monitorMgr != nil {
			logger.Warn("monitor manager missing in context; using default monitor manager fallback")
		} else {
			logger.Warn("monitor manager missing in context and default monitor manager is nil; monitoring will be disabled")
		}
	} else if defaultMonitor != nil && defaultMonitor != ctxMonitor {
		logger.Warn("monitor manager from context differs from default monitor manager (using context)")
	}

	normalized := normalizeOptions(options)
	memberCount := len(normalized.Members)
	p := &poolOutbound{
		Adapter: outbound.NewAdapter(Type, tag, []string{N.NetworkTCP, N.NetworkUDP}, normalized.Members),
		ctx:     ctx,
		logger:  logger,
		manager: manager,
		options: normalized,
		mode:    normalized.Mode,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
		monitor: monitorMgr,
		candidatesPool: sync.Pool{
			New: func() any {
				return make([]*memberState, 0, memberCount)
			},
		},
	}

	// Register nodes immediately if monitor is available
	if monitorMgr != nil {
		logger.Info("registering ", len(normalized.Members), " nodes to monitor")
		for _, memberTag := range normalized.Members {
			// Acquire shared state for this tag (creates if not exists)
			state := acquireSharedState(memberTag)

			meta := normalized.Metadata[memberTag]
			info := monitor.NodeInfo{
				Tag:           memberTag,
				Name:          meta.Name,
				URI:           meta.URI,
				Mode:          meta.Mode,
				ListenAddress: meta.ListenAddress,
				Port:          meta.Port,
			}
			entry := monitorMgr.Register(info)
			if entry != nil {
				// Attach entry to shared state so all pool instances share it
				state.attachEntry(entry)
				logger.Info("registered node: ", memberTag)
				// Set probe and release functions immediately
				entry.SetRelease(p.makeReleaseByTagFunc(memberTag))
				if probeFn := p.makeProbeByTagFunc(memberTag); probeFn != nil {
					entry.SetProbe(probeFn)
				}
			} else {
				logger.Warn("failed to register node: ", memberTag)
			}
		}
	} else {
		logger.Warn("monitor manager is nil, skipping node registration")
	}

	return p, nil
}

func normalizeOptions(options Options) Options {
	if options.FailureThreshold <= 0 {
		options.FailureThreshold = 3
	}
	if options.BlacklistDuration <= 0 {
		options.BlacklistDuration = 24 * time.Hour
	}
	if options.Metadata == nil {
		options.Metadata = make(map[string]MemberMeta)
	}
	switch strings.ToLower(options.Mode) {
	case modeRandom:
		options.Mode = modeRandom
	case modeBalance:
		options.Mode = modeBalance
	default:
		options.Mode = modeSequential
	}
	return options
}

func (p *poolOutbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	p.mu.Lock()
	err := p.initializeMembersLocked()
	p.mu.Unlock()
	if err != nil {
		return err
	}
	// 在初始化完成后，立即在后台触发健康检查
	if p.monitor != nil {
		go p.probeAllMembersOnStartup()
	}
	return nil
}

// initializeMembersLocked must be called with p.mu held
func (p *poolOutbound) initializeMembersLocked() error {
	if len(p.members) > 0 {
		return nil // Already initialized
	}

	members := make([]*memberState, 0, len(p.options.Members))
	for _, tag := range p.options.Members {
		detour, loaded := p.manager.Outbound(tag)
		if !loaded {
			return E.New("pool member not found: ", tag)
		}

		// Acquire shared state (creates if not exists, reuses if already created)
		state := acquireSharedState(tag)

		member := &memberState{
			outbound: detour,
			tag:      tag,
			shared:   state,
			entry:    state.entryHandle(),
		}

		// Connect to existing monitor entry if available
		if p.monitor != nil {
			meta := p.options.Metadata[tag]
			info := monitor.NodeInfo{
				Tag:           tag,
				Name:          meta.Name,
				URI:           meta.URI,
				Mode:          meta.Mode,
				ListenAddress: meta.ListenAddress,
				Port:          meta.Port,
			}
			entry := p.monitor.Register(info)
			if entry != nil {
				state.attachEntry(entry)
				member.entry = entry
				entry.SetRelease(p.makeReleaseFunc(member))
				if probe := p.makeProbeFunc(member); probe != nil {
					entry.SetProbe(probe)
				}
			}
		}
		members = append(members, member)
	}
	p.members = members
	p.logger.Info("pool initialized with ", len(members), " members")

	return nil
}

// probeAllMembersOnStartup performs initial health checks on all members
func (p *poolOutbound) probeAllMembersOnStartup() {
	destination, ok := p.monitor.DestinationForProbe()
	if !ok {
		p.logger.Warn("probe target not configured, skipping initial health check")
		// 没有配置探测目标时，标记所有节点为可用
		p.mu.Lock()
		for _, member := range p.members {
			if member.entry != nil {
				member.entry.MarkInitialCheckDone(true)
			}
		}
		p.mu.Unlock()
		return
	}

	_, useTLS, hostHeader, path, serverName, insecure, ok := p.monitor.ProbeHTTPInfo()
	if !ok {
		p.logger.Warn("probe target not configured, skipping initial health check")
		p.mu.Lock()
		for _, member := range p.members {
			if member.entry != nil {
				member.entry.MarkInitialCheckDone(true)
			}
		}
		p.mu.Unlock()
		return
	}

	p.logger.Info("starting initial health check for all nodes")

	p.mu.Lock()
	members := make([]*memberState, len(p.members))
	copy(members, p.members)
	p.mu.Unlock()

	availableCount := 0
	failedCount := 0

	for _, member := range members {
		// Create a timeout context for each probe
		ctx, cancel := context.WithTimeout(p.ctx, 15*time.Second)

		start := time.Now()
		conn, err := member.outbound.DialContext(ctx, N.NetworkTCP, destination)

		if err != nil {
			p.logger.Warn("initial probe failed for ", member.tag, ": ", err)
			failedCount++
			if member.entry != nil {
				member.entry.RecordFailure(err)
				member.entry.MarkInitialCheckDone(false) // 标记为不可用
			}
			cancel()
			continue
		}

		// Perform HTTP(S) probe to measure actual latency (TTFB)
		_, err = httpProbe(ctx, conn, useTLS, hostHeader, path, serverName, insecure, p.options.ExpectedStatuses)
		conn.Close()

		if err != nil {
			p.logger.Warn("initial HTTP probe failed for ", member.tag, ": ", err)
			failedCount++
			if member.entry != nil {
				member.entry.RecordFailure(err)
				member.entry.MarkInitialCheckDone(false)
			}
			cancel()
			continue
		}

		// Total latency = dial + HTTP probe
		latency := time.Since(start)
		latencyMs := latency.Milliseconds()
		p.logger.Info("initial probe success for ", member.tag, ", latency: ", latencyMs, "ms")
		availableCount++
		if member.entry != nil {
			member.entry.RecordSuccessWithLatency(latency)
			member.entry.MarkInitialCheckDone(true)
		}

		cancel()
	}

	p.logger.Info("initial health check completed: ", availableCount, " available, ", failedCount, " failed")
}

func (p *poolOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	member, err := p.pickMember(network)
	if err != nil {
		return nil, err
	}
	p.incActive(member)
	conn, err := member.outbound.DialContext(ctx, network, destination)
	if err != nil {
		p.decActive(member)
		p.recordFailure(member, err)
		return nil, err
	}
	p.recordSuccess(member)
	return p.wrapConn(conn, member), nil
}

func (p *poolOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	member, err := p.pickMember(N.NetworkUDP)
	if err != nil {
		return nil, err
	}
	p.incActive(member)
	conn, err := member.outbound.ListenPacket(ctx, destination)
	if err != nil {
		p.decActive(member)
		p.recordFailure(member, err)
		return nil, err
	}
	p.recordSuccess(member)
	return p.wrapPacketConn(conn, member), nil
}

func (p *poolOutbound) pickMember(network string) (*memberState, error) {
	now := time.Now()
	candidates := p.getCandidateBuffer()

	p.mu.Lock()
	if len(p.members) == 0 {
		if err := p.initializeMembersLocked(); err != nil {
			p.mu.Unlock()
			p.putCandidateBuffer(candidates)
			return nil, err
		}
	}
	candidates = p.availableMembersLocked(now, network, candidates)
	p.mu.Unlock()

	if len(candidates) == 0 {
		p.mu.Lock()
		if p.releaseIfAllBlacklistedLocked(now) {
			candidates = p.availableMembersLocked(now, network, candidates)
		}
		p.mu.Unlock()
	}

	if len(candidates) == 0 {
		p.putCandidateBuffer(candidates)
		return nil, E.New("no healthy proxy available")
	}

	member := p.selectMember(candidates)
	p.putCandidateBuffer(candidates)
	return member, nil
}

func (p *poolOutbound) availableMembersLocked(now time.Time, network string, buf []*memberState) []*memberState {
	result := buf[:0]
	for _, member := range p.members {
		// Check blacklist via shared state (auto-clears if expired)
		if member.shared != nil && member.shared.isBlacklisted(now) {
			continue
		}
		// Health gating: only schedule nodes that have completed initial check and are available.
		// This aligns runtime scheduling with /api/nodes and DB-derived health threshold semantics.
		if member.entry != nil {
			snap := member.entry.Snapshot()
			if !snap.InitialCheckDone || !snap.Available {
				continue
			}
		}
		if network != "" && !common.Contains(member.outbound.Network(), network) {
			continue
		}
		result = append(result, member)
	}
	return result
}

func (p *poolOutbound) releaseIfAllBlacklistedLocked(now time.Time) bool {
	if len(p.members) == 0 {
		return false
	}
	// Check if all members are blacklisted
	for _, member := range p.members {
		if member.shared == nil || !member.shared.isBlacklisted(now) {
			return false
		}
	}
	// All blacklisted, force release all
	for _, member := range p.members {
		if member.shared != nil {
			member.shared.forceRelease()
		}
	}
	p.logger.Warn("all upstream proxies were blacklisted, releasing them for retry")
	return true
}

func (p *poolOutbound) selectMember(candidates []*memberState) *memberState {
	switch p.mode {
	case modeRandom:
		p.rngMu.Lock()
		idx := p.rng.Intn(len(candidates))
		p.rngMu.Unlock()
		return candidates[idx]
	case modeBalance:
		var selected *memberState
		var minActive int32
		for _, member := range candidates {
			var active int32
			if member.shared != nil {
				active = member.shared.activeCount()
			}
			if selected == nil || active < minActive {
				selected = member
				minActive = active
			}
		}
		return selected
	default:
		idx := int(p.rrCounter.Add(1)-1) % len(candidates)
		return candidates[idx]
	}
}

func (p *poolOutbound) recordFailure(member *memberState, cause error) {
	if member.shared == nil {
		p.logger.Warn("proxy ", member.tag, " failure (no shared state): ", cause)
		return
	}
	failures, blacklisted, _ := member.shared.recordFailure(cause, p.options.FailureThreshold, p.options.BlacklistDuration)
	if blacklisted {
		p.logger.Warn("proxy ", member.tag, " blacklisted for ", p.options.BlacklistDuration, ": ", cause)
	} else {
		p.logger.Warn("proxy ", member.tag, " failure ", failures, "/", p.options.FailureThreshold, ": ", cause)
	}
}

func (p *poolOutbound) recordSuccess(member *memberState) {
	if member.shared != nil {
		member.shared.recordSuccess()
	}
}

func (p *poolOutbound) wrapConn(conn net.Conn, member *memberState) net.Conn {
	wrapped := &trackedConn{Conn: conn, release: func() {
		p.decActive(member)
	}}
	// Soft-failure: if a connection is established but we don't receive at least 1KB within 10s,
	// record a failure for this member without interrupting the connection.
	return newReadWatchConn(wrapped, 10*time.Second, 1024, func() {
		if member != nil && member.shared != nil {
			_, _, _ = member.shared.recordFailure(fmt.Errorf("no 1KB received within 10s"), p.options.FailureThreshold, p.options.BlacklistDuration)
		}
	})
}

func (p *poolOutbound) wrapPacketConn(conn net.PacketConn, member *memberState) net.PacketConn {
	return &trackedPacketConn{PacketConn: conn, release: func() {
		p.decActive(member)
	}}
}

func (p *poolOutbound) makeReleaseFunc(member *memberState) func() {
	return func() {
		if member.shared != nil {
			member.shared.forceRelease()
		}
	}
}

// httpProbe performs an HTTP probe through the connection and measures TTFB.
// It sends a minimal HTTP request and validates the HTTP status code if expectedStatuses is non-empty.
func httpProbe(ctx context.Context, conn net.Conn, useTLS bool, hostHeader, path, serverName string, insecure bool, expectedStatuses []int) (time.Duration, error) {
	if strings.TrimSpace(path) == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.TrimSpace(hostHeader) == "" {
		hostHeader = serverName
	}

	// Ensure the probe is bounded by ctx if provided.
	var hasDeadline bool
	var deadline time.Time
	if ctx != nil {
		if d, ok := ctx.Deadline(); ok {
			hasDeadline = true
			deadline = d
			_ = conn.SetDeadline(deadline)
		}
	}

	// If https, wrap the connection with TLS and handshake first.
	if useTLS {
		cfg := &tls.Config{
			ServerName:         serverName,
			InsecureSkipVerify: insecure,
		}
		tlsConn := tls.Client(conn, cfg)

		if hasDeadline {
			_ = tlsConn.SetDeadline(deadline)
		} else {
			_ = tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
		}
		if err := tlsConn.Handshake(); err != nil {
			return 0, fmt.Errorf("tls handshake: %w", err)
		}

		// Keep ctx-bound deadline (or clear if none).
		if hasDeadline {
			_ = tlsConn.SetDeadline(deadline)
		} else {
			_ = tlsConn.SetDeadline(time.Time{})
		}
		conn = tlsConn
	}

	// Build HTTP request
	// Minimal required headers for HTTP/1.1 + some common headers to avoid being blocked.
	req := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nUser-Agent: Mozilla/5.0\r\nAccept: */*\r\nAccept-Encoding: identity\r\n\r\n",
		path,
		hostHeader,
	)

	// Try to set write deadline (ignore errors for connections that don't support it)
	if hasDeadline {
		_ = conn.SetWriteDeadline(deadline)
	} else {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	}

	// Record time just before sending request
	start := time.Now()

	// Send HTTP request
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, fmt.Errorf("write request: %w", err)
	}

	// Try to set read deadline (ignore errors for connections that don't support it)
	if hasDeadline {
		_ = conn.SetReadDeadline(deadline)
	} else {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	}

	reader := bufio.NewReader(conn)

	// Read HTTP status line
	line, err := reader.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("read status line: %w", err)
	}
	status, err := parseHTTPStatusCode(line)
	if err != nil {
		return 0, err
	}

	// Validate status code if required
	if len(expectedStatuses) > 0 && !containsInt(expectedStatuses, status) {
		return 0, fmt.Errorf("unexpected HTTP status %d (expected %v)", status, expectedStatuses)
	}

	// TTFB: we already got the first line; measure from request send until now
	ttfb := time.Since(start)
	return ttfb, nil
}

func parseHTTPStatusCode(statusLine string) (int, error) {
	// Typical: "HTTP/1.1 204 No Content\r\n"
	parts := strings.Fields(statusLine)
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid HTTP status line: %q", strings.TrimSpace(statusLine))
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("invalid HTTP status code in line: %q", strings.TrimSpace(statusLine))
	}
	return code, nil
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (p *poolOutbound) makeProbeFunc(member *memberState) func(ctx context.Context) (time.Duration, error) {
	if p.monitor == nil {
		return nil
	}
	destination, ok := p.monitor.DestinationForProbe()
	if !ok {
		return nil
	}
	_, useTLS, hostHeader, path, serverName, insecure, ok := p.monitor.ProbeHTTPInfo()
	if !ok {
		return nil
	}
	return func(ctx context.Context) (time.Duration, error) {
		start := time.Now()
		conn, err := member.outbound.DialContext(ctx, N.NetworkTCP, destination)
		if err != nil {
			if member.entry != nil {
				member.entry.RecordFailure(err)
			}
			return 0, err
		}
		defer conn.Close()

		// Perform HTTP(S) probe to measure actual latency (TTFB)
		_, err = httpProbe(ctx, conn, useTLS, hostHeader, path, serverName, insecure, p.options.ExpectedStatuses)
		if err != nil {
			if member.entry != nil {
				member.entry.RecordFailure(err)
			}
			return 0, err
		}

		// Total duration = dial time + HTTP probe
		duration := time.Since(start)
		if member.entry != nil {
			member.entry.RecordSuccessWithLatency(duration)
		}
		return duration, nil
	}
}

// makeProbeByTagFunc creates a probe function that works before member initialization
func (p *poolOutbound) makeProbeByTagFunc(tag string) func(ctx context.Context) (time.Duration, error) {
	if p.monitor == nil {
		return nil
	}
	destination, ok := p.monitor.DestinationForProbe()
	if !ok {
		return nil
	}
	_, useTLS, hostHeader, path, serverName, insecure, ok := p.monitor.ProbeHTTPInfo()
	if !ok {
		return nil
	}
	return func(ctx context.Context) (time.Duration, error) {
		// Ensure members are initialized
		p.mu.Lock()
		if len(p.members) == 0 {
			if err := p.initializeMembersLocked(); err != nil {
				p.mu.Unlock()
				return 0, err
			}
		}

		// Find the member by tag
		var member *memberState
		for _, m := range p.members {
			if m.tag == tag {
				member = m
				break
			}
		}
		p.mu.Unlock()

		if member == nil {
			return 0, E.New("member not found: ", tag)
		}

		start := time.Now()
		conn, err := member.outbound.DialContext(ctx, N.NetworkTCP, destination)
		if err != nil {
			if member.entry != nil {
				member.entry.RecordFailure(err)
			}
			return 0, err
		}
		defer conn.Close()

		// Perform HTTP(S) probe to measure actual latency (TTFB)
		_, err = httpProbe(ctx, conn, useTLS, hostHeader, path, serverName, insecure, p.options.ExpectedStatuses)
		if err != nil {
			if member.entry != nil {
				member.entry.RecordFailure(err)
			}
			return 0, err
		}

		// Total duration = dial time + TTFB
		duration := time.Since(start)
		if member.entry != nil {
			member.entry.RecordSuccessWithLatency(duration)
		}
		return duration, nil
	}
}

// makeReleaseByTagFunc creates a release function that works before member initialization
func (p *poolOutbound) makeReleaseByTagFunc(tag string) func() {
	return func() {
		releaseSharedMember(tag)
	}
}

type trackedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

type readWatchConn struct {
	net.Conn
	needBytes   int64
	deadline    time.Duration
	onTimeout   func()
	readBytes   atomic.Int64
	timeoutFired atomic.Bool
	done        chan struct{}
}

func newReadWatchConn(conn net.Conn, deadline time.Duration, needBytes int64, onTimeout func()) net.Conn {
	if conn == nil || deadline <= 0 || needBytes <= 0 || onTimeout == nil {
		return conn
	}
	w := &readWatchConn{
		Conn:     conn,
		needBytes: needBytes,
		deadline: deadline,
		onTimeout: onTimeout,
		done:     make(chan struct{}),
	}
	go w.watch()
	return w
}

func (w *readWatchConn) watch() {
	timer := time.NewTimer(w.deadline)
	defer timer.Stop()

	select {
	case <-w.done:
		return
	case <-timer.C:
		if w.timeoutFired.CompareAndSwap(false, true) && w.readBytes.Load() < w.needBytes {
			w.onTimeout()
		}
		return
	}
}

func (w *readWatchConn) Read(p []byte) (int, error) {
	n, err := w.Conn.Read(p)
	if n > 0 {
		total := w.readBytes.Add(int64(n))
		if total >= w.needBytes {
			// Cancel watchdog early once we've seen enough bytes.
			select {
			case <-w.done:
			default:
				close(w.done)
			}
		}
	}
	return n, err
}

func (w *readWatchConn) Close() error {
	select {
	case <-w.done:
	default:
		close(w.done)
	}
	return w.Conn.Close()
}

type trackedPacketConn struct {
	net.PacketConn
	once    sync.Once
	release func()
}

func (c *trackedPacketConn) Close() error {
	err := c.PacketConn.Close()
	c.once.Do(c.release)
	return err
}

func (p *poolOutbound) incActive(member *memberState) {
	if member.shared != nil {
		member.shared.incActive()
	}
}

func (p *poolOutbound) decActive(member *memberState) {
	if member.shared != nil {
		member.shared.decActive()
	}
}

func (p *poolOutbound) getCandidateBuffer() []*memberState {
	if buf := p.candidatesPool.Get(); buf != nil {
		return buf.([]*memberState)
	}
	return make([]*memberState, 0, len(p.options.Members))
}

func (p *poolOutbound) putCandidateBuffer(buf []*memberState) {
	if buf == nil {
		return
	}
	const maxCached = 4096
	if cap(buf) > maxCached {
		return
	}
	p.candidatesPool.Put(buf[:0])
}
