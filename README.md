# Easy Proxies

基于 [sing-box](https://github.com/SagerNet/sing-box) 的代理节点池管理工具，支持多协议、多节点自动故障转移和负载均衡。

## 特性

- **多协议支持**: VMess、VLESS、Hysteria2、Shadowsocks、Trojan
- **多种传输层**: TCP、WebSocket、HTTP/2、gRPC、HTTPUpgrade
- **订阅链接支持**: 自动从订阅链接获取节点，支持 Base64、Clash YAML 等格式
- **节点池模式**: 自动故障转移、负载均衡
- **多端口模式**: 每个节点独立监听端口
- **Web 监控面板**: 实时查看节点状态、延迟探测、一键导出节点
- **密码保护**: WebUI 支持密码认证，保护节点信息安全
- **自动健康检查**: 启动时自动检测所有节点可用性，定期（5分钟）检查节点状态
- **智能节点过滤**: 自动过滤不可用节点，WebUI 和导出按延迟排序
- **灵活配置**: 支持配置文件、节点文件、订阅链接多种方式

## 快速开始

### 1. 配置

复制示例配置文件：

```bash
cp config.example.yaml config.yaml
cp nodes.example nodes.txt
```

编辑 `config.yaml` 配置监听地址和认证信息，编辑 `nodes.txt` 添加代理节点。

### 2. 运行

**Docker 方式（推荐）：**

```bash
./start.sh
```

或手动执行：

```bash
docker compose up -d
```

**本地编译运行：**

```bash
go build -tags "with_utls with_quic with_grpc" -o easy-proxies ./cmd/easy_proxies
./easy-proxies --config config.yaml
```

## 配置说明

### 基础配置

```yaml
mode: pool                    # 运行模式: pool (节点池) 或 multi-port (多端口)
log_level: info               # 日志级别: debug, info, warn, error
external_ip: ""               # 外部 IP 地址，用于导出时替换 0.0.0.0（Docker 部署时建议配置）

# 订阅链接（可选，支持多个）
subscriptions:
  - "https://example.com/subscribe"

# 管理接口
management:
  enabled: true
  listen: 0.0.0.0:9090        # Web 监控面板地址
  probe_target: www.apple.com:80  # 延迟探测目标
  password: ""                # WebUI 访问密码，为空则不需要密码（可选）

# 统一入口监听
listener:
  address: 0.0.0.0
  port: 2323
  username: username
  password: password

# 节点池配置
pool:
  mode: sequential            # sequential (顺序) 或 random (随机)
  failure_threshold: 3        # 失败阈值，超过后拉黑节点
  blacklist_duration: 24h     # 拉黑时长

# 多端口模式
multi_port:
  address: 0.0.0.0
  base_port: 24000            # 起始端口，节点依次递增
  username: mpuser
  password: mppass
```

### 运行模式详解

#### Pool 模式（节点池）

所有节点共享一个入口地址，程序自动选择可用节点：

```yaml
mode: pool

listener:
  address: 0.0.0.0
  port: 2323
  username: user
  password: pass

pool:
  mode: sequential  # sequential (顺序) 或 random (随机)
  failure_threshold: 3
  blacklist_duration: 24h
```

**适用场景：** 自动故障转移、负载均衡

**使用方式：** 配置代理为 `http://user:pass@localhost:2323`

#### Multi-Port 模式（多端口）

每个节点独立监听一个端口，精确控制使用哪个节点：

**配置格式：** 支持两种写法

```yaml
mode: multi-port  # 推荐：连字符格式
# 或
mode: multi_port  # 兼容：下划线格式
```

**完整配置示例：**

```yaml
mode: multi-port

multi_port:
  address: 0.0.0.0
  base_port: 24000  # 端口从这里开始自动递增
  username: user
  password: pass

# 使用 nodes_file 简化配置
nodes_file: nodes.txt
```

**启动时输出：**

```
📡 Proxy Links:
═══════════════════════════════════════════════════════════════
🔌 Multi-Port Mode (3 nodes):

   [24000] 台湾节点
       http://user:pass@0.0.0.0:24000
   [24001] 香港节点
       http://user:pass@0.0.0.0:24001
   [24002] 美国节点
       http://user:pass@0.0.0.0:24002
═══════════════════════════════════════════════════════════════
```

**适用场景：** 需要指定特定节点、测试节点性能

**使用方式：** 每个节点有独立的代理地址，可精确选择

### 节点配置

**方式 1: 使用订阅链接（推荐）**

支持从订阅链接自动获取节点，支持多种格式：

```yaml
subscriptions:
  - "https://example.com/subscribe/v2ray"
  - "https://example.com/subscribe/clash"
```

支持的订阅格式：
- **Base64 编码**: V2Ray 标准订阅格式
- **Clash YAML**: Clash 配置文件格式
- **纯文本**: 每行一个节点 URI

**方式 2: 使用节点文件**

在 `config.yaml` 中指定：

```yaml
nodes_file: nodes.txt
```

`nodes.txt` 每行一个节点 URI：

```
vless://uuid@server:443?security=reality&sni=example.com#节点名称
hysteria2://password@server:443?sni=example.com#HY2节点
ss://base64@server:8388#SS节点
trojan://password@server:443?sni=example.com#Trojan节点
vmess://base64...#VMess节点
```

**方式 3: 直接在配置文件中**

```yaml
nodes:
  - uri: "vless://uuid@server:443#节点1"
  - name: custom-name
    uri: "ss://base64@server:8388"
    port: 24001  # 可选，手动指定端口
```

> **提示**: 可以同时使用多种方式，节点会自动合并。

## 支持的协议

| 协议 | URI 格式 | 特性 |
|------|----------|------|
| VMess | `vmess://` | WebSocket、HTTP/2、gRPC、TLS |
| VLESS | `vless://` | Reality、XTLS-Vision、多传输层 |
| Hysteria2 | `hysteria2://` | 带宽控制、混淆 |
| Shadowsocks | `ss://` | 多加密方式 |
| Trojan | `trojan://` | TLS、多传输层 |

### VMess 参数

VMess 支持两种 URI 格式：

**格式一：Base64 JSON（标准格式）**
```
vmess://base64({"v":"2","ps":"名称","add":"server","port":443,"id":"uuid","aid":0,"scy":"auto","net":"ws","type":"","host":"example.com","path":"/path","tls":"tls","sni":"example.com"})
```

**格式二：URL 格式**
```
vmess://uuid@server:port?encryption=auto&security=tls&sni=example.com&type=ws&host=example.com&path=/path#名称
```

- `net/type`: tcp, ws, h2, grpc
- `tls/security`: tls 或空
- `scy/encryption`: auto, aes-128-gcm, chacha20-poly1305 等

### VLESS 参数

```
vless://uuid@server:port?encryption=none&security=reality&sni=example.com&fp=chrome&pbk=xxx&sid=xxx&type=tcp&flow=xtls-rprx-vision#名称
```

- `security`: none, tls, reality
- `type`: tcp, ws, http, grpc, httpupgrade
- `flow`: xtls-rprx-vision (仅 TCP)
- `fp`: 指纹 (chrome, firefox, safari 等)

### Hysteria2 参数

```
hysteria2://password@server:port?sni=example.com&insecure=0&obfs=salamander&obfs-password=xxx#名称
```

- `upMbps` / `downMbps`: 带宽限制
- `obfs`: 混淆类型
- `obfs-password`: 混淆密码

## Web 监控面板

访问 `http://localhost:9090` 查看：

- 节点状态（健康/警告/异常/拉黑）
- 实时延迟
- 活跃连接数
- 失败次数统计
- 手动探测延迟
- 解除节点拉黑
- **一键导出节点**: 导出所有可用节点的代理池 URI（格式：`http://user:pass@host:port`）

### 健康检查机制

程序启动时会自动对所有节点进行健康检查，之后定期检查：

- **初始检查**: 启动后立即检测所有节点的连通性
- **定期检查**: 每 5 分钟检查一次所有节点状态
- **智能过滤**: 不可用节点自动从 WebUI 和导出列表中隐藏
- **探测方式**: 通过代理请求 HTTP URL，要求5秒内成功返回响应
- **探测目标**: 通过 `management.probe_target` 配置（默认 `https://api.ipify.org`）

```yaml
management:
  enabled: true
  listen: 0.0.0.0:9090
  probe_target: https://api.ipify.org  # 健康检查探测目标（HTTP URL）
```

### 密码保护

为了保护节点信息安全，可以为 WebUI 设置访问密码：

```yaml
management:
  enabled: true
  listen: 0.0.0.0:9090
  password: "your_secure_password"  # 设置 WebUI 访问密码
```

- 如果 `password` 为空或不设置，则无需密码即可访问
- 设置密码后，首次访问会弹出登录界面
- 登录成功后，session 会保存 7 天

## 端口说明

| 端口 | 用途 |
|------|------|
| 2323 | 统一代理入口（节点池模式） |
| 9090 | Web 监控面板 |
| 24000+ | 多端口模式，每节点独立端口 |

## Docker 部署

**方式一：主机网络模式（推荐）**

使用 `network_mode: host` 直接使用主机网络，无需手动映射端口：

```yaml
# docker-compose.yml
services:
  easy-proxies:
    image: ghcr.io/jasonwong1991/easy_proxies:latest
    container_name: easy-proxies
    restart: unless-stopped
    network_mode: host
    volumes:
      - ./config.yaml:/etc/easy-proxies/config.yaml:ro
      - ./nodes.txt:/etc/easy-proxies/nodes.txt:ro
```

> **优点**: 容器直接使用主机网络，所有端口自动对外开放，无需手动配置端口映射。

**方式二：端口映射模式**

手动指定需要映射的端口：

```yaml
# docker-compose.yml
services:
  easy-proxies:
    image: ghcr.io/jasonwong1991/easy_proxies:latest
    container_name: easy-proxies
    restart: unless-stopped
    ports:
      - "2323:2323"       # Pool 模式入口
      - "9091:9091"       # Web 监控面板
      - "24000-24100:24000-24100"  # Multi-port 模式
    volumes:
      - ./config.yaml:/etc/easy-proxies/config.yaml:ro
      - ./nodes.txt:/etc/easy-proxies/nodes.txt:ro
```

> **注意**: 多端口模式需要映射对应的端口范围。如果有 N 个节点，需要开放 `24000` 到 `24000+N-1` 的端口。

## 构建

```bash
# 基础构建
go build -o easy-proxies ./cmd/easy_proxies

# 完整功能构建
go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o easy-proxies ./cmd/easy_proxies
```

## 许可证

MIT License
