# 与上游项目的差异

本文档记录了本 fork 与上游 [easy_proxies](https://github.com/jasonwong1991/easy_proxies) 项目的所有改动和新增功能。

## 新增需求

### 1. 节点状态 SQLite 持久化

将节点状态从内存管理改为 SQLite 数据库持久化存储，创建 nodes 表包含 id、host、port、uri、name、protocol、created_at、updated_at、is_damaged、health_check_count、health_check_success_count、last_check_at、last_success_at、last_latency_ms、failure_count、blacklisted_until 等字段，以 host:port 为唯一约束，节点状态实时更新到数据库，黑名单状态和健康检查结果持久化保存，受影响模块包括 internal/monitor/manager.go、internal/outbound/pool/shared_state.go 和新增的 internal/database/ 模块。

### 2. 启动时数据库导入 + 异步订阅去重

应用启动时立即从数据库加载已有节点并快速启动代理服务，同时启动异步线程获取订阅节点，新节点通过 host:port 主键与数据库节点去重后入库并触发配置重载，如果节点保存失败则标记 is_damaged=1 且不参与代理池选择，WebUI 显示损坏原因并提供修复或删除接口，受影响模块包括 internal/config/config.go、internal/subscription/manager.go 和 internal/app/app.go。

### 3. 健康检查次数统计

在数据库中记录 health_check_count（总检查次数）和 health_check_success_count（成功次数）以及 last_check_at、last_success_at 字段，Pool 模式选择节点时过滤 health_check_count=0 的节点避免使用未检查节点，WebUI 显示节点成功率为 success_count/check_count*100%，新节点入库后 health_check_count=0 只有首次健康检查完成后才能被代理池使用，受影响模块包括 internal/monitor/manager.go、internal/outbound/pool/pool.go 和 internal/monitor/server.go。

### 4. 自定义状态码验证

在配置文件 management 部分新增 probe_expected_status 或 probe_expected_statuses 字段支持指定期望的 HTTP 状态码（如 204、200、301、302 等），修改 httpProbe() 函数解析 HTTP 响应状态行提取状态码仅当状态码匹配配置时才算成功，未配置时保持现有行为（任何响应都算成功）确保向后兼容，受影响模块包括 internal/config/config.go、internal/outbound/pool/pool.go 和 internal/monitor/manager.go。

## 技术实现

首次启动时自动创建数据库和表，现有配置文件格式不变，WebUI 界面保持兼容并新增统计字段显示，数据库操作添加索引避免频繁写入影响性能，使用事务和锁保证数据一致性，首次启动需要从 nodes.txt 导入到数据库完成迁移。