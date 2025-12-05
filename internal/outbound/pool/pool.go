package pool

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
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

const (
	startupHealthCheckConcurrency = 200
	tcpProbeTimeout               = 2 * time.Second
	httpProbeTimeout              = 5 * time.Second
	httpProbePoolSize             = startupHealthCheckConcurrency
	// dialTimeout bounds how long we wait for establishing a connection to an
	// upstream proxy node for regular client traffic. This only affects pool
	// outbounds and does not change sing-box global defaults.
	dialTimeout = 1 * time.Second
	// firstByteTimeout is the maximum time we wait for the first byte from the
	// upstream server after a successful connection. If no data is received
	// within this window, the connection is treated as a failure.
	firstByteTimeout = 2 * time.Second
)

// Options controls pool outbound behaviour.
type Options struct {
	Mode              string
	Members           []string
	FailureThreshold  int
	BlacklistDuration time.Duration
	Metadata          map[string]MemberMeta
}

// MemberMeta carries optional descriptive information for monitoring UI.
type MemberMeta struct {
	Name          string
	URI           string
	Mode          string
	ListenAddress string
	Port          uint16
	EndpointID    string
	EndpointHost  string
	EndpointPort  uint16
	Scheme        string
}

// Register wires the pool outbound into the registry.
func Register(registry *outbound.Registry) {
	outbound.Register[Options](registry, Type, newPool)
}

type memberState struct {
	outbound adapter.Outbound
	tag      string

	failures         int
	blacklisted      bool
	blacklistedUntil time.Time
	active           atomic.Int32
	entry            *monitor.EntryHandle
}

type poolOutbound struct {
	outbound.Adapter
	ctx    context.Context
	logger log.ContextLogger

	manager adapter.OutboundManager
	options Options
	mode    string

	// All nodes in the pool (static after initialization).
	members []*memberState

	// mu protects mutable fields on memberState and the members slice itself.
	mu sync.Mutex

	// Precomputed views of "currently usable" members, maintained off the hot path.
	// These are read in O(1) on the request path to avoid scanning the full pool.
	availableAny []*memberState
	availableTCP []*memberState
	availableUDP []*memberState

	// availableMu protects the precomputed available slices.
	availableMu sync.RWMutex

	// rrCounter is used for round-robin selection without taking the main mutex.
	rrCounter atomic.Uint64

	// rng is used only for random mode, guarded by rngMu.
	rng   *rand.Rand
	rngMu sync.Mutex

	monitor *monitor.Manager
}

func newPool(ctx context.Context, _ adapter.Router, logger log.ContextLogger, tag string, options Options) (adapter.Outbound, error) {
	if len(options.Members) == 0 {
		return nil, E.New("pool requires at least one member")
	}
	manager := service.FromContext[adapter.OutboundManager](ctx)
	if manager == nil {
		return nil, E.New("missing outbound manager in context")
	}
	monitorMgr := monitor.FromContext(ctx)
	normalized := normalizeOptions(options)
	p := &poolOutbound{
		Adapter: outbound.NewAdapter(Type, tag, []string{N.NetworkTCP, N.NetworkUDP}, normalized.Members),
		ctx:     ctx,
		logger:  logger,
		manager: manager,
		options: normalized,
		mode:    normalized.Mode,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
		monitor: monitorMgr,
	}

	// Register nodes immediately if monitor is available
	if monitorMgr != nil {
		logger.Info("registering ", len(normalized.Members), " nodes to monitor")
		for _, memberTag := range normalized.Members {
			meta := normalized.Metadata[memberTag]
			info := monitor.NodeInfo{
				Tag:           memberTag,
				Name:          meta.Name,
				URI:           meta.URI,
				Mode:          meta.Mode,
				ListenAddress: meta.ListenAddress,
				Port:          meta.Port,
				EndpointID:    meta.EndpointID,
				EndpointHost:  meta.EndpointHost,
				EndpointPort:  meta.EndpointPort,
				Scheme:        meta.Scheme,
			}
			entry := monitorMgr.Register(info)
			if entry != nil {
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
	if err == nil {
		// Build initial availability view. At this point most nodes haven't been
		// health-checked yet, so the result may be empty; it will be refreshed
		// after startup probes and on state changes.
		p.refreshAvailabilityLocked(time.Now(), false)
	}
	p.mu.Unlock()
	if err != nil {
		p.logger.Warn("proxy pool initialization skipped: ", err)
		return nil
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
		member := &memberState{
			outbound: detour,
			tag:      tag,
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
				EndpointID:    meta.EndpointID,
				EndpointHost:  meta.EndpointHost,
				EndpointPort:  meta.EndpointPort,
				Scheme:        meta.Scheme,
			}
			entry := p.monitor.Register(info)
			if entry != nil {
				entry.SetRelease(p.makeReleaseFunc(member))
				if probe := p.makeProbeFunc(member); probe != nil {
					entry.SetProbe(probe)
				}
			}
			member.entry = entry
			if entry != nil {
				if blacklisted, until := entry.BlacklistState(); blacklisted {
					member.blacklisted = true
					member.blacklistedUntil = until
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
	probeURL, ok := p.monitor.ProbeURL()
	if !ok {
		p.logger.Warn("probe target not configured, skipping initial health check")
		// 没有配置探测目标时，标记所有节点为可用
		p.mu.Lock()
		for _, member := range p.members {
			if member.entry != nil {
				member.entry.MarkInitialCheckDone(true)
			}
		}
		// Rebuild availability view based on updated monitor state.
		p.refreshAvailabilityLocked(time.Now(), false)
		p.mu.Unlock()
		return
	}

	p.mu.Lock()
	members := make([]*memberState, len(p.members))
	copy(members, p.members)
	memberCount := len(members)
	p.mu.Unlock()

	p.logger.Info("starting initial health check for all nodes, total members: ", memberCount)

	// 为了避免在大规模节点场景下始终以相同顺序进行健康检查，这里随机打乱
	// 检查顺序，使得每次订阅刷新或重启时的探测顺序更加均匀。
	rand.Shuffle(len(members), func(i, j int) {
		members[i], members[j] = members[j], members[i]
	})

	host, port, err := parseProbeTarget(probeURL)
	if err != nil {
		p.logger.Warn("invalid probe target ", probeURL, ", skipping initial health check: ", err)
		p.mu.Lock()
		for _, member := range p.members {
			if member.entry != nil {
				member.entry.MarkInitialCheckDone(true)
			}
		}
		// Mark all as usable from the pool's perspective as well.
		p.refreshAvailabilityLocked(time.Now(), false)
		p.mu.Unlock()
		return
	}

	workerCount := startupHealthCheckConcurrency
	if len(members) < workerCount {
		workerCount = len(members)
	}
	if workerCount == 0 {
		p.logger.Warn("initial health check skipped: no members to probe")
		return
	}

	p.logger.Info("initial health check worker count: ", workerCount)

	jobs := make(chan *memberState, len(members))
	var wg sync.WaitGroup
	var availableCount atomic.Int32
	var failedCount atomic.Int32

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for member := range jobs {
				start := time.Now()

				tcpCtx, tcpCancel := context.WithTimeout(p.ctx, tcpProbeTimeout)
				tcpErr := p.probeTCP(tcpCtx, member, host, port)
				tcpCancel()
				if tcpErr != nil {
					p.recordInitialProbeFailure(member, fmt.Errorf("tcp dial: %w", tcpErr))
					failedCount.Add(1)
					continue
				}

				httpCtx, httpCancel := context.WithTimeout(p.ctx, httpProbeTimeout)
				httpErr := p.probeHTTP(httpCtx, member, probeURL, httpProbePoolSize)
				httpCancel()
				if httpErr != nil {
					p.recordInitialProbeFailure(member, httpErr)
					failedCount.Add(1)
					continue
				}

				latency := time.Since(start)
				p.recordInitialProbeSuccess(member, latency)
				availableCount.Add(1)
			}
		}()
	}

	for _, member := range members {
		jobs <- member
	}
	close(jobs)
	wg.Wait()

	p.logger.Info("initial health check completed: ", availableCount.Load(), " available, ", failedCount.Load(), " failed")
	// Refresh availability after initial probes have finished.
	p.mu.Lock()
	p.refreshAvailabilityLocked(time.Now(), false)
	p.mu.Unlock()
}

func parseProbeTarget(rawURL string) (string, uint16, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", 0, err
	}
	host := parsed.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("probe url missing host: %s", rawURL)
	}
	port := 80
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		port = 443
	case "http":
		port = 80
	}
	if p := parsed.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n <= 65535 {
			port = n
		}
	}
	return host, uint16(port), nil
}

func (p *poolOutbound) probeTCP(ctx context.Context, member *memberState, host string, port uint16) error {
	destination := M.ParseSocksaddrHostPort(host, port)
	conn, err := member.outbound.DialContext(ctx, N.NetworkTCP, destination)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func (p *poolOutbound) recordInitialProbeFailure(member *memberState, err error) {
	p.logger.Warn("initial probe failed for ", member.tag, ": ", err)
	// Treat initial probe failure as a hard failure: immediately blacklist the
	// node for BlacklistDuration so it is not used until either the blacklist
	// expires or a manual release happens. When the blacklist expires, the node
	// will still require a successful health check before it is considered
	// available again.
	p.blacklistMemberFromHealthCheck(member, err)
}

func (p *poolOutbound) recordInitialProbeSuccess(member *memberState, latency time.Duration) {
	p.logger.Info("initial probe success for ", member.tag, ", latency: ", latency.Milliseconds(), "ms")
	if member.entry != nil {
		member.entry.RecordSuccessWithLatency(latency)
		member.entry.MarkInitialCheckDone(true)
	}
}

func (p *poolOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	member, err := p.pickMember(network)
	if err != nil {
		return nil, err
	}
	p.incActive(member)
	// Wrap the context with a per-dial timeout unless the caller already
	// specified a shorter deadline.
	dialCtx := ctx
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > dialTimeout {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	conn, err := member.outbound.DialContext(dialCtx, network, destination)
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
	// Apply the same per-dial timeout semantics to UDP listen operations.
	dialCtx := ctx
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > dialTimeout {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	conn, err := member.outbound.ListenPacket(dialCtx, destination)
	if err != nil {
		p.decActive(member)
		p.recordFailure(member, err)
		return nil, err
	}
	p.recordSuccess(member)
	return p.wrapPacketConn(conn, member), nil
}

func (p *poolOutbound) pickMember(network string) (*memberState, error) {
	// Fast path: choose from precomputed available slices without scanning the
	// full pool or taking the main mutex.
	candidates := p.availableForNetwork(network)
	if len(candidates) == 0 {
		// Slow path: (re)build availability from the full pool and retry once.
		now := time.Now()
		p.mu.Lock()
		// Lazy initialization: initialize members on first use if not already done.
		if len(p.members) == 0 {
			if err := p.initializeMembersLocked(); err != nil {
				p.mu.Unlock()
				return nil, err
			}
		}
		// Recompute availability; if everything is blacklisted, try releasing them.
		p.refreshAvailabilityLocked(now, true)
		p.mu.Unlock()
		candidates = p.availableForNetwork(network)
		if len(candidates) == 0 {
			return nil, E.New("no healthy proxy available")
		}
	}
	return p.selectMember(candidates), nil
}

// availableForNetwork returns the precomputed available slice for the given
// network. It only uses a read lock and never scans the full pool.
func (p *poolOutbound) availableForNetwork(network string) []*memberState {
	p.availableMu.RLock()
	defer p.availableMu.RUnlock()
	switch network {
	case N.NetworkTCP:
		return p.availableTCP
	case N.NetworkUDP:
		return p.availableUDP
	default:
		return p.availableAny
	}
}

func (p *poolOutbound) releaseIfAllBlacklistedLocked(now time.Time) bool {
	if len(p.members) == 0 {
		return false
	}
	for _, member := range p.members {
		if !member.blacklisted {
			return false
		}
		if now.After(member.blacklistedUntil) {
			member.blacklisted = false
			member.blacklistedUntil = time.Time{}
			member.failures = 0
			if member.entry != nil {
				member.entry.ClearBlacklist()
			}
		}
	}
	for _, member := range p.members {
		if member.blacklisted {
			member.blacklisted = false
			member.blacklistedUntil = time.Time{}
			member.failures = 0
			if member.entry != nil {
				member.entry.ClearBlacklist()
			}
		}
	}
	p.logger.Warn("all upstream proxies were blacklisted, releasing them for retry")
	return true
}

// refreshAvailabilityLocked rebuilds the precomputed availability slices from
// the full member list. It must be called with p.mu held.
//
// This function is intentionally not called on the hot request path; instead it
// is triggered by state changes (startup probes, blacklist updates, etc.).
func (p *poolOutbound) refreshAvailabilityLocked(now time.Time, allowRelease bool) {
	// First scan to build candidate lists.
	all := make([]*memberState, 0, len(p.members))
	tcp := make([]*memberState, 0, len(p.members))
	udp := make([]*memberState, 0, len(p.members))

	for _, member := range p.members {
		// Expire blacklist if needed.
		if member.blacklisted && now.After(member.blacklistedUntil) {
			member.blacklisted = false
			member.blacklistedUntil = time.Time{}
			member.failures = 0
			if member.entry != nil {
				member.entry.ClearBlacklist()
			}
		}
		if member.blacklisted {
			continue
		}

		// Only include members that have been checked and are available.
		// If the monitor is not enabled, entry will be nil,
		// and this check is skipped, effectively treating all nodes as available.
		if member.entry != nil {
			checked, available := member.entry.IsAvailable()
			if !checked || !available {
				continue
			}
		}

		all = append(all, member)
		networks := member.outbound.Network()
		if len(networks) == 0 || common.Contains(networks, N.NetworkTCP) {
			tcp = append(tcp, member)
		}
		if len(networks) == 0 || common.Contains(networks, N.NetworkUDP) {
			udp = append(udp, member)
		}
	}

	// If nothing is available but everything is blacklisted, optionally release
	// them for retry and rebuild once.
	if allowRelease && len(all) == 0 {
		if p.releaseIfAllBlacklistedLocked(now) {
			all = all[:0]
			tcp = tcp[:0]
			udp = udp[:0]
			for _, member := range p.members {
				// After release, treat them as available regardless of monitor state.
				networks := member.outbound.Network()
				all = append(all, member)
				if len(networks) == 0 || common.Contains(networks, N.NetworkTCP) {
					tcp = append(tcp, member)
				}
				if len(networks) == 0 || common.Contains(networks, N.NetworkUDP) {
					udp = append(udp, member)
				}
			}
		}
	}

	p.availableMu.Lock()
	p.availableAny = all
	p.availableTCP = tcp
	p.availableUDP = udp
	p.availableMu.Unlock()
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
			active := member.active.Load()
			if selected == nil || active < minActive {
				selected = member
				minActive = active
			}
		}
		return selected
	default:
		// Round-robin selection over the precomputed candidate slice.
		// rrCounter is incremented atomically without taking the main mutex.
		index := p.rrCounter.Add(1) - 1
		return candidates[int(index)%len(candidates)]
	}
}

func (p *poolOutbound) recordFailure(member *memberState, cause error) {
	p.mu.Lock()
	member.failures++
	failures := member.failures
	threshold := p.options.FailureThreshold
	blacklisted := false
	if failures >= threshold {
		member.failures = 0
		member.blacklisted = true
		member.blacklistedUntil = time.Now().Add(p.options.BlacklistDuration)
		blacklisted = true
	}
	// If this failure caused the member to be blacklisted, refresh availability
	// so that it is removed from the fast-path candidate lists.
	if blacklisted {
		p.refreshAvailabilityLocked(time.Now(), false)
	}
	p.mu.Unlock()

	if member.entry != nil {
		member.entry.RecordFailure(cause)
		if failures >= threshold {
			member.entry.Blacklist(member.blacklistedUntil)
		}
	}
	if failures >= threshold {
		p.logger.Warn("proxy ", member.tag, " blacklisted for ", p.options.BlacklistDuration, ": ", cause)
	} else {
		p.logger.Warn("proxy ", member.tag, " failure ", failures, "/", threshold, ": ", cause)
	}
}

func (p *poolOutbound) recordSuccess(member *memberState) {
	p.mu.Lock()
	member.failures = 0
	p.mu.Unlock()
	if member.entry != nil {
		member.entry.RecordSuccess()
		if !member.blacklisted {
			member.entry.ClearBlacklist()
		}
	}
}

func (p *poolOutbound) wrapConn(conn net.Conn, member *memberState) net.Conn {
	return &trackedConn{
		Conn:         conn,
		member:       member,
		pool:         p,
		bytesRead:    0,
		checkPending: true,
		release: func() {
			p.decActive(member)
		},
	}
}

func (p *poolOutbound) wrapPacketConn(conn net.PacketConn, member *memberState) net.PacketConn {
	return &trackedPacketConn{PacketConn: conn, release: func() {
		p.decActive(member)
	}}
}

func (p *poolOutbound) makeReleaseFunc(member *memberState) func() {
	return func() {
		p.mu.Lock()
		member.blacklisted = false
		member.blacklistedUntil = time.Time{}
		member.failures = 0
		p.refreshAvailabilityLocked(time.Now(), false)
		p.mu.Unlock()
		if member.entry != nil {
			member.entry.ClearBlacklist()
		}
	}
}

// blacklistMemberFromHealthCheck marks a member as blacklisted due to a failed
// health check (initial or periodic) and updates both the in-memory pool state
// and the monitoring/state subsystems. The node will remain excluded from the
// scheduling fast path for at least BlacklistDuration. When the blacklist
// expires, the node is still considered unavailable until a subsequent health
// check succeeds.
func (p *poolOutbound) blacklistMemberFromHealthCheck(member *memberState, cause error) {
	now := time.Now()
	until := now.Add(p.options.BlacklistDuration)

	// Update pool-local blacklist state and availability views.
	p.mu.Lock()
	member.blacklisted = true
	member.blacklistedUntil = until
	member.failures = 0
	p.refreshAvailabilityLocked(now, false)
	p.mu.Unlock()

	// Update monitoring + persisted state.
	if member.entry != nil {
		member.entry.RecordFailure(cause)
		member.entry.Blacklist(until)
		// Mark initial check as completed but unavailable; this ensures UI
		// treats the node as failed, and the pool scheduler will not consider
		// it available until a later health check succeeds.
		member.entry.MarkInitialCheckDone(false)
	}

	p.logger.Warn("health check blacklisted proxy ", member.tag, " for ", p.options.BlacklistDuration, ": ", cause)
}

// probeHTTP performs HTTP GET request through the proxy to verify connectivity
func (p *poolOutbound) probeHTTP(ctx context.Context, member *memberState, url string, poolSize int) error {
	// Create HTTP client that uses the proxy outbound for dialing
	if poolSize <= 0 {
		poolSize = 1
	}
	transport := &http.Transport{
		MaxConnsPerHost:     poolSize,
		MaxIdleConns:        poolSize,
		MaxIdleConnsPerHost: poolSize,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Parse the address to get host and port
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			portNum := uint16(80)
			if port == "443" {
				portNum = 443
			}
			destination := M.ParseSocksaddrHostPort(host, portNum)
			return member.outbound.DialContext(ctx, network, destination)
		},
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Don't follow redirects
		},
	}
	defer transport.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Read and discard response body to ensure full response
	_, _ = io.Copy(io.Discard, resp.Body)

	return nil
}

func (p *poolOutbound) makeProbeFunc(member *memberState) func(ctx context.Context) (time.Duration, error) {
	if p.monitor == nil {
		return nil
	}
	probeURL, ok := p.monitor.ProbeURL()
	if !ok {
		return nil
	}
	return func(ctx context.Context) (time.Duration, error) {
		start := time.Now()
		err := p.probeHTTP(ctx, member, probeURL, httpProbePoolSize)
		if err != nil {
			// Any health check failure immediately blacklists the node for
			// BlacklistDuration so that it is not used until the blacklist
			// expires or is manually released.
			p.blacklistMemberFromHealthCheck(member, err)
			return 0, err
		}
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
	probeURL, ok := p.monitor.ProbeURL()
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
		err := p.probeHTTP(ctx, member, probeURL, httpProbePoolSize)
		if err != nil {
			// Same semantics as makeProbeFunc: immediately blacklist on health
			// check failure so that this node is not used until the blacklist
			// window expires or the user manually releases it.
			p.blacklistMemberFromHealthCheck(member, err)
			return 0, err
		}
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
		// Ensure members are initialized
		p.mu.Lock()
		if len(p.members) == 0 {
			if err := p.initializeMembersLocked(); err != nil {
				// Initialization failed; nothing else to do here.
				p.mu.Unlock()
				return
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

		if member != nil {
			member.blacklisted = false
			member.blacklistedUntil = time.Time{}
			member.failures = 0
			p.refreshAvailabilityLocked(time.Now(), false)
		}
		p.mu.Unlock()

		if member != nil && member.entry != nil {
			member.entry.ClearBlacklist()
		}
	}
}

type trackedConn struct {
	net.Conn
	member       *memberState
	pool         *poolOutbound
	bytesRead    int64
	checkPending bool
	once         sync.Once
	mu           sync.Mutex
	release      func()
	// firstReadDeadlineSet is used to ensure we only install the first-byte
	// timeout once, on the first Read call when no data has been received yet.
	firstReadDeadlineSet bool
}

func (c *trackedConn) Read(b []byte) (n int, err error) {
	// If this is the first Read and we haven't seen any data yet, enforce a
	// first-byte timeout: if the upstream server does not send any data within
	// firstByteTimeout, the read will fail with a timeout error. This failure is
	// then counted against the node.
	c.mu.Lock()
	needDeadline := !c.firstReadDeadlineSet && c.bytesRead == 0
	if needDeadline {
		c.firstReadDeadlineSet = true
	}
	c.mu.Unlock()
	if needDeadline {
		_ = c.Conn.SetReadDeadline(time.Now().Add(firstByteTimeout))
	}

	n, err = c.Conn.Read(b)

	c.mu.Lock()
	c.bytesRead += int64(n)
	checkNeeded := c.checkPending && c.bytesRead < 10
	// Once we have received any data, clear the read deadline so that subsequent
	// traffic is not subject to the first-byte timeout.
	if c.bytesRead > 0 {
		_ = c.Conn.SetReadDeadline(time.Time{})
	}
	c.mu.Unlock()

	// If connection is closing and we haven't received 10 bytes, record failure
	if err != nil && checkNeeded {
		c.pool.recordFailure(c.member, fmt.Errorf("connection closed after %d bytes (expected at least 10): %w", c.bytesRead, err))
	} else if c.bytesRead >= 10 {
		// Once we've read 10 bytes, mark the check as done
		c.mu.Lock()
		c.checkPending = false
		c.mu.Unlock()
	}

	return n, err
}

func (c *trackedConn) Close() error {
	c.mu.Lock()
	checkNeeded := c.checkPending && c.bytesRead < 10
	bytesRead := c.bytesRead
	c.mu.Unlock()

	// If closing with less than 10 bytes read, record as failure
	if checkNeeded {
		c.pool.recordFailure(c.member, fmt.Errorf("connection closed with only %d bytes received (expected at least 10)", bytesRead))
	}

	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
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
	member.active.Add(1)
	if member.entry != nil {
		member.entry.IncActive()
	}
}

func (p *poolOutbound) decActive(member *memberState) {
	member.active.Add(-1)
	if member.entry != nil {
		member.entry.DecActive()
	}
}
