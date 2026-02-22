# SQLite → Pebble 迁移指南（含配置与数据）

本文描述如何将 easy-proxies 从旧版 **SQLite 单文件数据库** 迁移到新版 **Pebble 目录型数据库**，并同步更新配置文件与 Docker 部署。

> 重要：Pebble 是 **目录**（包含多个文件），不再是 `easy_proxies.db` 这种单文件。

---

## 0. 迁移前检查清单

- 你需要确认当前运行实例的数据位置：
  - 旧 SQLite 文件（典型）：`/etc/easy-proxies/easy_proxies.db`
  - 旧配置文件：`/etc/easy-proxies/config.yaml`
  - `nodes.txt`（如果有）：`/etc/easy-proxies/nodes.txt`

- 迁移建议在停机窗口进行：
  - 健康检查与订阅刷新会持续写入状态；迁移期间应停机，避免数据不一致。

---

## 1. 配置文件变化（向后兼容）

### 1.1 新增 `store` 段（推荐写法）

新版配置支持：

```yaml
store:
  type: pebble
  dir: /etc/easy-proxies/easy_proxies.pebble
```

- `store.type`：目前仅支持 `pebble`
- `store.dir`：Pebble 数据目录

对应解析逻辑见：[`Config.normalize()`](../internal/config/config.go:176)

### 1.2 旧版 `database.path` 仍可用（兼容写法）

如果你暂时不想改配置，旧配置中的：

```yaml
database:
  enabled: true
  path: /etc/easy-proxies/easy_proxies.db
```

在新版会被解释为：
- 若 `database.path` 以 `.db` 结尾：自动推导 Pebble 目录为同路径的 `*.pebble`
  - 例：`/etc/easy-proxies/easy_proxies.db` → `/etc/easy-proxies/easy_proxies.pebble`
- 若 `database.path` 本身就是目录：直接作为 Pebble 目录

兼容推导逻辑见：[`Config.normalize()`](../internal/config/config.go:245)

---

## 2. Docker / Compose 挂载变化

### 2.1 docker-compose.yml（从“文件挂载”改为“目录挂载”）

旧版常见：

```yaml
volumes:
  - ./easy_proxies.db:/etc/easy-proxies/easy_proxies.db
```

新版建议改为：

```yaml
volumes:
  - ./easy_proxies.pebble:/etc/easy-proxies/easy_proxies.pebble
```

原因：Pebble 是目录，需要持久化整个目录。

当前仓库 compose 仍带 SQLite 示例（需要更新）：[`docker-compose.yml`](../docker-compose.yml:31)

### 2.2 Dockerfile 目录权限

容器运行用户是 `easy`（非 root），Pebble 目录需要可写。

- 建议在镜像里创建并 `chown` 数据目录（例如 `/etc/easy-proxies/easy_proxies.pebble` 或 `/var/lib/easy-proxies/easy_proxies.pebble`）

相关位置：[`Dockerfile`](../Dockerfile:10)

---

## 3. 数据迁移（两种策略）

### 策略 A：保留旧 SQLite 数据（推荐，需迁移工具）

要做到“旧健康统计/节点 damaged 状态不丢失”，需要从 sqlite 读取并写入 pebble。

当前状态：
- 本分支已经 **完全移除 SQLite 依赖与代码**（`internal/database` 已删除），因此主程序不再包含 sqlite 读取能力。
- 若需要策略 A，需要新增一个迁移工具（建议独立命令/独立二进制），临时引入 sqlite driver 只用于迁移。

迁移工具建议形态：
- 新增独立工具：`cmd/easy_proxies_migrate`
- 命令形如：
  - `easy-proxies-migrate sqlite-to-pebble --sqlite <file> --pebble <dir>`

迁移内容建议覆盖：
- `nodes`（URI、name、protocol、is_damaged、damage_reason、blacklisted_until、last_*、health_check_count 等）
- `node_health_hourly`（hourly 聚合 success/fail/latency）

### 策略 B：不迁移 SQLite 数据（快速升级，数据清零）

如果你接受：
- 节点 damaged 标记
- 历史健康统计

这些全部重置，那么可以不做迁移工具。

做法：
1) 停机
2) 修改 config 指向新的 `store.dir`
3) 删除旧 `easy_proxies.db`（可保留备份）
4) 启动新版，系统会根据 config/nodes/subscription 重新导入节点

---

## 4. 迁移执行步骤（策略 B：数据清零版）

1) 停机
```bash
docker compose down
```

2) 备份旧数据
```bash
cp ./easy_proxies.db ./easy_proxies.db.bak
cp ./config.yaml ./config.yaml.bak
```

3) 更新 config.yaml（推荐显式写 store）
```yaml
store:
  type: pebble
  dir: /etc/easy-proxies/easy_proxies.pebble
```

4) 更新 docker-compose.yml 挂载
```yaml
volumes:
  - ./easy_proxies.pebble:/etc/easy-proxies/easy_proxies.pebble
```

5) 启动
```bash
docker compose up -d
```

6) 验证
- WebUI 能打开
- 节点能导出
- 运行一段时间后，观察健康检查统计是否正常

---

## 5. 迁移执行步骤（策略 A：保留历史数据版，待补迁移工具）

由于当前主程序已移除 sqlite，策略 A 需要先实现迁移工具。

建议流程：
1) 停机
2) 运行迁移工具把 `easy_proxies.db` → `easy_proxies.pebble/`
3) 更新 config/compose 指向 `store.dir`
4) 启动新版

---

## 6. 常见问题

### Q1：我没写 `store`，只写了 `database.path=...easy_proxies.db`，新版会怎样？
会自动把 Pebble dir 推导为同目录 `easy_proxies.pebble`：见 [`Config.normalize()`](../internal/config/config.go:245)

### Q2：Pebble 目录权限不够怎么办？
确保宿主机目录可写，并且容器内映射路径对 `USER easy` 有写权限。

---

## 7. 相关实现参考

- store 配置解析与兼容逻辑：[`internal/config/config.go`](../internal/config/config.go:23)
- Pebble store 实现：[`internal/store/pebble`](../internal/store/pebble/pebble.go:1)
- App 启动导入与从 store 加载 active nodes：[`internal/app/app.go`](../internal/app/app.go:1)
