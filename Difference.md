# 与上游项目的差异

本文档记录了本 fork 与上游 [easy_proxies](https://github.com/jasonwong1991/easy_proxies) 项目的所有改动和新增功能。

## 新增需求 / 已实现差异

### 1. 节点状态 SQLite 持久化（已实现：DB 层 + schema）

新增 SQLite 数据库存储 nodes 表（host:port 唯一约束），字段包含：id、host、port、uri、name、protocol、is_damaged、damage_reason、health_check_count、health_check_success_count、last_check_at、last_success_at、last_latency_ms、failure_count、blacklisted_until、created_at、updated_at，并建立必要索引。实现位于 internal/database/db.go。

说明：目前持久化层主要用于“导入/重载时的 damaged 标记与过滤”等基础能力；运行时 blacklist 仍是内存态（见下文“damaged vs blacklist”）。

### 2. 构建期坏节点自动标记 damaged，并且不载入 sing-box（已实现）

目标：坏节点不应进入 sing-box 配置；damaged 的变动应伴随 sing-box 重载/构建阶段发生（而不是运行时失败不断修改 damaged）。

当前实现行为：
- 构建期（sing-box build/reload）对每个节点先做 outbound 构建校验；构建失败则该节点不会进入 sing-box outbounds。
- 若启用 database（database.enabled=true），构建失败的节点会 best-effort 标记为 is_damaged=1，并写入 damage_reason（仅当能从 URI 提取 host:port 时）。
- 若某节点在 DB 中已经 is_damaged=1，则构建期会直接跳过该节点（不会载入 sing-box）。
- 若 sing-box 在启动/重载时出现 `initialize outbound[N]` 这类初始化错误，会 best-effort 标记对应节点 damaged 并快速重试；重试超过 5 次才退出。

涉及模块：
- internal/builder/builder.go：构建期校验、写入 damaged、跳过 damaged 节点。
- internal/boxmgr/manager.go：sing-box init outbound 错误映射 damaged + 启动/重载重试。
- internal/database/db.go：is_damaged/damage_reason 字段与查询接口。

注意：host:port 主键现在支持从 `vmess://<base64(json)>` 解析（提取 json 的 `add` + `port`），因此这类节点也可以去重入库、标记 damaged、参与健康统计与阈值调度。若仍有极端 URI 无法提取 host:port，则该节点会被直接过滤掉（不入库、不载入、不参与调度）。

### 3. 启动时数据库导入 + 异步订阅去重（已实现：DB 主数据源 + nodes.txt 兼容）

目标：启动优先从 DB 加载可用节点快速启动；订阅/文件导入按 host:port 去重入库，入库成功触发重载；无法提取 host:port 的节点直接过滤掉（不入库、不载入、不参与调度）。

当前实现行为：
- `nodes.txt` 始终作为数据源：配置加载阶段总会读取 `nodes_file`（默认 `nodes.txt`）；即使存在订阅，也不会忽略本地 nodes.txt。
  - 支持 `socks://` / `socks5://` / `socks4://` / `socks4a://` 节点 URI。
  - 兼容 “`URI 备注...`” 行格式：会取每行第一个 token 作为 URI，其余内容忽略（便于保留地区/来源描述）。
- 启动阶段 DB 导入 + DB 加载：
  - 若启用 database，则会 best-effort 将当前 `cfg.Nodes`（包含 inline + nodes.txt）按 host:port upsert 入库；
  - 随后从 DB `nodes` 表加载 `is_damaged=0` 的 active nodes 作为运行时节点列表（DB 成为主数据源）。
- 订阅刷新异步去重入库：
  - SubscriptionManager 拉取订阅后先按 host:port upsert 入库去重；
  - 再从 DB 读取 active nodes 组装成新的运行时 nodes 并触发 reload；
  - 同时把最终节点集合写回 `nodes.txt` 作为缓存/下次启动的数据源。

涉及模块：
- internal/app/app.go：启动时 upsert 导入 + 从 DB 加载 active nodes。
- internal/config/config.go：nodes.txt 始终作为数据源；订阅仅作为运行时异步刷新，不在 Load 阶段拉取。
- internal/subscription/manager.go：订阅刷新 upsert 去重入库 + 从 DB 读 active nodes reload + 写回 nodes.txt。

### 4. 健康检查统计落库 + 24h 成功率阈值调度（已实现）

目标：健康检查结果需要持久化，并用最近 24h 的成功率分布做“健康可调度”过滤：计算所有节点成功率的 p95，再取阈值 = p95 - 5%，只有成功率 >= (p95 - 5%) 的节点才可被调度。

当前实现行为：
- 健康检查结果按“小时”聚合落库：新增 `node_health_hourly` 表，按 UTC 小时累计 `success_count/fail_count/latency_sum_ms/latency_sum_ms/latency_count`。
- 周期性健康检查（monitor 侧 probe）：
  - 每轮健康检查会随机打散节点探测顺序，避免固定顺序导致探测偏置与尾部节点长期延后。
  - 若新一轮健康检查到期启动时上一轮仍未结束，则强制取消上一轮并立即启动新一轮；已完成的节点探测结果不会丢失（逐节点写入内存状态与 DB hourly 聚合是边跑边落的）。
  - 写入 DB 的 hourly 统计，并同步更新 nodes 表里的累计计数与 last_* 字段（用于展示/快速查询）。
  - DB 启用时，不再由 probe 结果直接写入 `Available`，只更新 last_* 观测字段；`Available` 统一由 DB 阈值计算产出。
- 真实连接（业务流量）健康事件入库（DB 启用时）：
  - `RecordFailure/RecordSuccess/RecordSuccessWithLatency` 会（节流）写入 `node_health_hourly`，使真实连接的成功/失败也能影响 24h 成功率。
  - 写入后会触发（节流的）DB 阈值重算，使运行态 `Available` 尽快收敛到最新 DB 统计口径。
- 统计窗口为最近 24h：从 hourly 表聚合出每个节点的成功率（success / (success+fail)）。
- 计算 p95 成功率并下调 5% 形成阈值：threshold = p95 - 0.05。
- DB 启用时，`Available` 的唯一写入来源：p95-5% 阈值计算（无其它路径可覆写）。
  - 若 DB 启用但近 24h 没有任何统计行，则所有节点视为不可调度（Available=false），避免沿用历史脏状态。
- pool 调度过滤：仅允许 `InitialCheckDone=true 且 Available=true` 的节点进入候选集；`InitialCheckDone=false`（尚未完成初始检查/尚未应用 DB 阈值）也不会被调度，避免未检查节点承载真实流量。
- 全量重算：DB 启用时，每 5 分钟强制全量重算全部节点的 `Available`（即使 probe 间隔更长或真实流量触发被节流）。
- 自动清理：`node_health_hourly` 表自动删除 7 天前的历史聚合数据。

涉及模块：
- internal/database/db.go：新增 `node_health_hourly` 表；写入聚合统计；提供 24h 成功率聚合查询接口；提供历史数据清理接口。
- internal/monitor/manager.go：周期性 probe 结果写库；真实连接健康事件写库；按 24h 成功率计算 p95-5% 阈值并更新节点 Available；每 5 分钟全量重算；定时清理 7 天前统计数据。
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
- 在观察到节点注册后，会立即将 DB 的 24h 健康阈值应用到运行态节点（不必等待下一轮 periodic probe），用于尽快收敛 `Available/InitialCheckDone` 语义与调度/展示口径。

涉及模块：
- internal/boxmgr/manager.go：waitForMonitorRegistration + 启动/重载流程里的调用。

### 8. 重载时取消正在进行的健康检查，避免旧实例探测污染（已实现）

背景：健康检查进行中触发重载时，旧 sing-box instance 会被关闭，但 monitor 的 probe goroutine 可能仍在跑；探测继续使用旧 outbound 的 probe 会造成大量失败、UI 抖动、DB 统计被污染。

当前实现行为：
- 重载开始时，先取消 monitor 的运行时 context（终止 in-flight probes/periodic loop），并清空运行时节点注册表（nodes map）；等待新实例启动后重新注册节点。
- 重载成功后，同样等待 monitor 重新注册，再启动 periodic health check。
- 在观察到节点重新注册后，会立即将 DB 的 24h 健康阈值应用到运行态节点（不必等待下一轮 periodic probe），用于尽快收敛 `Available/InitialCheckDone` 语义与调度/展示口径。

涉及模块：
- internal/monitor/manager.go：新增 ResetRuntime（保留 DB/Config，仅重置运行态）。
- internal/boxmgr/manager.go：Reload 中调用 ResetRuntime，重载后重启 periodic health check。

### 9. nodes.txt 只读 + 强制启用数据库（已实现）

目标：
- `nodes.txt` 只作为“可读的数据源/导入来源”，进程不再写入该文件（避免 WebUI/订阅刷新覆盖人工内容）。
- 强制启用 SQLite DB：节点的增删改/订阅合并等持久化全部走 DB。

当前实现行为：
- 启动脚本将 `nodes.txt` 设置为只读（`chmod 444`），不再尝试给 `nodes.txt` `chmod 666`。
- 配置加载阶段强制 `database.enabled=true`（并保留默认 `database.path`）。
- WebUI 的节点 CRUD 保存不再写 `nodes.txt`：`SaveNodes()` 会把节点 upsert 到 DB。
- 订阅刷新不再写回 `nodes.txt`（不再作为 cache），仅通过 DB 合并/去重后触发 reload。

涉及模块：
- start.sh：调整文件权限策略（config.yaml 可写、nodes.txt 只读）。
- internal/config/config.go：normalize 强制启用 DB；SaveNodes 写 DB 而不是写 nodes.txt。
- internal/subscription/manager.go：订阅刷新流程移除写 nodes.txt 的步骤。

## 关键语义：damaged vs health

- damaged：持久化状态（DB），用于“导入/重载阶段”剔除明显错误/不应被加载的节点；damaged 的变动应伴随 sing-box 重载/构建发生。
- health（健康度）：基于最近 24h 成功率（p95-5% 阈值）判断节点是否“可调度”。
  - DB 启用时，`Available` 只由 DB 阈值计算写入（p95-5%）；启动初检/手动标记/probe 即时结果不会覆写可调度性口径。
  - 运行时失败不仅来源于周期性 probe，也包括真实连接的成功/失败事件（节流入库），从而让业务流量能反馈到健康统计与可调度性。