# Design Document

## Overview

本设计文档描述了 easy_proxies 项目的重构方案。重构的核心目标是将现有的单体式代码结构拆分为职责清晰的模块，提高代码的可维护性、可测试性和可扩展性。

重构遵循以下原则：
- **单一职责原则**：每个模块只负责一个明确的功能
- **依赖倒置原则**：高层模块不依赖低层模块，都依赖抽象
- **接口隔离原则**：使用小而专注的接口
- **渐进式重构**：保持向后兼容，逐步迁移

## Architecture

### 当前架构问题

```
┌─────────────────────────────────────────────────────────────┐
│                        pool.go (1366 lines)                  │
│  ┌─────────────┐ ┌─────────────┐ ┌─────────────┐            │
│  │ Connection  │ │   Health    │ │   Member    │            │
│  │  Tracking   │ │   Check     │ │  Selection  │            │
│  └─────────────┘ └─────────────┘ └─────────────┘            │
│  ┌─────────────┐ ┌─────────────┐ ┌─────────────┐            │
│  │    HTTP     │ │  Blacklist  │ │   Probe     │            │
│  │   Probing   │ │   Logic     │ │  Functions  │            │
│  └─────────────┘ └─────────────┘ └─────────────┘            │
└─────────────────────────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│                    monitor/manager.go                        │
│  ┌─────────────┐ ┌─────────────┐ ┌─────────────┐            │
│  │    State    │ │    Probe    │ │     API     │            │
│  │   Tracking  │ │  Execution  │ │   Exposure  │            │
│  └─────────────┘ └─────────────┘ └─────────────┘            │
└─────────────────────────────────────────────────────────────┘
```

### 目标架构

```
┌─────────────────────────────────────────────────────────────┐
│                     internal/outbound/pool/                  │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐       │
│  │   pool.go    │  │  selector.go │  │  options.go  │       │
│  │  (核心调度)   │  │  (成员选择)   │  │  (配置处理)  │       │
│  └──────────────┘  └──────────────┘  └──────────────┘       │
│  ┌──────────────┐  ┌──────────────┐                         │
│  │   conn.go    │  │ blacklist.go │                         │
│  │  (连接追踪)   │  │  (黑名单)    │                         │
│  └──────────────┘  └──────────────┘                         │
└─────────────────────────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│                   internal/health/                           │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐       │
│  │  checker.go  │  │   tcp.go     │  │   http.go    │       │
│  │  (检查器)    │  │  (TCP探测)   │  │  (HTTP探测)  │       │
│  └──────────────┘  └──────────────┘  └──────────────┘       │
└─────────────────────────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│                   internal/protocol/                         │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐       │
│  │ registry.go  │  │   vless.go   │  │   vmess.go   │       │
│  │  (协议注册)   │  │  (VLESS)    │  │  (VMess)     │       │
│  └──────────────┘  └──────────────┘  └──────────────┘       │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐       │
│  │ shadowsocks  │  │  trojan.go   │  │ hysteria2.go │       │
│  │    .go       │  │  (Trojan)    │  │ (Hysteria2)  │       │
│  └──────────────┘  └──────────────┘  └──────────────┘       │
└─────────────────────────────────────────────────────────────┘
```

## Components and Interfaces

### 1. Health Checker 模块

```go
// internal/health/checker.go

// Prober 定义探测接口
type Prober interface {
    // Probe 执行健康检查，返回延迟和错误
    Probe(ctx context.Context) (time.Duration, error)
}

// ProbeResult 探测结果
type ProbeResult struct {
    Success   bool
    Latency   time.Duration
    Error     error
    Timestamp time.Time
}

// Checker 健康检查器
type Checker struct {
    tcpTimeout  time.Duration
    httpTimeout time.Duration
    probeURL    string
    logger      Logger
}

// NewChecker 创建健康检查器
func NewChecker(opts CheckerOptions) *Checker

// ProbeTCP 执行 TCP 连接探测
func (c *Checker) ProbeTCP(ctx context.Context, dialer Dialer, host string, port uint16) error

// ProbeHTTP 执行 HTTP 请求探测
func (c *Checker) ProbeHTTP(ctx context.Context, dialer Dialer, url string) error

// ProbeWithTimeout 带硬超时的探测包装
func (c *Checker) ProbeWithTimeout(ctx context.Context, timeout time.Duration, fn func() error) error
```

### 2. Blacklist Manager 模块

```go
// internal/outbound/pool/blacklist.go

// BlacklistState 黑名单状态
type BlacklistState struct {
    Blacklisted bool
    Until       time.Time
    Reason      string
}

// BlacklistManager 黑名单管理器
type BlacklistManager struct {
    duration  time.Duration
    threshold int
    mu        sync.RWMutex
    states    map[string]*nodeBlacklistState
}

// NewBlacklistManager 创建黑名单管理器
func NewBlacklistManager(threshold int, duration time.Duration) *BlacklistManager

// RecordFailure 记录失败，返回是否触发黑名单
func (m *BlacklistManager) RecordFailure(nodeID string, err error) bool

// RecordSuccess 记录成功，重置失败计数
func (m *BlacklistManager) RecordSuccess(nodeID string)

// IsBlacklisted 检查节点是否被黑名单
func (m *BlacklistManager) IsBlacklisted(nodeID string) BlacklistState

// Release 手动解除黑名单
func (m *BlacklistManager) Release(nodeID string)

// ExpireCheck 检查并清理过期的黑名单
func (m *BlacklistManager) ExpireCheck(now time.Time) []string
```

### 3. Connection Tracker 模块

```go
// internal/outbound/pool/conn.go

// TrackedConn 追踪的连接
type TrackedConn struct {
    net.Conn
    tracker     *ConnectionTracker
    nodeID      string
    createdAt   time.Time
    bytesRead   int64
    released    atomic.Bool
    onClose     func()
}

// ConnectionTracker 连接追踪器
type ConnectionTracker struct {
    maxConcurrent int32
    globalActive  atomic.Int32
    mu            sync.Mutex
    conns         []*TrackedConn
    logger        Logger
}

// NewConnectionTracker 创建连接追踪器
func NewConnectionTracker(maxConcurrent int) *ConnectionTracker

// Track 追踪新连接
func (t *ConnectionTracker) Track(conn net.Conn, nodeID string, onClose func()) *TrackedConn

// Release 释放连接
func (t *ConnectionTracker) Release(conn *TrackedConn)

// ActiveCount 获取活跃连接数
func (t *ConnectionTracker) ActiveCount() int32

// EvictOldest 驱逐最老的连接
func (t *ConnectionTracker) EvictOldest() bool
```

### 4. Member Selector 模块

```go
// internal/outbound/pool/selector.go

// SelectionMode 选择模式
type SelectionMode string

const (
    ModeSequential SelectionMode = "sequential"
    ModeRandom     SelectionMode = "random"
    ModeBalance    SelectionMode = "balance"
)

// Selector 成员选择器接口
type Selector interface {
    Select(candidates []*Member) *Member
}

// SequentialSelector 顺序选择器（轮询）
type SequentialSelector struct {
    counter atomic.Uint64
}

// RandomSelector 随机选择器
type RandomSelector struct {
    rng   *rand.Rand
    mu    sync.Mutex
}

// BalanceSelector 负载均衡选择器
type BalanceSelector struct{}

// NewSelector 根据模式创建选择器
func NewSelector(mode SelectionMode) Selector
```

### 5. Protocol Registry 模块

```go
// internal/protocol/registry.go

// Parser 协议解析器接口
type Parser interface {
    // Scheme 返回支持的协议 scheme
    Scheme() string
    // Parse 解析 URI 为 sing-box outbound 配置
    Parse(uri string) (option.Outbound, error)
}

// Registry 协议注册表
type Registry struct {
    parsers map[string]Parser
}

// NewRegistry 创建协议注册表
func NewRegistry() *Registry

// Register 注册协议解析器
func (r *Registry) Register(parser Parser)

// Parse 解析 URI
func (r *Registry) Parse(tag, uri string) (option.Outbound, error)

// SupportedSchemes 返回支持的协议列表
func (r *Registry) SupportedSchemes() []string
```

### 6. Build Error 结构化错误

```go
// internal/builder/errors.go

// BuildError 构建错误
type BuildError struct {
    NodeIndex int
    NodeName  string
    URI       string
    Reason    string
    Cause     error
}

func (e *BuildError) Error() string
func (e *BuildError) Unwrap() error

// BuildResult 构建结果
type BuildResult struct {
    Outbound option.Outbound
    Error    *BuildError
}

// BatchBuildErrors 批量构建错误
type BatchBuildErrors struct {
    Errors []*BuildError
}

func (e *BatchBuildErrors) Error() string
func (e *BatchBuildErrors) Failed() int
func (e *BatchBuildErrors) ByReason() map[string][]*BuildError
```

## Data Models

### 配置统一处理

```go
// internal/config/defaults.go

// Defaults 所有默认值集中定义
var Defaults = struct {
    Mode                string
    ListenerAddress     string
    ListenerPort        uint16
    PoolMode            string
    FailureThreshold    int
    BlacklistDuration   time.Duration
    MaxConcurrent       int
    ProbeConcurrency    int
    TCPProbeTimeout     time.Duration
    HTTPProbeTimeout    time.Duration
    DialTimeout         time.Duration
    FirstByteTimeout    time.Duration
    StateFlushInterval  time.Duration
    ManagementListen    string
    ManagementProbeTarget string
}{
    Mode:                "pool",
    ListenerAddress:     "0.0.0.0",
    ListenerPort:        2323,
    PoolMode:            "sequential",
    FailureThreshold:    3,
    BlacklistDuration:   24 * time.Hour,
    MaxConcurrent:       50,
    ProbeConcurrency:    50,
    TCPProbeTimeout:     1 * time.Second,
    HTTPProbeTimeout:    3 * time.Second,
    DialTimeout:         1 * time.Second,
    FirstByteTimeout:    2 * time.Second,
    StateFlushInterval:  30 * time.Second,
    ManagementListen:    "127.0.0.1:9090",
    ManagementProbeTarget: "www.apple.com:80",
}
```

### 节点状态统一模型

```go
// internal/state/node.go

// NodeState 节点状态（单一数据源）
type NodeState struct {
    ID               string        `json:"id"`
    Name             string        `json:"name"`
    URI              string        `json:"uri"`
    
    // 运行时状态
    Available        bool          `json:"available"`
    InitialCheckDone bool          `json:"initial_check_done"`
    ActiveConns      int32         `json:"active_conns"`
    
    // 黑名单状态
    Blacklisted      bool          `json:"blacklisted"`
    BlacklistedUntil time.Time     `json:"blacklisted_until"`
    FailureCount     int           `json:"failure_count"`
    
    // 统计信息
    LastSuccess      time.Time     `json:"last_success"`
    LastFailure      time.Time     `json:"last_failure"`
    LastError        string        `json:"last_error"`
    LastLatency      time.Duration `json:"last_latency"`
}
```

## Correctness Properties

*A property is a characteristic or behavior that should hold true across all valid executions of a system-essentially, a formal statement about what the system should do. Properties serve as the bridge between human-readable specifications and machine-verifiable correctness guarantees.*

