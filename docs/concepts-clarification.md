# 项目概念澄清（准确口径）

本文档用于澄清 easy_proxies（本 fork）中的关键概念、状态口径与模块边界，避免“看起来像一回事但实现口径不同”的误解。本文优先以代码行为为准，并给出对应实现位置，便于追溯与校验。

---

## 1. 系统目标与核心数据流

### 1.1 系统目标
- 管理一组“代理节点”（Node），把它们转换成 sing-box 的 outbounds，并提供：
  - 单入口的节点池（Pool）调度
  - 每节点独立端口（Multi-Port）
  - 混合（Hybrid）
- 提供运行态监控（WebUI/API）：节点可用性、延迟、失败、活跃连接、导出链接等。
- （本 fork）提供 SQLite 持久化：节点主数据、damaged 标记、健康统计（按小时聚合）与基于 24h 成功率的调度过滤。

### 1.2 关键数据路径（概览）
1) 配置加载：
- 从 config.yaml 解析出 `cfg`，并（可选）加载 `nodes_file` 追加到 `cfg.Nodes`。
- 当 `subscriptions` 配置存在且本地没有任何节点时，会在启动时做一次“订阅拉取引导”（仅用于能启动 sing-box，不等同于运行期刷新循环）。

2) DB（可选）成为运行时主数据源：
- 启动时把 `cfg.Nodes` best-effort upsert 入库（按 host:port 去重）。
- 随后从 DB 读取 `is_damaged=0` 的 active nodes 覆盖 `cfg.Nodes`，作为运行时节点集合。

3) sing-box 构建/启动：
- Builder 将 `cfg.Nodes` 转换成 sing-box Options（并跳过 DB 里已 damaged 的节点；构建失败的节点会被标记 damaged）。
- BoxManager 启动 sing-box 实例，并等待 monitor 注册完成后启动周期健康检查。

4) 订阅刷新（可选）：
- 周期拉取订阅，upsert 去重到 DB，读 active nodes 形成 merged 集合，写回 nodes.txt，再触发 BoxManager reload（保留端口映射）。

---

## 2. “节点（Node）”的定义与身份（identity）

### 2.1 NodeConfig：配置态节点
节点在配置层的结构为：
- Name：展示名（可从 URI fragment `#...` 自动提取）
- URI：原始节点 URI（vless/vmess/trojan/ss/hysteria2 等）
- Port/Username/Password：主要用于 multi-port/hybrid 下每节点监听端口与认证
- Source：运行时字段（inline / nodes_file / subscription）

重要点：
- `NodeKey()` 的默认实现是 `URI` 字符串本身，用于端口保留（preserve ports across reloads）。

### 2.2 DB 节点主键：host:port
当启用 DB 时，“同一个节点”的去重/统计/损坏标记都基于 `host:port`：
- `HostPortFromURI(raw)` 从 URI 提取 host/port（含 vmess base64-json 特判）。
- 若无法从 URI 提取 host:port：
  - 启动导入与订阅刷新时会直接丢弃该节点（不入库、不参与健康统计、也不会成为运行态主数据源）。
  - 构建阶段（builder.Build）仍可能尝试 build 该节点（如果它仍在 cfg.Nodes），但 DB 相关能力无法覆盖它。

因此，启用 DB 后，“能不能提取 host:port”变成一个硬门槛：决定节点是否能进入 DB 驱动的治理闭环（damaged、24h 成功率阈值、可调度性）。

---

## 3. 三种容易混淆的状态：damaged / blacklist / health(Available)

这是本 fork 最重要的语义澄清之一：它们作用阶段不同、存储位置不同、驱动逻辑不同。

### 3.1 damaged（持久化、构建期剔除）
定义：
- damaged 是 DB 中的持久化标记（`nodes.is_damaged=1`），表示该节点在“构建/重载阶段”就已经被判定为明显错误，不应进入 sing-box outbounds。

关键特性：
- damaged 的作用点是“构建期过滤”：builder.Build 会查询 DB，跳过已 damaged 的节点，避免它进入 sing-box。
- damaged 的产生来源：
  1) Builder 构建单节点 outbound 失败（build-time 校验失败）→ best-effort 标记 damaged（前提：能提取 host:port）。
  2) sing-box 在创建/初始化 outbounds 时出现 `initialize outbound[N]...` 类错误 → BoxManager 解析出 outbound index → 映射到 node URI → 标记 damaged → 快速重试。

结论：
- damaged 用于“把明显错误节点从配置/重载链路中剔除”，并通过持久化避免每次启动/重载都重复踩坑。

### 3.2 blacklist（运行时、内存态、失败阈值拉黑）
定义：
- blacklist 是 pool outbound 的运行时“失败阈值/拉黑”机制，属于内存态共享状态（shared state），用于快速隔离运行时频繁失败的节点。

关键特性：
- 当某个节点在真实连接里失败达到阈值（`failure_threshold`），会进入黑名单一段时间（`blacklist_duration`）。
- blacklist 与 damaged 不同：
  - blacklist 是运行时策略，不会阻止节点进入 sing-box outbounds（它已经在运行中了）。
  - 本 fork 仍保留 nodes 表字段 `blacklisted_until`，但当前主要调度黑名单仍是内存态（代码口径以 pool/shared_state 为准）。

结论：
- blacklist 负责“快速、短期”的失败隔离；damaged 负责“构建期、长期”的硬剔除。

### 3.3 health / Available（可调度性口径，可能由 DB 驱动）
定义：
- Available 是 Monitor 中对节点“是否可调度/可展示为可用”的运行态标志。
- 但启用 DB 后，Available 的写入口径发生变化：它不再直接由 probe 成功/失败决定，而是由 DB 中最近 24h 成功率阈值计算决定。

DB 模式下的规则（非常关键）：
- 健康统计按小时落库到 `node_health_hourly`：
  - probe 的结果写入 hourly 聚合，并更新 nodes 表里 last_* 等展示字段。
  - 真实业务流量产生的 success/failure 事件也会（节流）写入 hourly 聚合，从而让业务反馈影响可调度性。
- 每轮 probe 完成后，或定时（每 5 分钟）触发：
  - 聚合最近 24h 的成功率（success/(success+fail)）
  - 计算所有节点成功率的 p95
  - 阈值 = p95 - 0.05（下调 5%）
  - 只有成功率 >= 阈值的节点，Available=true，否则 false
- 若 DB 启用但最近 24h 没有任何统计行：
  - 所有节点视为不可调度（Available=false，InitialCheckDone=true）
  - 用于避免“没有统计仍沿用旧状态”的隐性风险

结论：
- DB 模式下，Available 是“统计学阈值”的结果，不是“上一轮 probe 是否成功”的结果。

---

## 4. InitialCheckDone：调度门闩（gating）

调度侧（pool outbound）会过滤候选集：
- 只有 `InitialCheckDone=true 且 Available=true` 的节点才会被选入候选集。
- 这保证：
  - 未完成初始检查/阈值收敛的节点不会承载真实流量
  - 调度口径与 WebUI/API 的可用性口径一致

注意：
- 非 DB 模式下：初始 probe 会直接设置 Available 与 InitialCheckDone。
- DB 模式下：Monitor 的 MarkInitialCheckDone/MarkAvailable 会被忽略（以 DB 阈值写入为唯一来源），InitialCheckDone 由 DB 阈值计算阶段统一置为 true。

---

## 5. 探测（probe）语义：目标、协议、状态码

### 5.1 probe_target 的两种写法
`management.probe_target` 支持：
1) `host:port`：
- 被视为 HTTP 探测
- path 默认 `/generate_204`

2) `http(s)://host[:port]/path[?query]`：
- 解析 URL
- `https://` 会启用 TLS 握手（SNI=hostname）
- 未显式端口：http 默认 80，https 默认 443
- path 为空或 `/` 时回退到 `/generate_204`

### 5.2 状态码校验（ExpectedStatuses）
- `probe_expected_statuses` 优先，其次 `probe_expected_status` 单值
- 若未配置任何 expected status：
  - 兼容旧行为：只要能读取 HTTP status line 就算成功（不强制校验 code）
- 若配置了 expected statuses：
  - 必须命中才算 probe 成功

### 5.3 TLS 证书校验
- 全局开关 `skip_cert_verify` 会影响探测：
  - https 探测时映射为 `InsecureSkipVerify`

---

## 6. 运行模式（Mode）与端口语义

### 6.1 pool 模式
- 单入口代理：`listener.address:listener.port`
- 节点池 outbound 负责选择一个可用节点转发

### 6.2 multi-port 模式
- 每个节点一个独立 inbound（端口）：
  - `multi_port.base_port` 起始，节点依次递增分配
- 访问者通过不同端口精确选定节点

### 6.3 hybrid 模式
- 同时拥有：
  - pool 单入口（listener）
  - multi-port 每节点端口（multi_port）
- 共享运行态节点状态（同一节点的 blacklist/health 影响在两个入口一致）

### 6.4 端口保留（Port Preservation）
- Reload / 订阅刷新时，会构造 `portMap (URI -> port)`，并在新配置 normalize 时尽量复用旧端口。
- 端口保留的“身份键”是 `NodeKey()`，当前实现为 `URI` 字符串。
  - 意味着 URI 只要发生字符串层面的变化（即使 host:port 不变），端口也可能无法保留。

---

## 7. 订阅刷新（Subscription Refresh）的真实实现口径

不要把 docs/subscription-refresh-proposal.md 的“设想”当成当前实现；当前实现口径是：
- 启动时会异步触发一次刷新（无需等第一个 interval）。
- 刷新流程：
  1) 拉取订阅，解析成 nodes 列表
  2) 若 DB 启用：
     - upsert 入库（按 host:port 去重；无法提取 host:port 的丢弃）
     - 从 DB 读 active nodes 作为 merged
  3) 若 DB 未启用或 DB 结果为空：merged=订阅拉取结果
  4) 写 merged 到 nodes.txt（作为缓存 + 下次启动数据源）
  5) 触发 BoxManager reload，并传入 portMap 以保留端口
- nodes.txt 修改检测：
  - 通过 modTime 快路径 + hash 慢路径识别是否被外部编辑，从而在 UI 提示“NodesModified”。

---

## 8. Monitor（监控管理器）与 BoxManager（实例管理器）的时序关系

### 8.1 为什么需要 “等待 monitor 注册”
sing-box 启动并不保证我们的 pool outbound/monitor 注册立即可见：
- 如果过早开始 probe/统计，会出现短时间 “0 nodes / 0/0” 假象，影响 UI 与调度。

当前做法：
- BoxManager 在 Start/Reload 后，会等待 monitor `Snapshot()` 的节点数量 > 0 或超时告警，再启动周期健康检查。
- 同时，在观察到注册后立即调用 `RefreshHealthFromDB()`，让 DB 阈值口径尽快落到运行态 Available/InitialCheckDone。

### 8.2 Reload 时为什么要 ResetRuntime
Reload 期间如果 monitor 还在跑旧实例的 probe：
- 旧 outbound 被关闭后会产生大量失败
- UI 抖动
- DB 统计被污染

当前做法：
- Reload 开始前：`monitorMgr.ResetRuntime()` 取消 in-flight probes/periodic loop，并清空 nodes map。
- 新实例启动后：等待重新注册，再启动 periodic health check，并立即刷新 DB 阈值。

---

## 9. 概念对照表（速查）

- NodeConfig：配置态节点（Name/URI/Port/Source），可来自 config inline、nodes_file、subscription
- host:port：DB 去重与统计的唯一键（无法提取则失去 DB 驱动能力）
- damaged：DB 持久化硬剔除；构建期/重载期产生；builder 会跳过
- blacklist：运行时内存态软隔离；失败阈值触发；时间到自动释放
- Available：可调度性标志；DB 模式下由 24h 成功率阈值（p95-5%）唯一写入
- InitialCheckDone：调度门闩；必须为 true 且 Available 为 true 才进入候选集
- probe_target：健康探测目标，支持 host:port 或 http(s)://...，可配置 expected status 校验
- port preservation：依赖 NodeKey（当前为 URI 字符串）进行端口复用

---

## 10. 常见误解与正确结论

1) “probe 成功就 Available=true”
- 错：DB 模式下 probe 只写统计，Available 由 DB 阈值统一决定。

2) “blacklist 会持久化到 DB”
- 当前主要不是：调度拉黑仍以内存共享状态为主；DB 里虽有字段但不是当前核心口径。

3) “订阅刷新不会中断连接（优雅切换）”
- 设想文档里提到过，但当前实现是关闭旧实例再启动新实例（multi-port 必须释放端口），连接会短暂中断。

4) “节点唯一性是 URI”
- 配置层端口保留键是 URI；DB 层唯一性是 host:port。两者不一致时会出现“统计聚合在一起但端口不复用”的现象。

---

## 11. 追溯实现位置（文件索引）

- 配置结构/默认值/节点来源与 nodes.txt 保存：internal/config/config.go
- 启动时 DB 导入与 DB 作为主数据源：internal/app/app.go
- DB schema / host:port 提取 / hourly 健康统计：internal/database/db.go
- build 阶段校验、跳过 damaged、expected statuses 下发：internal/builder/builder.go
- sing-box 生命周期、init outbound 错误映射 damaged、Reload 时序与 monitor reset：internal/boxmgr/manager.go
- probe_target 解析、DB 阈值（p95-5%）计算、ResetRuntime：internal/monitor/manager.go
- pool 调度过滤（InitialCheckDone + Available）、probe HTTP/HTTPS 与状态码校验：internal/outbound/pool/pool.go
- 订阅刷新 loop、DB upsert 去重、写 nodes.txt、Reload with portMap：internal/subscription/manager.go