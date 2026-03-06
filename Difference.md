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

目标：启动优先从 store 加载可用节点快速启动；订阅/文件导入按 host:port 去重入库；无法提取 host:port 的节点直接过滤掉（不入库、不载入、不参与调度）。

当前实现行为：
- `nodes.txt` 始终作为数据源：配置加载阶段总会读取 `nodes_file`（默认 `nodes.txt`）；即使存在订阅，也不会忽略本地 nodes.txt。
  - 支持 `socks://` / `socks5://` / `socks4://` / `socks4a://` / `http://` / `https://` 节点 URI。
  - 兼容 “`URI 备注...`” 行格式：会取每行第一个 token 作为 URI，其余内容忽略（便于保留地区/来源描述）。
- 启动阶段 store 导入 + store 加载：
  - 若启用 store，则会 best-effort 将当前 `cfg.Nodes`（包含 inline + nodes.txt）按 host:port upsert 写入 store；
  - 随后从 store 加载 active nodes（非 damaged）作为运行时节点列表（store 成为主数据源）。
- 订阅刷新异步去重入库：
  - SubscriptionManager 拉取订阅后先按 host:port upsert 入库去重（仅插入缺失节点，已有记录不重写）；
  - 刷新流程不再触发 sing-box reload，现网运行态由现有实例保持；
  - 订阅内容解析与启动加载阶段统一：同一套完整解析逻辑（Base64 / Clash YAML / 纯文本逐行 URI）。
  - 注意：本 fork 中 `nodes.txt` 为只读，订阅刷新不再写回 `nodes.txt`。

涉及模块：
- internal/app/app.go：启动时 upsert 导入 + 从 store 加载 active nodes。
- internal/config/config.go：订阅解析完整实现（Base64 / Clash YAML / 纯文本逐行 URI）。
- internal/subscription/manager.go：订阅刷新复用 config 的订阅解析 + upsert 去重入库（仅新增，不写 nodes.txt，不触发 reload）。

### 4. 健康检查统计落库 + 24h 成功率阈值调度（已实现）

目标：健康检查结果需要持久化，并用最近 24h 的成功率分布做“健康可调度”过滤：计算成功率非 0 节点的 p95，再取阈值 = p95 - 5%，只有成功率 >= (p95 - 5%) 的节点才可被调度。

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
  - p95 输入集合仅包含成功率 `> 0` 的节点（不包含成功率为 0 的节点）。
  - 若非 0 成功率集合为空，则 p95 按 0 处理，再走阈值夹紧（`[0,1]`）。
- store 启用时，`Available` 的唯一写入来源：p95-5% 阈值计算（无其它路径可覆写）。
  - 若 store 启用但近 24h 没有任何统计行，则所有节点视为不可调度（Available=false），避免沿用历史脏状态。
- pool 调度过滤：仅允许 `InitialCheckDone=true 且 Available=true` 的节点进入候选集；`InitialCheckDone=false`（尚未完成初始检查/尚未应用阈值）也不会被调度，避免未检查节点承载真实流量。
- sing-box 加载节点上限与分层选取：
  - 在 `createBox` 前会基于 store 最近 24h 统计按同一口径临时重算“健康节点集合”（p95-5%，且 p95 仅统计成功率 `>0`）。
  - 当候选节点超过 5000 时，最多加载 5000：优先 90% 健康节点，其余 10% 从其它节点补齐；任一类不足时由另一类补足。
  - 该筛选仅影响 sing-box 实际加载节点（从而影响运行时健康检查与入站请求路由范围），不影响订阅刷新入库。
- 全量重算：store 启用时，每 5 分钟强制全量重算全部节点的 `Available`（即使 probe 间隔更长或真实流量触发被节流）。
  - 性能优化：24h 统计查询增加短 TTL 缓存（默认 5s），`Attach24hStats/Compute24hHealthMap/applyHealthThresholdFromDB` 复用同一份统计快照，减少重复扫描 Pebble。
  - 性能优化：重算入口增加并发单飞，避免多个 goroutine 同时执行全量阈值重算。
  - 性能优化：阈值应用阶段由线性 `lookupRate` 改为 map 查找，降低大规模节点场景的 CPU 开销。
  - 性能优化：`p95-5%` 阈值缓存调整为 30s；缓存过期后前台请求会立即返回旧缓存，同时后台异步触发单飞重算；仅在冷启动（缓存尚未初始化）时阻塞等待一次重算完成。
  - 性能优化：健康检查写入不再每次强制清空 24h 阈值缓存，避免高频 probe 导致缓存抖动与频繁全量重算。
- 自动清理：hourly 统计自动删除 7 天前的历史聚合数据。

涉及模块：
- internal/store/pebble/*：hourly 聚合写入、近 24h 成功率查询、历史数据清理。
- internal/monitor/manager.go：周期性 probe 结果写入 store；真实连接健康事件写入 store；按 24h 成功率计算 p95-5% 阈值并更新节点 Available；每 5 分钟全量重算；定时清理 7 天前统计数据。
- internal/outbound/pool/pool.go：调度时读取 monitor 的 Available/InitialCheckDone 进行过滤；新增“连接建立后 10 秒内累计未收到 1KB 数据则记一次失败（不影响连接）”的软失败逻辑。
- internal/boxmgr/manager.go：createBox 前按 24h 口径选取最多 5000 节点（90% 健康 + 10% 其它）用于 sing-box 实际加载。

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
- 重载切换改为“两阶段”：
  - 阶段 1：旧实例继续运行时先执行 `createBox`（含构建/初始化重试与超时控制）；
  - 阶段 2：仅在准备好新实例后才关闭旧实例并启动新实例。
  - 若阶段 1 失败，旧实例不会被中断；若阶段 2 失败，会回滚拉起旧配置实例，避免服务长时间不可用。

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

### 14. 订阅刷新强门控：仅写 Store，不触发 reload（已实现）

目标：避免订阅刷新过程影响现网 sing-box 运行态；只做持久化增量入库。

当前实现行为：
- Store 不可用：本轮刷新直接失败，且不触发 reload。
- Store 写入失败：直接失败，且不触发 reload。
- 订阅刷新流程只负责入库，不再读取 active nodes 组装运行时配置。
- 不再回退到“用 fetched nodes 直接重载”。

涉及模块：
- `internal/subscription/manager.go`

### 15. 订阅写入改为 Pebble Batch（已实现）

目标：降低大规模节点 upsert 的 fsync 开销，减少 `context deadline exceeded`。

当前实现行为：
- 新增 Store 批量接口：`UpsertNodesByHostPortBatch`。
- 订阅刷新写入改为“按批次调用批量 upsert”（默认每批 2000），每批独立超时控制。
- Pebble 批量写入按 `host:port` 去重键先扫描已有 key：已存在节点直接跳过，不重写。
- 仅对“库中不存在”的节点执行批量插入，写入阶段使用分块 `Batch Commit(Sync)`。
- 若某批批量写入失败，会降级为该批逐条写入（逐条也独立超时），避免整轮失败。
- 写入前会先过滤无法解析 host:port 的非法 URI，仅丢弃坏节点并继续。

涉及模块：
- `internal/store/store.go`
- `internal/store/pebble/nodes.go`
- `internal/subscription/manager.go`

### 16. 订阅读取 active 超时与耗时日志（已实现）

目标：大库扫描时避免过短超时导致误判失败，并提供诊断信息。

当前实现行为：
- 订阅写入超时常量化（10m）。
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

### 20. 可配置 pprof 调试服务（已实现）

目标：增加可选的运行时性能分析入口，便于线上排查 CPU/内存/阻塞问题。

当前实现行为：
- 新增 `pprof` 配置段：
  - `pprof.enabled`：是否启用
  - `pprof.host`：监听地址（默认 `127.0.0.1`）
  - `pprof.port`：监听端口（默认 `6060`）
- 启用后在独立 HTTP server 暴露标准 Go pprof 路由：
  - `/debug/pprof/`
  - `/debug/pprof/profile`
  - `/debug/pprof/trace`
  - 等标准端点
- 进程退出时会随主 context 做优雅关闭。

涉及模块：
- `internal/config/config.go`
- `internal/app/app.go`
- `config.example.yaml`

### 21. 节点监控 Tab 展示成功率与成功/总次数（已实现）

目标：在“节点监控”页直接看到每个节点的成功率，以及成功次数/总次数。

当前实现行为：
- 后端 `Snapshot` 新增字段：
  - `total_count`：`success_count + failure_count`
  - `success_rate`：`success_count / total_count * 100`（百分比）
  - `db_24h_success_count` / `db_24h_failure_count` / `db_24h_total_count`
  - `db_24h_success_rate` / `db_24h_failure_rate`
- `/api/nodes` 返回的节点数据包含上述字段，前端可直接消费。
- 前端“节点监控”卡片新增：
  - 成功率（24h）
  - 失败率（24h）
  - 24h 统计总调用次数
- 前端对旧数据保留兼容：若后端未返回 `db_24h_*` 字段，会回退到运行时计数口径计算。
- 节点监控页的数据源调整：
  - `/api/nodes` 的节点主集合改为 **可调度节点** 的 monitor 运行时快照（即 sing-box 实际加载并注册到运行时且满足 `initial_check_done && available` 的节点），不再返回全量数据库节点，也不再遍历 boxmgr 配置节点做主集合。
  - 因 sing-box 侧存在最多 5000 节点加载上限，`/api/nodes` 的返回规模随运行时节点集合变化，通常不超过该上限。
  - `/api/nodes/probe-all` 的待探测列表也改为 boxmgr 配置节点；未加载到运行时的节点会返回不可探测错误项。

涉及模块：
- `internal/monitor/manager.go`
- `internal/monitor/server.go`
- `internal/monitor/assets/index.html`

### 22. 修复 periodic health check 在 ResetRuntime/Reload 后 goroutine 泄漏（已实现）

问题现象：
- 多次 Reload 后，monitor 的周期性健康检查 goroutine 未按预期退出，导致同一时刻存在多个健康检查循环。
- 多个循环会互相触发 “新一轮取消旧一轮”，产生大量 `context cancelled` 的 probe 结果与脏数据，进一步影响可调度性判断。

根因：
- `ResetRuntime()` 会替换 `m.ctx` 为一个新的 context；而周期循环使用 `select { case <-m.ctx.Done(): ... }` 动态读取 `m.ctx`，可能错过旧 ctx 的取消，从而泄漏旧循环。

修复后行为：
- 周期健康检查循环在启动时捕获 `loopCtx`（当时的 `m.ctx`），后续只监听该 ctx 的 Done，从而保证 `ResetRuntime()` 取消旧 ctx 时能可靠退出旧循环。

涉及模块：
- `internal/monitor/manager.go`

### 23. 修复 sing-box 实例 runtime context 被 create 超时取消，导致真实流量拨号被取消（已实现）

问题现象：
- 客户端一入站即出现大量 `dial tcp ...: operation was canceled`，表现为 CONNECT 先返回 `200 Connection established`，随后 TLS/数据流立刻被 RST/中断。

根因：
- `createWithTimeout()` 使用带超时的 `attemptCtx` 作为 `box.Context(...)` 的基础 context；函数返回时会 `cancel()`，间接把实例 runtime context 一并取消。
- 结果是：实例启动后 `ctx` 已处于 cancelled 状态，所有下游 `DialContext` 都会被取消。

修复后行为：
- 分离“创建阶段超时控制 ctx（attemptCtx）”与“实例生命周期 ctx（instanceCtx）”：
  - `attemptCtx` 仅用于限制 create/build 时长；
  - `instanceCtx` 随实例存活，并在 Reload/Close/rollback 时显式 cancel。

涉及模块：
- `internal/boxmgr/manager.go`

## 本次会话新增差异（2026-03）

### 24. 调度健康语义收敛 + HTTP(S) 节点 URI 强制显式端口（已实现）

目标：
- 统一“可调度（schedulable）”口径：运行态调度仅依据 DB 近 24h 成功率阈值（p95(非0)-5%）推导的 `Available/InitialCheckDone`。
- 消除 `http://host` / `https://host` 这类未显式端口的节点在“实际连接端口 / host:port 去重键 / 健康统计键”之间产生隐性漂移。

当前实现行为：
- 调度 gating 统一使用 `InitialCheckDone && Available`（DB 模式下 `MarkInitialCheckDone/MarkAvailable` 外部写入会被忽略）。
- 对节点 URI 的 `http(s)://`：必须显式写端口，否则视为非法节点（不入库、不参与健康统计、构建阶段会被跳过）。
- `pool.failure_threshold` / `pool.blacklist_duration` 标记为已废弃（不再影响调度），避免配置含义误导。

涉及模块：
- `internal/store/uri.go`
- `internal/builder/builder.go`
- `internal/outbound/pool/pool.go`
- `config.example.yaml`
- `README.md`
- `README_ZH.md`

### 25. WebUI 前端重构：以“可调度”语义为中心（已实现）

目标：
- 消除 WebUI 中“健康/拉黑/订阅/调试”等与当前 DB 调度口径不一致的隐性语义漂移，让界面展示与运行态调度逻辑保持一致。

当前实现行为：
- WebUI 仅保留两个入口：`监控` 与 `节点管理`（移除调试 / Pebble / 设置等面板）。
- 监控页节点状态统一为三态：
  - `not-ready`：`!initial_check_done`
  - `schedulable`：`initial_check_done && available`
  - `unschedulable`：其它情况
- 统计卡片统一围绕调度：总节点 / 可调度 / 不可调度 / 未就绪 / 活跃连接；移除“拉黑/订阅状态”等展示与操作。

涉及模块：
- `internal/monitor/assets/index.html`

### 26. DB 模式调度阈值与运行态收敛：修复“全量历史 stats 抬高 p95 导致 0 可用”（已实现）

问题：
- DB 中 24h 统计以 `host:port` 聚合，且可能包含历史/非当前运行态节点的 key；如果用“全量 stats”直接计算 `p95(non-zero)-5%`，阈值可能被抬到接近 1，导致当前 runtime 节点全部 `Available=false`，进而 pool 报 `no healthy proxy available`。

修复后行为：
- DB 模式下 `InitialCheckDone` 在节点注册时即置为 `true`，避免被“初检”语义阻塞；调度实质只由 DB 推导的 `Available` 决定。
- 计算 `p95(non-zero)-5%` 时限定作用域为“当前 runtime 节点集合的 host:port 交集”，并忽略样本量不足的 key（`success+fail < dbThresholdMinTotal`，当前为 2；避免低样本 100% 主导阈值）。
- 周期 debug 日志每 10 秒输出一次系统汇总并随机抽样 10 个节点，便于定位阈值/统计/可用性问题。

涉及模块：
- `internal/monitor/manager.go`

### 27. 出口 IP（egress_ip）持久化 + 按出口去重调度/加载（已实现）

目标：
- 解决“random pool 随机到不同节点但出口公网 IP 高度同质”的问题：把“出口 IP 多样性”引入 DB 与调度口径。
- 在不改变用户现有接入方式（pool/random）的前提下，使运行时更倾向于“随机出口 IP”，并降低同出口节点重复占用 runtime 配额。

当前实现行为：
- DB 节点结构新增 `egress_ip` 与 `egress_ip_updated_at` 字段，持久化保存出口公网 IP。
- periodic health check（monitor probe）会在原有 `probe_target` 探测前，先通过节点出站请求 `https://www.cloudflare.com/cdn-cgi/trace` 并解析 `ip=...`：
  - 成功：写入 DB 的 `egress_ip`（不额外计为一次成功）
  - 失败：额外记录一次失败（但仍继续执行原来的 `probe_target` 探测）
- DB 模式下的阈值计算（`p95(non-zero)-5%`）会对相同 `egress_ip` 的节点进行分组去重：
  - 每个出口组仅保留一个“代表节点”参与 p95 与阈值计算，代表选择规则：成功率最高；成功率相同则样本数更多优先；再相同则稳定排序。
  - 非代表节点即使成功率很高，也不会被标记为 `Available=true`（确保“同出口只保留一个可调度节点”）。
- 启动加载阶段（store → runtime nodes）同样按 `egress_ip` 去重，并对去重后的节点集合进行排序后截断到最多 5000 个；当去重后仍超过 5000 时，会 **预留 3000 个名额给从未健康检查过的节点**（`HealthCheckCount==0`），其余名额给已测试节点；再执行端口保留的 normalize。
- 性能优化：阈值计算/调试日志不再为获取 `egress_ip` 全表扫描 nodes，而是采用 “按需 point-get + 内存 TTL 缓存” 的方式读取 `egress_ip`，避免 5000+ 节点时的周期性 JSON 解码抖动。

涉及模块：
- `internal/store/store.go`
- `internal/store/pebble/nodes.go`
- `internal/outbound/pool/pool.go`
- `internal/monitor/manager.go`
- `internal/monitor/server.go`
- `internal/app/app.go`

### 28. probe 成功日志 + Cloudflare trace 成功日志（已实现）

目标：
- 排查 “outbound 日志很多但 probe 日志很少/只见失败” 的问题时，补齐成功路径的可观测性。

当前实现行为：
- monitor 的 periodic health check：每个节点 probe 成功会输出 `probe success for ... latency ...ms` 日志；失败仍维持 `probe failed for ...` 日志不变。
- Cloudflare trace 获取出口 IP 成功时输出 `cloudflare trace success for ... egress_ip: ...` 日志；写入 `egress_ip` 失败则额外输出 warn（不影响后续 probe 流程）。

涉及模块：
- `internal/monitor/manager.go`
- `internal/outbound/pool/pool.go`

### 29. debug 节点抽样按可调度/不可调度分组（已实现）

目标：
- debug 抽样日志更贴近排障诉求：同时看到少量可调度节点与不可调度节点的代表样本，避免 5000 节点时随机抽样基本都落在不可调度集合。

当前实现行为：
- 每次 debug 输出节点明细时：
  - 从可调度（`initial_check_done && available`）节点中随机选最多 5 个打印；
  - 从不可调度节点中随机选最多 5 个打印；
  - 不进行额外 DB 扫描（`db24h` 仍基于已缓存的 24h 聚合 stats 做 key 查找）。

涉及模块：
- `internal/monitor/manager.go`

### 30. 修复健康检查轮次取消导致大量无意义失败记录（已实现）

问题：
- 当节点数量很大且单轮健康检查未完成时，下一轮启动会取消上一轮 `roundCtx`；如果取消后仍继续调度后续节点的 probe，会导致大量节点“刚开始就因 ctx canceled 失败”，从而在短时间内累积海量 failure 记录，污染统计与可调度判断。

修复后行为：
- 一旦轮次 `roundCtx` 被取消，本轮不再继续启动未开始的节点 probe；仅允许已在执行中的少量 probe 收尾。

涉及模块：
- `internal/monitor/manager.go`

### 31. boxmgr 的 5000 cap 分支也应用 egress_ip 去重（已实现）

问题：
- 当启动加载未能走 store active 节点分支（例如 store active 获取失败/为空）且 `cfg.nodes` 仍然超过 5000 时，boxmgr 会走 `selectNodesForSingBox` 做 5000 限制；若该分支不做 egress_ip 去重，可能与“按出口去重后再排序截断”的预期不一致。

修复后行为：
- `selectNodesForSingBox` 在可用 store 的情况下，优先按 store 中记录的 `egress_ip` 对候选节点分组去重（无 egress_ip 的按 host:port 处理），并以 24h success rate/样本数/最近延迟做排序后再截断到 5000；失败时回退到原有 healthySet 策略。

涉及模块：
- `internal/boxmgr/manager.go`
