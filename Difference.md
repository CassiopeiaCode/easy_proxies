# 与上游项目的差异

本文档记录了本 fork 与上游 [easy_proxies](https://github.com/jasonwong1991/easy_proxies) 项目的所有改动和新增功能。

## 新增需求 / 已实现差异

### 1. 节点状态持久化：SQLite → Pebble（已实现：Store 抽象 + Pebble 实现）

历史实现曾使用 SQLite 单文件数据库存储 nodes 表（host:port 唯一约束）以及健康统计。

当前实现已**完全迁移为 Pebble**（目录型 KV 存储）：
- Store 接口：[`internal/store/store.go`](internal/store/store.go:1)
- Pebble 实现：[`internal/store/pebble`](internal/store/pebble/pebble.go:1)

说明：持久化层主要用于“导入/重载时的 damaged 标记与过滤”“健康统计的 hourly 聚合与阈值调度”等能力；运行时调度仍以 monitor 的 `Available/InitialCheckDone` 过滤为准（见下文）。

### 2. 构建期坏节点自动标记 damaged，并且不载入 sing-box（已实现）

目标：坏节点不应进入 sing-box 配置；damaged 的变动应伴随 sing-box 重载/构建阶段发生（而不是运行时失败不断修改 damaged）。

当前实现行为：
- 构建期（sing-box build/reload）对每个节点先做 outbound 构建校验；构建失败则该节点不会进入 sing-box outbounds。
- 若启用 store（`store.dir` 非空；兼容旧配置的 `database.path` 推导），构建失败的节点会 best-effort 标记为 damaged，并写入 reason（仅当能从 URI 提取 host:port 时）。
- 若某节点在 store 中已经 damaged，则构建期会直接跳过该节点（不会载入 sing-box）。
- 若 sing-box 在启动/重载时出现 `initialize outbound[N]` 这类初始化错误，会 best-effort 标记对应节点 damaged 并快速重试；重试超过 5 次才退出。

涉及模块：
- internal/builder/builder.go：构建期校验、写入 damaged、跳过 damaged 节点。
- internal/boxmgr/manager.go：sing-box init outbound 错误映射 damaged + 启动/重载重试。
- internal/store/*：持久化与 URI 解析（host:port 提取、damaged 标记、hourly 健康统计）。

注意：host:port 主键现在支持从 `vmess://<base64(json)>` 解析（提取 json 的 `add` + `port`），因此这类节点也可以去重入库、标记 damaged、参与健康统计与阈值调度。若仍有极端 URI 无法提取 host:port，则该节点会被直接过滤掉（不入库、不载入、不参与调度）。

### 3. 启动时导入 + 异步订阅去重（已实现：store 主数据源 + nodes.txt 兼容）

目标：启动优先从 store 加载可用节点快速启动；订阅/文件导入按 host:port 去重入库，入库成功触发重载；无法提取 host:port 的节点直接过滤掉（不入库、不载入、不参与调度）。

当前实现行为：
- `nodes.txt` 始终作为数据源：配置加载阶段总会读取 `nodes_file`（默认 `nodes.txt`）；即使存在订阅，也不会忽略本地 nodes.txt。
  - 支持 `socks://` / `socks5://` / `socks4://` / `socks4a://` / `http://` / `https://` 节点 URI。
  - 兼容 “`URI 备注...`” 行格式：会取每行第一个 token 作为 URI，其余内容忽略（便于保留地区/来源描述）。
- 启动阶段 store 导入 + store 加载：
  - 若启用 store，则会 best-effort 将当前 `cfg.Nodes`（包含 inline + nodes.txt）按 host:port upsert 写入 store；
  - 随后从 store 加载 active nodes（非 damaged）作为运行时节点列表（store 成为主数据源）。
- 订阅刷新异步去重入库：
  - SubscriptionManager 拉取订阅后先按 host:port upsert 入库去重；
  - 再从 store 读取 active nodes 组装成新的运行时 nodes 并触发 reload；
  - 订阅内容解析与启动加载阶段统一：同一套完整解析逻辑（Base64 / Clash YAML / 纯文本逐行 URI）。
  - 注意：本 fork 中 `nodes.txt` 为只读，订阅刷新不再写回 `nodes.txt`。

涉及模块：
- internal/app/app.go：启动时 upsert 导入 + 从 store 加载 active nodes。
- internal/config/config.go：订阅解析完整实现（Base64 / Clash YAML / 纯文本逐行 URI）。
- internal/subscription/manager.go：订阅刷新复用 config 的订阅解析 + upsert 去重入库 + 从 store 读 active nodes reload（不写 nodes.txt）。

### 4. 健康检查统计落库 + 24h 成功率阈值调度（已实现）

目标：健康检查结果需要持久化，并用最近 24h 的成功率分布做“健康可调度”过滤：计算所有节点成功率的 p95，再取阈值 = p95 - 5%，只有成功率 >= (p95 - 5%) 的节点才可被调度。

当前实现行为：
- 健康检查结果按“小时”聚合落库：使用 store 的 hourly 记录（逻辑等价于旧版 `node_health_hourly` 表），按 UTC 小时累计 `success_count/fail_count/latency_sum_ms/latency_count`。
- 周期性健康检查（monitor 侧 probe）：
  - 每轮健康检查会随机打散节点探测顺序，避免固定顺序导致探测偏置与尾部节点长期延后。
  - 若新一轮健康检查到期启动时上一轮仍未结束，则强制取消上一轮并立即启动新一轮；已完成的节点探测结果不会丢失（逐节点写入内存状态与 store hourly 聚合是边跑边落的）。
  - 写入 store 的 hourly 统计，并同步更新节点的 last_* 与累计计数（用于展示/快速查询）。
  - store 启用时，不再由 probe 结果直接写入 `Available`，只更新 last_* 观测字段；`Available` 统一由近 24h 阈值计算产出。
- 真实连接（业务流量）健康事件入库（store 启用时）：
  - `RecordFailure/RecordSuccess/RecordSuccessWithLatency` 会（节流）写入 hourly 统计，使真实连接的成功/失败也能影响 24h 成功率。
  - 写入后会触发（节流的）阈值重算，使运行态 `Available` 尽快收敛到最新统计口径。
- 统计窗口为最近 24h：从 hourly 聚合出每个节点的成功率（success / (success+fail)）。
- 计算 p95 成功率并下调 5% 形成阈值：threshold = p95 - 0.05。
- store 启用时，`Available` 的唯一写入来源：p95-5% 阈值计算（无其它路径可覆写）。
  - 若 store 启用但近 24h 没有任何统计行，则所有节点视为不可调度（Available=false），避免沿用历史脏状态。
- pool 调度过滤：仅允许 `InitialCheckDone=true 且 Available=true` 的节点进入候选集；`InitialCheckDone=false`（尚未完成初始检查/尚未应用阈值）也不会被调度，避免未检查节点承载真实流量。
- 全量重算：store 启用时，每 5 分钟强制全量重算全部节点的 `Available`（即使 probe 间隔更长或真实流量触发被节流）。
- 自动清理：hourly 统计自动删除 7 天前的历史聚合数据。

涉及模块：
- internal/store/pebble/*：hourly 聚合写入、近 24h 成功率查询、历史数据清理。
- internal/monitor/manager.go：周期性 probe 结果写入 store；真实连接健康事件写入 store；按 24h 成功率计算 p95-5% 阈值并更新节点 Available；每 5 分钟全量重算；定时清理 7 天前统计数据。
- internal/outbound/pool/pool.go：调度时读取 monitor 的 Available/InitialCheckDone 进行过滤；新增“连接建立后 10 秒内累计未收到 1KB 数据则记一次失败（不影响连接）”的软失败逻辑。

注意：
- 成功率统计与阈值过滤依赖从 URI 提取 host:port；若某类 URI 无法提取 host:port，则不会参与 hourly 统计与阈值过滤。

### 5. 探测目标 URL/HTTPS + 自定义状态码验证（已实现）

目标：`management.probe_target` 支持直接填 URL（含 https），并且可通过 `probe_expected_status` / `probe_expected_statuses` 对返回 HTTP 状态码做校验；只有状态码匹配才算探测成功。

当前实现行为：
- `management.probe_target` 支持两类写法：
  - `host:port`：按 HTTP 探测，默认 path 为 `/generate_204`。
  - `http(s)://host[:port]/path[?query]`：按 URL 解析；`https://` 会启用 TLS；未显式写端口时 http 默认 80、https 默认 443；path 为空或 `/` 时回退到 `/generate_204`。
- 探测逻辑支持 HTTP + HTTPS：
  - 当 `probe_target` 为 https 时，会先进行 TLS 握手（SNI=hostname）；证书校验遵循全局 `skip_cert_verify`（跳过校验时会启用 `InsecureSkipVerify`）。
  - 发送 `GET <path> HTTP/1.1` 并读取 status line，用于计算 TTFB（探测耗时 = dial + probe）。
  - HTTP 请求头至少包含 `Host`，并补齐常见头（如 `Accept`/`Accept-Encoding`）以避免部分站点拦截过于精简的探测请求。
- 状态码校验：
  - `probe_expected_statuses` 优先；否则使用单值 `probe_expected_status`。
  - 若未配置期望状态码，则保持兼容：只要能成功读到响应 status line 即视为成功（不强制校验 code）。
- 超时：
  - 探测严格受 `context` 超时控制；当上层设置了 `context.WithTimeout` 时，TLS 握手/写入/读取会绑定到同一个 deadline，避免单次健康检查卡住超过预期。

涉及模块：
- internal/monitor/manager.go：probe_target URL 解析与 https 探测参数下发。
- internal/outbound/pool/pool.go：HTTP/HTTPS 探测实现（含状态码校验与 TLS 握手）。
- internal/builder/builder.go：把期望状态码配置下发到 pool outbound。
- internal/monitor/manager_test.go：probe_target 解析行为单测覆盖（https/http/host:port 等）。

### 6. 日志节流（已实现）

目标：按“调用点（go 文件 + 行号）”进行节流，同一个日志调用点每秒最多输出 10 条，避免错误风暴刷屏并影响性能。

已实现

### 7. 启动/重载时的监控注册与健康检查时序修复（已实现）

背景：节点管理页（配置态）能看到节点，但监控页（运行态）可能显示“暂无调试数据 / 0 个节点”，常见原因是 sing-box 启动成功后，monitor 的节点注册回调还未完成就开始统计/健康检查，导致短时间内 Snapshot 为空（0/0 假象）。

当前实现行为：
- 启动 sing-box 成功后，会等待 monitor 观察到至少 1 个节点注册（或超时告警），再启动 periodic health check 与 initial health check 等逻辑，避免 0/0 假象与空列表。
- 等待逻辑：默认等待 30 秒（大量节点场景下更稳）。
- 在观察到节点注册后，会立即将 store 的近 24h 健康阈值应用到运行态节点（不必等待下一轮 periodic probe），用于尽快收敛 `Available/InitialCheckDone` 语义与调度/展示口径。

涉及模块：
- internal/boxmgr/manager.go：waitForMonitorRegistration + 启动/重载流程里的调用。

### 8. 重载时取消正在进行的健康检查，避免旧实例探测污染（已实现）

背景：健康检查进行中触发重载时，旧 sing-box instance 会被关闭，但 monitor 的 probe goroutine 可能仍在跑；探测继续使用旧 outbound 的 probe 会造成大量失败、UI 抖动、健康统计被污染。

当前实现行为：
- 重载开始时，先取消 monitor 的运行时 context（终止 in-flight probes/periodic loop），并清空运行时节点注册表（nodes map）；等待新实例启动后重新注册节点。
- 重载成功后，同样等待 monitor 重新注册，再启动 periodic health check。
- 在观察到节点重新注册后，会立即将 store 的近 24h 健康阈值应用到运行态节点（不必等待下一轮 periodic probe），用于尽快收敛 `Available/InitialCheckDone` 语义与调度/展示口径。

涉及模块：
- internal/monitor/manager.go：新增 ResetRuntime（保留 DB/Config，仅重置运行态）。
- internal/boxmgr/manager.go：Reload 中调用 ResetRuntime，重载后重启 periodic health check。

### 9. nodes.txt 只读 + 强制启用数据库（已实现）

目标：
- `nodes.txt` 只作为“可读的数据源/导入来源”，进程不再写入该文件（避免 WebUI/订阅刷新覆盖人工内容）。
- 强制启用 SQLite DB：节点的增删改/订阅合并等持久化全部走 DB。

当前实现行为：
- 启动脚本将 `nodes.txt` 设置为只读（`chmod 444`），不再尝试给 `nodes.txt` `chmod 666`。
- 配置加载阶段会将持久化目录规范化为 `store.dir`：
  - 推荐显式配置 `store.dir`；
  - 兼容旧配置 `database.path`：若为 `*.db` 文件路径会推导为同路径 `*.pebble` 目录。
- WebUI 的节点 CRUD 保存不再写 `nodes.txt`：`SaveNodes()` 会把节点 upsert 到 store（Pebble）。
- 订阅刷新不再写回 `nodes.txt`（不再作为 cache），仅通过 store 合并/去重后触发 reload。

涉及模块：
- start.sh：调整文件权限策略（config.yaml 可写、nodes.txt 只读）。
- internal/config/config.go：normalize store 配置兼容推导；SaveNodes 写 store 而不是写 nodes.txt。
- internal/subscription/manager.go：订阅刷新流程移除写 nodes.txt 的步骤。

### 10. 新增 Sticky 粘性代理入口（已实现）

目标：
- 新增一个独立端口的粘性代理入口（可配置地址/端口/认证）。
- 同一时间窗口内固定使用一个出口节点，到达 `sticky.switch_interval` 后再轮换。
- 若当前出口在窗口内提前变为不健康，则立即切换，不等待窗口到期。
- 节点健康判断继续沿用现有规则，并且真实连接事件（成功/失败）继续计入 health 统计。
- 切换策略使用长度为 100（可配置）的 deque：
  - 优先选择“健康且不在 deque 中”的节点；
  - 若健康节点都已在 deque 中，则选择 deque 中“最久未使用”的健康节点；
  - 每次选中节点都会记录到 deque（去重后追加到队尾）。

当前实现行为：
- 配置新增 `sticky` 段（`enabled/address/port/username/password/switch_interval/history_size`），默认 `history_size=100`。
- 在 `builder` 中新增独立入站 `http-sticky-in`，绑定 `sticky.port`，路由到独立的 `pool` outbound（`Mode=sticky`）。
- `pool` outbound 新增 `sticky` 调度模式与历史 deque 策略：
  - 窗口内优先复用当前 sticky 出口；
  - 当前出口不健康或窗口到期时，按 deque 规则重选；
  - 仍受现有健康过滤约束（`InitialCheckDone && Available`）。
- Docker 相关暴露与 compose 注释同步补充 sticky 端口（默认 2324）。

涉及模块：
- internal/config/config.go：sticky 配置结构、默认值、端口冲突规避。
- internal/builder/builder.go：sticky inbound/outbound 构建与路由、启动日志输出 sticky 入口。
- internal/outbound/pool/pool.go：sticky 调度实现、deque 选择逻辑。
- config.example.yaml：sticky 示例配置。
- docker-compose.yml、Dockerfile：sticky 端口说明与暴露。

### 11. 订阅刷新 grep 统计日志 + 订阅解析兼容增强（已实现）

目标：
- 刷新订阅时输出便于 `grep` 的固定键值日志，至少包含：
  - 订阅 URL
  - 拉取内容大小（bytes）
  - 解析得到的节点数
  - 解析失败/跳过的节点数
- 提升“有内容但解析失败”的订阅兼容性，减少因上游格式不规范导致整份丢弃。

当前实现行为：
- 订阅刷新新增固定格式日志（每个订阅 URL 一行）：
  - 成功：`sub_refresh_result url="..." size_bytes=... parsed_nodes=... error_nodes=...`
  - 失败：`sub_refresh_result url="..." size_bytes=... parsed_nodes=0 error_nodes=0 err="..."`
- `subscription` 与 `config` 之间新增解析统计通道：
  - `ParseSubscriptionContentWithStats(...)` 返回 `ParsedNodes/ErrorNodes`；
  - 原有 `ParseSubscriptionContent(...)` 保持兼容，内部复用新实现。
- PlainText 解析增强：
  - 保持原有“每行 URI / URI + 备注”兼容；
  - 新增对嵌入式链接提取（如 `Link: ss://...`），可识别并提取 URI。
- Clash YAML 解析增强：
  - `port` / `alterId` 支持数字字符串（如 `"443"`、`"64"`），避免因类型严格导致整份 YAML 解析失败。
- 本地分析临时目录策略：
  - `.gitignore` 新增 `.tmp/`，用于订阅原文排查等临时文件，避免误提交。

涉及模块：
- `internal/subscription/manager.go`：订阅拉取结果日志、`size_bytes/error_nodes` 统计输出。
- `internal/config/config.go`：`ParseSubscriptionContentWithStats`、PlainText URI 提取增强、Clash `intOrString` 兼容解析。
- `.gitignore`：新增 `.tmp/`。

## 关键语义：damaged vs health

- damaged：持久化状态（DB），用于“导入/重载阶段”剔除明显错误/不应被加载的节点；damaged 的变动应伴随 sing-box 重载/构建发生。
- health（健康度）：基于最近 24h 成功率（p95-5% 阈值）判断节点是否“可调度”。
  - store 启用时，`Available` 只由阈值计算写入（p95-5%）；启动初检/手动标记/probe 即时结果不会覆写可调度性口径。
  - 运行时失败不仅来源于周期性 probe，也包括真实连接的成功/失败事件（节流入库），从而让业务流量能反馈到健康统计与可调度性。

## 本次会话新增差异（2026-02）

### 12. 重建硬超时 + 超时累计退出（已实现）

目标：避免 sing-box create/start 在单次重建中长时间卡死。

当前实现行为：
- 启动/重载每次 create+start 都套硬超时（默认 30s）。
- 若连续超时达到 10 次，进程直接 `exit 1`，避免无限卡住。
- 同时修复“重试全部失败仍继续走成功路径”的隐患。

涉及模块：
- `internal/boxmgr/manager.go`

### 13. 全进程共享 Store 实例（已实现）

目标：消除同进程重复 `pebblestore.Open` 导致的锁冲突（`lock held by current process`）。

当前实现行为：
- 在 `app.Run` 中只打开一次 Pebble Store，并通过依赖注入传给 `boxmgr/monitor/subscription/builder`。
- 相关模块不再自行 `Open/Close` 同一路径 Pebble。
- `monitor.Stop()` 不再关闭共享 Store（由 app 生命周期统一关闭）。
- `Config.Save()` 不再自行打开 Pebble 写节点，节点持久化统一由运行时组件负责。

涉及模块：
- `internal/app/app.go`
- `internal/boxmgr/manager.go`
- `internal/monitor/manager.go`
- `internal/subscription/manager.go`
- `internal/builder/builder.go`
- `internal/config/config.go`

### 14. 订阅刷新强门控：写/读 Store 成功才允许 reload（已实现）

目标：避免“Store 异常时仍用抓取结果重载 sing-box”。

当前实现行为：
- Store 不可用：本轮刷新直接失败并 `skip reload`。
- Store 写入失败：直接失败并 `skip reload`。
- Store 读取 active 失败：直接失败并 `skip reload`。
- 读取 active 为 0：直接 `skip reload`。
- 不再回退到“用 fetched nodes 直接重载”。

涉及模块：
- `internal/subscription/manager.go`

### 15. 订阅写入改为 Pebble Batch（已实现）

目标：降低大规模节点 upsert 的 fsync 开销，减少 `context deadline exceeded`。

当前实现行为：
- 新增 Store 批量接口：`UpsertNodesByHostPortBatch`。
- Pebble 实现使用 `Batch` 聚合写入后一次 `Commit(Sync)`。
- 订阅刷新改为批量 upsert，而非逐条 `Set(Sync)`。

涉及模块：
- `internal/store/store.go`
- `internal/store/pebble/nodes.go`
- `internal/subscription/manager.go`

### 16. 订阅读取 active 超时与耗时日志（已实现）

目标：大库扫描时避免过短超时导致误判失败，并提供诊断信息。

当前实现行为：
- 订阅写入超时常量化（60s）。
- 订阅读取 active 超时提升（120s）。
- 增加读取耗时日志（读到多少节点、耗时多久）。

涉及模块：
- `internal/subscription/manager.go`

### 17. Reality `public_key` 前置校验（已实现）

目标：在 builder 阶段提前发现无效 `public_key`，而不是等到 sing-box init 报错。

当前实现行为：
- 当 `security=reality` 时，对 `pbk` 做前置校验：
  - 支持常见 base64 变体（含 URL-safe）。
  - 解码后长度必须为 32 字节（X25519 公钥长度）。
- 校验失败会在构建阶段直接判无效，走现有 damaged 标记与跳过路径。

涉及模块：
- `internal/builder/builder.go`

### 18. 重试策略与日志精简（已实现）

目标：减少“未收敛就提前失败”和构建日志刷屏。

当前实现行为：
- 启动/重载重试上限改为与 damaged 收敛联动（`maxRetries = maxDamagedRetries + 10`）。
- 构建阶段去掉逐节点 `marked damaged ... skipping build` 噪声日志。
- 失败日志改为汇总：总失败数、damaged/invalid 计数、少量示例。

涉及模块：
- `internal/boxmgr/manager.go`
- `internal/builder/builder.go`

### 19. damaged 启发式联动标记（已实现）

目标：某节点确认 damaged 后，快速屏蔽“除 name/host/ip 外其余特征一致”的同类节点。

当前实现行为：
- 在 `MarkNodeDamaged` 后执行启发式传播：
  - 计算 URI 签名（忽略 name/host/ip 相关字段）。
  - 扫描节点并批量标记签名相同节点为 damaged。
  - 使用 Batch 提交。
- 用于加速同模板垃圾节点收敛，降低后续反复初始化失败。

涉及模块：
- `internal/store/pebble/nodes.go`
