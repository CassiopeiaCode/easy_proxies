# 计划：将 easy-proxies 的数据库从 SQLite（modernc.org/sqlite）迁移到 Pebble

本文目标是形成一个可执行的迁移路线图：把项目中目前依赖 SQLite 的“节点存储 + 健康检查统计/黑名单状态”等持久化能力，替换为嵌入式 KV（优先 Pebble）。

> 结论先行：如果你只需要“状态存储”，Pebble（LSM + batch）很适合你当前 perf 显示的高频 I/O/拷贝开销场景。但迁移代价是：放弃 SQL 的通用查询能力，改为按 key 组织数据与自建索引/聚合。

---

## 1. 当前 SQLite 使用点盘点（代码入口）

SQLite 驱动与连接：
- SQLite 驱动引用：[`internal/database/db.go`](../internal/database/db.go:1)（`_ "modernc.org/sqlite"`）
- SQLite 打开方式：[`database.Open()`](../internal/database/db.go:51) 使用 `sql.Open("sqlite", dsn)` + `_pragma=...WAL...`

从代码看，当前仓库是“强制 DB 模式”（SQLite 作为主要持久化）：
- 配置 normalize 时强制开启 DB：[`Config.normalize()`](../internal/config/config.go:156) 内 `c.Database.Enabled = true`（见 [`internal/config/config.go`](../internal/config/config.go:221)）
- WebUI 保存节点时：优先写 DB、并且 **不写 nodes.txt**（注释写明）：[`Config.SaveNodes()`](../internal/config/config.go:977)
- App 启动时：会将 `cfg.Nodes` 导入 DB，然后从 DB 读取 active nodes 覆盖 `cfg.Nodes`：[`app.Run()`](../internal/app/app.go:21)

目前调用 `database.Open()` 的模块（需要改为新 store 的初始化/注入）：
- 订阅刷新导入：[`subscription.Manager.doRefresh()`](../internal/subscription/manager.go:185)
- 监控 manager（健康统计落库 + 以 DB 派生 Available 阈值）：[`monitor.Manager`](../internal/monitor/manager.go:111)
  - 周期探测每个节点都会写 hourly 聚合：[`Manager.probeAllNodes()`](../internal/monitor/manager.go:308)
  - RecordSuccess/Failure（真实流量事件）也会按 5s 节流落库：[`entry.recordSuccessWithLatency()`](../internal/monitor/manager.go:814)
  - 并且会从 DB 查询近 24h rates 计算 p95-5% 阈值来设置 Available：[`Manager.applyHealthThresholdFromDB()`](../internal/monitor/manager.go:1083)
- 配置层校验/初始化/写回：[`config.Config`](../internal/config/config.go:28)
- builder 构建阶段读取 DB：跳过 damaged，且 build-time invalid 会 mark damaged：[`builder.Build()`](../internal/builder/builder.go:27)
- BoxManager 的 CRUD（Create/Update/Delete node）与“标记 damaged”：[`boxmgr.Manager`](../internal/boxmgr/manager.go:1)
- App 启动时导入+以 DB active nodes 为主：[`app.Run()`](../internal/app/app.go:21)

SQLite 当前承载的数据与行为（见 schema）：
- `nodes`：节点去重主键 `(host, port)`、URI、name、protocol、damaged、黑名单、统计累计、last_* 时间与延迟
- `node_health_hourly`：按小时聚合的 success/fail/latency sum/count
- 写路径：[`DB.UpsertNodeByHostPort()`](../internal/database/db.go:154)、[`DB.RecordHealthCheck()`](../internal/database/db.go:225)、[`DB.MarkNodeDamaged()`](../internal/database/db.go:197)
- 读路径：[`DB.ListActiveNodes()`](../internal/database/db.go:369)、[`DB.QueryNodeRatesSince()`](../internal/database/db.go:307)、[`DB.IsNodeDamaged()`](../internal/database/db.go:427)
- 维护路径：[`DB.CleanupHealthStatsBefore()`](../internal/database/db.go:351)

---

## 2. 迁移目标与范围界定

### 2.1 迁移目标
- 降低高频健康检查/订阅导入导致的 CPU 与内核 I/O 拷贝开销（perf 中大量 `pread64`/`rep_movs_alternative`）
- 保持功能不回退：
  - 节点去重（host:port）
  - damaged 标记与跳过逻辑
  - 黑名单到期时间
  - 健康检查统计（至少支持“近 24h 成功率/失败率/平均延迟”）
  - monitor UI/API 所需的统计字段

### 2.2 是否保留 SQL
本计划采用**一次性切换**策略：完全移除 SQLite，直接迁移到 Pebble。
- 不提供“双写/双读/灰度切换”路径（除非后续另行添加）。
- 迁移窗口内需要停机（或至少停止健康检查写入），执行一次性迁移，然后以 Pebble 数据目录启动。

---

## 3. Pebble 方案设计（建议的数据模型）

Pebble 是 KV，不提供 SQL；需要定义 key 结构与序列化。

### 3.1 Key 设计
建议按“前缀 + 主键字段”组织，并尽量保持顺序扫描友好：

1) 节点元数据（等价于 `nodes` 表）
- Key：`node/<host>/<port>`
- Value：NodeRecord（建议 msgpack/json/protobuf；字段包含：uri/name/protocol/is_damaged/damage_reason/blacklisted_until/created_at/updated_at/last_check_at/last_success_at/last_latency_ms/health_check_count/health_check_success_count/failure_count）

2) 小时聚合统计（等价于 `node_health_hourly`）
- Key：`hc_hour/<YYYYMMDDHH>/<host>/<port>`
- Value：{success_count, fail_count, latency_sum_ms, latency_count, updated_at}

3) 可选索引（如果需要快速列出 active nodes 或 damaged nodes）
- 方案 A：直接扫描 `node/` 前缀，按 value 过滤（简单但可能慢）
- 方案 B：维护二级索引 key：
  - `idx/active/<host>/<port>` -> empty（仅在 is_damaged=0 时存在）
  - `idx/damaged/<host>/<port>` -> empty
  - `idx/blacklisted/<until_ts>/<host>/<port>` -> empty（支持按时间清理/扫描）

### 3.2 写入策略（解决 perf 痛点）
- 健康检查写入：做“内存聚合 + 定时 flush + pebble.Batch”
  - 每次探测只更新内存 map
  - 每 1s 或每 N=1000 次更新，批量写入 `hc_hour/..` 与 `node/..`（last_*）
- 订阅导入：用 batch upsert 节点（host:port dedup）

### 3.3 TTL/清理
- SQLite 的 `CleanupHealthStatsBefore()` 迁移为：扫描 `hc_hour/<YYYYMMDDHH>/...` 前缀，按 cutoff 删除。

---

## 4. 代码结构改造建议（分层接口）

### 4.1 新增统一接口层（替代 `internal/database` 直接暴露 SQL）
建议新增包：`internal/store`（或 `internal/state`），定义接口：
- `Open(ctx, cfg) (Store, error)`
- `Store.UpsertNodeByHostPort(ctx, in)`
- `Store.MarkNodeDamaged(ctx, host, port, reason)`
- `Store.RecordHealthCheck(ctx, update)` 或 `Store.RecordHealthChecksBatch(ctx, []update)`
- `Store.ListActiveNodes(ctx)`
- `Store.QueryNodeRatesSince(ctx, since)`
- `Store.CleanupHealthStatsBefore(ctx, cutoff)`

然后实现：
- `internal/store/pebble`：Pebble 实现

### 4.2 注入与生命周期
- 在 app 启动时初始化 store（替换目前多处 `database.Open()` 反复开关 DB）
- `boxmgr/monitor/subscription/builder` 改为依赖 `Store` 接口，而不是直接调用 `database.Open()`

---

## 5. 需要改动的文件清单（包含配置与非代码）

### 5.1 Go 代码（必须改）
SQLite 实现与调用点：
- 替换/移除 SQLite 实现：[`internal/database/db.go`](../internal/database/db.go:1)
- 所有打开 DB 的地方改成使用新 Store（初始化一次、注入）：
  - [`internal/subscription/manager.go`](../internal/subscription/manager.go:1)
  - [`internal/monitor/manager.go`](../internal/monitor/manager.go:1)
  - [`internal/config/config.go`](../internal/config/config.go:1)
  - [`internal/builder/builder.go`](../internal/builder/builder.go:1)
  - [`internal/boxmgr/manager.go`](../internal/boxmgr/manager.go:1)
  - [`internal/app/app.go`](../internal/app/app.go:1)

新增文件（计划中将创建）：
- `internal/store/store.go`（接口定义）
- `internal/store/pebble/pebble.go`（Open/Close、Get/Set/Iterate、Batch、序列化）
- `internal/store/pebble/codec.go`（key 编码、value 序列化）
- `internal/store/pebble/migrate.go`（如需要从旧 sqlite 导入）

### 5.2 go.mod / 依赖（必须改）
- [`go.mod`](../go.mod:1)
  - 移除 `modernc.org/sqlite`
  - 增加 `github.com/cockroachdb/pebble`（或选定的 pebble 版本）

### 5.3 配置（必须改）
当前配置中存在 database 相关字段（SQLite path）：
- [`config.example.yaml`](../config.example.yaml:1)
- 配置结构定义与校验：[`internal/config/config.go`](../internal/config/config.go:1)

需要做的变更：
- `database.type`：新增枚举（`sqlite|pebble`），或直接改为 `store.type`
- `database.path`：对 pebble 仍然需要目录路径（Pebble 通常是目录，而 SQLite 是文件）
- 可能新增：
  - `pebble.cache_size_mb`
  - `pebble.memtable_size_mb`
  - `pebble.max_open_files`
  - `pebble.wal_dir`（可选）

### 5.4 文档（需要改）
- [`README.md`](../README.md:1) / [`README_ZH.md`](../README_ZH.md:1)：更新“数据库选项/路径/迁移方法”
- 可能新增：`docs/storage-pebble.md`（运行参数、目录布局、备份/恢复）

### 5.5 Docker / Compose / 脚本（需要改）
- [`Dockerfile`](../Dockerfile:1)
  - 需要创建并 `chown` Pebble 数据目录（例如 `/var/lib/easy-proxies/pebble`），因为 runtime 使用 `USER easy`：[`Dockerfile`](../Dockerfile:22)
- [`docker-compose.yml`](../docker-compose.yml:1)
  - 目前已经挂载 sqlite 文件：[`docker-compose.yml`](../docker-compose.yml:31)
  - 且注释提示 `chmod ... easy_proxies.db`：[`docker-compose.yml`](../docker-compose.yml:32)
  - 迁移到 Pebble 后要改为挂载目录（volume）并更新注释（例如 `chmod/chown` 一个目录）。
- [`start.sh`](../start.sh:1)
  - 若脚本或 README 引用 `easy_proxies.db` 文件，需要改为 Pebble 目录。

### 5.6 示例数据/资源（视情况）
- [`nodes.example`](../nodes.example:1)：一般不需要动
- `resources/`、`scripts/`：若有数据库备份/清理脚本需要更新

---

## 6. 迁移步骤（一次性切换）

### Step 0：冻结接口与字段
- 明确 Pebble 必须覆盖的功能与字段（对齐现有 schema）：见 [`internal/database/db.go`](../internal/database/db.go:95)
- 列出 monitor UI/API 依赖字段，防止“迁移后 UI 缺字段”

### Step 1：实现 Pebble Store（不保留 SQLite 实现）
- 新增 `internal/store` 接口与 `internal/store/pebble` 实现
- 统一在 app 启动时初始化 store，并注入到：
  - [`internal/subscription/manager.go`](../internal/subscription/manager.go:1)
  - [`internal/monitor/manager.go`](../internal/monitor/manager.go:1)
  - [`internal/builder/builder.go`](../internal/builder/builder.go:1)
  - [`internal/boxmgr/manager.go`](../internal/boxmgr/manager.go:1)

### Step 2：删除 SQLite 代码与依赖
- 删除/清空 `internal/database` 旧实现（或重命名为 `internal/store/pebble` 并迁移类型定义）
- 移除 `modernc.org/sqlite`：更新 [`go.mod`](../go.mod:1)
- 移除所有 `database.Open()` 调用点，改为 store 注入

### Step 3：配置与部署变更
- 配置从 `database.path`（sqlite 文件）改为 Pebble 数据目录（例如 `/var/lib/easy-proxies/pebble`）
- 更新：
  - [`config.example.yaml`](../config.example.yaml:1)
  - [`internal/config/config.go`](../internal/config/config.go:1)
  - [`README.md`](../README.md:1) / [`README_ZH.md`](../README_ZH.md:1)
  - [`docker-compose.yml`](../docker-compose.yml:1)（挂载目录 volume）
  - [`Dockerfile`](../Dockerfile:1)（创建并 chown 数据目录，确保 `USER easy` 有权限）

### Step 4：一次性迁移工具（必须有，否则旧数据丢失）
- 增加一个命令：`easy-proxies migrate sqlite-to-pebble --sqlite <file> --pebble <dir>`
  - 读取 sqlite 的 `nodes` 与 `node_health_hourly`，写入 pebble
  - 迁移完成后输出校验摘要（node 数、damaged 数、最近 24h 总成功/失败）

### Step 5：验证与回滚预案
- 验证项：
  - `ListActiveNodes` 数量/字段一致
  - damaged 跳过逻辑一致
  - monitor 展示的成功率/平均延迟与 sqlite 时一致（允许微小差异）
- 回滚：保留 sqlite 文件与 pebble 目录备份；回滚时切换旧镜像/旧配置

---

## 7. 风险与验证清单

风险：
- KV 模型下的查询/聚合需要自己实现，monitor UI 依赖字段可能遗漏
- 扫描量大时，缺少二级索引会慢（需要 idx key 或缓存）

验证：
- 与旧 SQLite 行为对齐：
  - damaged 节点在 builder/启动时能被跳过
  - `ListActiveNodes` 输出与原一致
  - `QueryNodeRatesSince(24h)` 返回一致
- 性能：
  - 健康检查写入 QPS 提升、CPU 降低
  - perf 中 `pread64/rep_movs` 占比显著下降
