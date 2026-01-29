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
- 健康检查结果按“小时”聚合落库：新增 `node_health_hourly` 表，按 UTC 小时累计 `success_count/fail_count/latency_sum_ms/latency_count`。
- 每轮周期性健康检查（monitor 侧 probe）会写入 DB 的 hourly 统计，并同步更新 nodes 表里的累计计数与 last_* 字段（用于展示/快速查询）。
- 统计窗口为最近 24h：从 hourly 表聚合出每个节点的成功率（success / (success+fail)）。
- 计算 p95 成功率并下调 5% 形成阈值：threshold = p95 - 0.05。
- pool 调度过滤：被标记为 `InitialCheckDone=true 且 Available=false` 的节点不进入候选集（即成功率低于阈值的“不健康节点”不可调度；且没有 24h 内健康检查统计的节点也不可调度）。
- 自动清理：`node_health_hourly` 表自动删除 7 天前的历史聚合数据。

涉及模块：
- internal/database/db.go：新增 `node_health_hourly` 表；写入聚合统计；提供 24h 成功率聚合查询接口；提供历史数据清理接口。
- internal/monitor/manager.go：周期性 probe 结果写库；按 24h 成功率计算 p95-5% 阈值并更新节点 Available；定时清理 7 天前统计数据。
- internal/outbound/pool/pool.go：调度时读取 monitor 的 Available/InitialCheckDone 进行过滤。

注意：
- 成功率统计与阈值过滤依赖从 URI 提取 host:port；若某类 URI 无法提取 host:port，则不会参与 hourly 统计与阈值过滤（保持原先可用性语义）。

### 5. 探测目标 URL/HTTPS + 自定义状态码验证（已实现）

目标：`management.probe_target` 支持直接填 URL（含 https），并且可通过 `probe_expected_status` / `probe_expected_statuses` 对返回 HTTP 状态码做校验；只有状态码匹配才算探测成功。

当前实现行为：
- `management.probe_target` 支持两类写法：
  - `host:port`：按 HTTP 探测，默认 path 为 `/generate_204`。
  - `http(s)://host[:port]/path[?query]`：按 URL 解析；`https://` 会启用 TLS；未显式写端口时 http 默认 80、https 默认 443；path 为空或 `/` 时回退到 `/generate_204`。
- 探测逻辑支持 HTTP + HTTPS：
  - 当 `probe_target` 为 https 时，会先进行 TLS 握手（SNI=hostname）；证书校验遵循全局 `skip_cert_verify`（跳过校验时会启用 `InsecureSkipVerify`）。
  - 发送 `GET <path> HTTP/1.1` 并读取 status line，用于计算 TTFB（探测耗时 = dial + probe）。
- 状态码校验：
  - `probe_expected_statuses` 优先；否则使用单值 `probe_expected_status`。
  - 若未配置期望状态码，则保持兼容：只要能成功读到响应 status line 即视为成功（不强制校验 code）。
- 超时：
  - TLS 握手/写入/读取均设置了 deadline，避免探测卡死。

涉及模块：
- internal/monitor/manager.go：probe_target URL 解析与 https 探测参数下发。
- internal/outbound/pool/pool.go：HTTP/HTTPS 探测实现（含状态码校验与 TLS 握手）。
- internal/builder/builder.go：把期望状态码配置下发到 pool outbound。
- internal/monitor/manager_test.go：probe_target 解析行为单测覆盖（https/http/host:port 等）。

### 6. 日志节流（已实现）

目标：按“调用点（go 文件 + 行号）”进行节流，同一个日志调用点每秒最多输出 10 条，避免错误风暴刷屏并影响性能。

已实现

## 关键语义：damaged vs health

- damaged：持久化状态（DB），用于“导入/重载阶段”剔除明显错误/不应被加载的节点；damaged 的变动应伴随 sing-box 重载/构建发生。
- health（健康度）：基于最近 24h 成功率（p95-5% 阈值）判断节点是否“可调度”；调度层不再使用运行时 blacklist 概念，运行时失败只会进入健康统计（hourly 聚合）并在后续周期性 probe 后影响可调度性。