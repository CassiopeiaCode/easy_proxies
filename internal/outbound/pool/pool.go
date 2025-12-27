package pool

import (
	"context"
	"errors"
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
	// tcpProbeTimeout bounds how long we wait for establishing a TCP
	// connection to a node during health checks. This is intentionally kept
	// short so that a few slow or stuck nodes do not delay the whole pool.
	tcpProbeTimeout = 1 * time.Second
	// httpProbeTimeout bounds the full HTTP probe (including TLS handshake).
	// It can be slightly longer than tcpProbeTimeout because it covers more
	// work than a bare TCP connect.
	httpProbeTimeout = 3 * time.Second
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
	// MaxConcurrent limits the maximum number of active upstream connections
	// managed by this pool. When the limit is exceeded, the oldest active
	// connection is closed to make room for new ones. A value <= 0 disables
	// this behaviour.
	MaxConcurrent int
	// ProbeConcurrency controls how many nodes are probed in parallel during
	// health checks. If <= 0, a safe default is chosen by the caller.
	ProbeConcurrency int
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

	// globalActive tracks the total number of active connections across all
	// members managed by this pool. It is used together with maxConcurrent and
	// activeConns to implement a global concurrency cap with "drop oldest"
	// semantics when the cap is exceeded.
	maxConcurrent int32
	globalActive  atomic.Int32
	connsMu       sync.Mutex
	activeConns   []*trackedConn

	// startupProbeRunning indicates whether a full initial health check is
	// currently running. It is used by the health-check daemon to avoid
	// starting multiple concurrent startup probes when the pool becomes
	// temporarily empty.
	startupProbeRunning atomic.Bool
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

	// Pre-allocate activeConns slice with reasonable capacity
	initialConnCap := 64
	if normalized.MaxConcurrent > 0 && normalized.MaxConcurrent < initialConnCap {
		initialConnCap = normalized.MaxConcurrent
	}

	p := &poolOutbound{
		Adapter:       outbound.NewAdapter(Type, tag, []string{N.NetworkTCP, N.NetworkUDP}, normalized.Members),
		ctx:           ctx,
		logger:        logger,
		manager:       manager,
		options:       normalized,
		mode:          normalized.Mode,
		rng:           rand.New(rand.NewSource(time.Now().UnixNano())),
		monitor:       monitorMgr,
		maxConcurrent: int32(normalized.MaxConcurrent),
		activeConns:   make([]*trackedConn, 0, initialConnCap),
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
	// Propagate reasonable defaults for concurrency-related settings if the
	// builder did not fill them explicitly. These defaults mirror the config
	// layer: a MaxConcurrent/ProbeConcurrency of 0 is treated as 50.
	if options.MaxConcurrent < 0 {
		options.MaxConcurrent = 0
	}
	if options.ProbeConcurrency <= 0 {
		if options.MaxConcurrent > 0 {
			options.ProbeConcurrency = options.MaxConcurrent
		} else {
			options.ProbeConcurrency = 50
		}
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
	// 这里打点 Start 调用的阶段，方便排查 sing-box 在何时调用 pool 的生命周期方法。
	p.logger.Info("pool.Start: entered with stage=", stage)
	if stage != adapter.StartStateStart {
		return nil
	}
	p.mu.Lock()
	err := p.initializeMembersLocked()
	memberCount := len(p.members)
	if err == nil {
		// Build initial availability view. At this point most nodes haven't been
		// health-checked yet, so the result may be empty; it will be refreshed
		// after startup probes and on state changes.
		p.refreshAvailabilityLocked(time.Now(), false)
	}
	p.mu.Unlock()
	if err != nil {
		p.logger.Warn("pool.Start: proxy pool initialization skipped due to error: ", err)
		return nil
	}
	p.logger.Info("pool.Start: members initialized, total members=", memberCount)
	// 在初始化完成后，尝试触发一次健康检查守护任务；如果当前已有健康检查在运行，
	// 守护任务会自动跳过。
	if p.monitor != nil {
		p.logger.Info("pool.Start: monitor present, requesting initial health check daemon")
		p.triggerStartupHealthCheck("StartStateStart")
	} else {
		p.logger.Warn("pool.Start: monitor is nil, skipping initial health check")
	}
	return nil
}

// Close implements adapter.Lifecycle interface.
// This is required for sing-box to call Start() on the outbound.
func (p *poolOutbound) Close() error {
	p.logger.Info("pool.Close: shutting down")
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

	// Decide how many probes may run in parallel. We prefer the per-pool
	// configuration from options; if it is unset, fall back to a safe
	// hard-coded default.
	workerCount := p.options.ProbeConcurrency
	if workerCount <= 0 {
		workerCount = 50
	}
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

				// 为了避免依赖 sing-box 内部的超时行为，这里在每个 probe 外再包一层
				// “硬超时”保护：一旦超过 tcpProbeTimeout/httpProbeTimeout，我们就认为
				// 该节点探测超时并继续处理下一个节点，即使底层 DialContext 仍在阻塞。
				tcpErr := p.runProbeWithHardTimeout("tcp", tcpProbeTimeout, func() error {
					tcpCtx, tcpCancel := context.WithTimeout(p.ctx, tcpProbeTimeout)
					defer tcpCancel()
					return p.probeTCP(tcpCtx, member, host, port)
				})
				if tcpErr != nil {
					p.recordInitialProbeFailure(member, fmt.Errorf("tcp dial: %w", tcpErr))
					failedCount.Add(1)
					continue
				}

				httpErr := p.runProbeWithHardTimeout("http", httpProbeTimeout, func() error {
					httpCtx, httpCancel := context.WithTimeout(p.ctx, httpProbeTimeout)
					defer httpCancel()
					// During startup probes, reuse the workerCount as the
					// per-host HTTP connection pool size to avoid creating
					// more concurrent HTTP requests than there are probe
					// workers.
					return p.probeHTTP(httpCtx, member, probeURL, workerCount)
				})
				if httpErr != nil {
					p.recordInitialProbeFailure(member, httpErr)
					failedCount.Add(1)
					continue
				}

				latency := time.Since(start)
				p.recordInitialProbeSuccess(member, latency)
				newCount := availableCount.Add(1)

				// 立即刷新可调度列表，让节点尽快可用
				// 第一个成功的节点立即刷新，之后每 5 个刷新一次
				if newCount == 1 || newCount%5 == 0 {
					p.mu.Lock()
					p.refreshAvailabilityLocked(time.Now(), false)
					p.mu.Unlock()
					if newCount == 1 {
						p.logger.Info("first node available, pool is now schedulable")
					}
				}
			}
		}()
	}

	for _, member := range members {
		jobs <- member
	}
	close(jobs)
	wg.Wait()

	p.logger.Info("initial health check completed: ", availableCount.Load(), " available, ", failedCount.Load(), " failed")
	// Final refresh to ensure all successful nodes are included.
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

// runProbeWithHardTimeout wraps a single probe (TCP or HTTP) with an extra
// "hard" timeout. This protects the health-check worker from being stuck if
// the underlying sing-box outbound ignores context deadlines or otherwise
// blocks for longer than expected. From the worker's point of view, this
// function never blocks longer than the provided timeout.
func (p *poolOutbound) runProbeWithHardTimeout(kind string, timeout time.Duration, fn func() error) error {
	// If timeout is non-positive, fall back to running the probe directly.
	if timeout <= 0 {
		return fn()
	}

	resultCh := make(chan error, 1)
	go func() {
		// Ensure the goroutine never blocks the caller: the channel is
		// buffered, so writing once will not block even if the caller has
		// already given up due to timeout.
		resultCh <- fn()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-resultCh:
		return err
	case <-timer.C:
		// From the health-check scheduler's perspective this probe has timed
		// out. The underlying goroutine may still be blocked in DialContext or
		// HTTP I/O, but the worker can move on to the next node.
		return context.DeadlineExceeded
	}
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
		p.logger.Warn("pickMember: no candidates on fast path for network=", network, ", rebuilding availability")
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
			p.logger.Warn("pickMember: still no candidates after availability refresh for network=", network)
			// 如果在慢路径中仍然找不到任何可调度节点，则触发健康检查守护线程：
			// 当且仅当当前没有健康检查在运行时，会重置初始健康检查状态并启动一次
			// 全量并行探测。当前请求依然会返回错误，但后续请求有机会使用新的健康
			// 检查结果。
			p.triggerStartupHealthCheck("no_schedulable_nodes")
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

	// 打点当前可调度视图，便于观察在黑名单/健康检查之后的实际可用节点数量。
	p.logger.Info("pool.refreshAvailabilityLocked: updated availability snapshot, all=", len(all),
		", tcp=", len(tcp), ", udp=", len(udp), ", allowRelease=", allowRelease)
}

// triggerStartupHealthCheck launches a background "health-check daemon" which
// (re)runs the full initial health check when the pool becomes empty. It is
// safe to call this from multiple code paths; only one health check will run
// at a time.
func (p *poolOutbound) triggerStartupHealthCheck(reason string) {
	if p.monitor == nil {
		p.logger.Warn("health-check daemon: monitor is nil, skip trigger, reason=", reason)
		return
	}
	if !p.startupProbeRunning.CompareAndSwap(false, true) {
		p.logger.Info("health-check daemon: probe already running, skip trigger, reason=", reason)
		return
	}

	go func() {
		defer p.startupProbeRunning.Store(false)
		p.logger.Info("health-check daemon: starting initial health check, reason=", reason)

		// 确保成员已经初始化。
		p.mu.Lock()
		if len(p.members) == 0 {
			if err := p.initializeMembersLocked(); err != nil {
				p.mu.Unlock()
				p.logger.Warn("health-check daemon: initialize members failed: ", err)
				return
			}
		}

		// 重置初始健康检查状态，使得接下来的探测可以从干净的视图重新评估可用性。
		for _, member := range p.members {
			if member.entry != nil {
				member.entry.ResetInitialCheck()
			}
		}
		// 立即刷新一次可调度视图，确保在健康检查完成前，调度器不会误用旧的可用性。
		p.refreshAvailabilityLocked(time.Now(), false)
		p.mu.Unlock()

		// 复用现有的并行初始健康检查逻辑。
		p.probeAllMembersOnStartup()
	}()
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

	// 注意：这里的 FailureThreshold 只作用于“运行时”黑名单逻辑：
	// - member.failures 是进程内计数器，达到阈值即触发一次黑名单，并在触发后清零；
	// - 状态持久化里的 FailureCount 是累计值，仅用于 WebUI 展示，不参与阈值判断；
	// - 启动时 filterBlockedNodes 只依据 Enabled/BlacklistUntil 判定是否跳过节点，
	//   不会根据历史 FailureCount 自动禁用。
	//
	// 因此在面板上看到某些节点的 failure_count 看似“超过阈值但仍未禁用”是预期行为：
	// 它们可能已经经历过多次黑名单/恢复周期，但当前并未处于禁用状态。
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
	tc := &trackedConn{
		Conn:         conn,
		member:       member,
		pool:         p,
		bytesRead:    0,
		checkPending: true,
	}
	tc.release = func() {
		p.decActive(member)
		p.unregisterConn(tc)
	}
	p.registerConn(tc)
	return tc
}

func (p *poolOutbound) wrapPacketConn(conn net.PacketConn, member *memberState) net.PacketConn {
	return &trackedPacketConn{PacketConn: conn, release: func() {
		p.decActive(member)
	}}
}

// registerConn records a newly established connection for global concurrency
// tracking and enforces the MaxConcurrent limit (if configured). When the
// limit is exceeded, the oldest still-active connection is closed to make
// room for the new one.
func (p *poolOutbound) registerConn(c *trackedConn) {
	if p.maxConcurrent <= 0 {
		return
	}
	c.createdAt = time.Now()

	p.connsMu.Lock()
	p.activeConns = append(p.activeConns, c)
	p.connsMu.Unlock()

	newTotal := p.globalActive.Add(1)
	if newTotal <= p.maxConcurrent {
		return
	}

	// Limit exceeded: find the oldest active connection and close it to free
	// capacity. We perform the potentially blocking Close call outside the
	// lock to avoid deadlocks.
	oldest := p.findOldestActiveConn()
	if oldest == nil || oldest == c {
		return
	}
	go oldest.Close()
}

// unregisterConn removes a connection from the global tracking structures and
// decrements the active counter. It is safe to call multiple times for the
// same connection; only the first call will take effect.
func (p *poolOutbound) unregisterConn(c *trackedConn) {
	if p.maxConcurrent <= 0 {
		return
	}
	// Ensure we only decrement once per connection.
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	p.globalActive.Add(-1)

	// Periodically compact activeConns to prevent unbounded memory growth.
	// We only compact when the slice grows beyond a threshold to avoid
	// excessive allocations on the hot path.
	p.connsMu.Lock()
	if len(p.activeConns) > 200 {
		alive := make([]*trackedConn, 0, len(p.activeConns)/2)
		for _, conn := range p.activeConns {
			if conn != nil && !conn.closed.Load() {
				alive = append(alive, conn)
			}
		}
		p.activeConns = alive
	}
	p.connsMu.Unlock()
}

// findOldestActiveConn returns the oldest connection that is still considered
// active from the pool's perspective. It ignores connections that have been
// marked closed. Also performs opportunistic cleanup of closed connections.
func (p *poolOutbound) findOldestActiveConn() *trackedConn {
	if p.maxConcurrent <= 0 {
		return nil
	}
	p.connsMu.Lock()
	defer p.connsMu.Unlock()

	var oldest *trackedConn
	closedCount := 0
	for _, c := range p.activeConns {
		if c == nil {
			continue
		}
		if c.closed.Load() {
			closedCount++
			continue
		}
		if oldest == nil || c.createdAt.Before(oldest.createdAt) {
			oldest = c
		}
	}

	// Opportunistic cleanup: if more than half are closed, compact now
	if closedCount > len(p.activeConns)/2 && len(p.activeConns) > 50 {
		alive := make([]*trackedConn, 0, len(p.activeConns)-closedCount)
		for _, c := range p.activeConns {
			if c != nil && !c.closed.Load() {
				alive = append(alive, c)
			}
		}
		p.activeConns = alive
	}

	return oldest
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
// Uses a shared transport pool to reduce memory allocations during health checks.
func (p *poolOutbound) probeHTTP(ctx context.Context, member *memberState, url string, poolSize int) error {
	// Create HTTP client that uses the proxy outbound for dialing
	if poolSize <= 0 {
		poolSize = 1
	}
	transport := &http.Transport{
		MaxConnsPerHost:       1, // Only need 1 connection per probe
		MaxIdleConns:          1,
		MaxIdleConnsPerHost:   1,
		IdleConnTimeout:       5 * time.Second,
		DisableKeepAlives:     true, // Don't keep connections alive after probe
		ResponseHeaderTimeout: httpProbeTimeout,
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
		Timeout:   httpProbeTimeout,
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

	// 对于 HTTP 探测，我们只关心“能否可靠收取到一定量的数据”，而不是返回码本身：
	// - 状态码非 200 也视为成功（例如 3xx/4xx/5xx），只要连接建立且有足够的响应体；
	// - 仅在连接失败、请求出错或总接收字节数不足 10 字节时视为失败。
	//
	// 这里按最多读取 10 字节来判定“是否有足够数据”，避免对探测目标造成不必要负载。
	const minProbeBytes = 10
	var readBytes int

	buf := make([]byte, minProbeBytes)
	for readBytes < minProbeBytes {
		n, err := resp.Body.Read(buf[readBytes:])
		if n > 0 {
			readBytes += n
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// 提前 EOF 且不足 minProbeBytes，认为响应数据太少，判定为失败。
				if readBytes < minProbeBytes {
					return fmt.Errorf("probe response too short: got %d bytes, want at least %d", readBytes, minProbeBytes)
				}
				// 读到足够数据再 EOF 视为成功。
				break
			}
			// 其他 I/O 错误直接视为失败。
			return fmt.Errorf("read response body: %w", err)
		}
		if readBytes >= minProbeBytes {
			break
		}
	}

	if readBytes < minProbeBytes {
		return fmt.Errorf("probe response too short: got %d bytes, want at least %d", readBytes, minProbeBytes)
	}

	// 其余响应体（如果有）直接丢弃。
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
		err := p.probeHTTP(ctx, member, probeURL, p.options.ProbeConcurrency)
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
		err := p.probeHTTP(ctx, member, probeURL, p.options.ProbeConcurrency)
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
	checkPending bool        // whether we still need to validate minimum bytes
	once         sync.Once   // guards release so it only runs once
	mu           sync.Mutex  // protects mutable fields on trackedConn
	release      func()      // invoked exactly once when the connection is fully closed
	createdAt    time.Time   // creation time used for "oldest connection" eviction
	closed       atomic.Bool // marks whether unregister/release has run
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

	if err != nil {
		// If the upstream side errors out before we see enough data, treat it as
		// a failure for health/blacklist purposes.
		if checkNeeded {
			c.pool.recordFailure(c.member, fmt.Errorf("connection closed after %d bytes (expected at least 10): %w", c.bytesRead, err))
			// We have already recorded this failure; avoid counting it again in
			// Close by marking the check as completed.
			c.mu.Lock()
			c.checkPending = false
			c.mu.Unlock()
		}

		// 无论错误类型（超时、EOF 或其他 I/O 错误）如何，都立即在这里关闭
		// 上游连接，而不是依赖 sing-box 在未来某个时刻再调用 Close：
		// - 可以确保用户侧连接尽快感知到故障，而不是长时间挂起；
		// - 也避免“读出错但连接仍被认为是活跃”的状态泄漏。
		//
		// Close 可能会被调用多次（例如上层在收到错误后再次调用 Close），
		// 但 trackedConn 内部通过 once/closed 标记保证释放逻辑只执行一次。
		_ = c.Close()
	} else if c.bytesRead >= 10 {
		// Once we've read 10 bytes, mark the check as done.
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
