# WARP Pool

Cloudflare WARP 代理池管理工具，支持多实例、唯一 IPv4、负载均衡。

## 功能特性

- **唯一 IPv4 保证**: 启动时自动确保每个实例有不同的 IPv4 出口
- **稳定轮询**: 3 个不同 IPv4 地址稳定轮询，无自动轮换
- **统一代理入口**: SOCKS5 和 HTTP 代理，自动 Round-Robin 负载均衡
- **直连模式**: 支持直接连接各实例端口
- **健康检查**: 自动监控实例状态和 IP 变化

## 快速开始

### 1. 准备 WARP CLI

```bash
# Ubuntu/Debian
curl -fsSL https://pkg.cloudflareclient.com/pubkey.gpg | sudo gpg --yes --dearmor -o /usr/share/keyrings/cloudflare-warp-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/cloudflare-warp-archive-keyring.gpg] https://pkg.cloudflareclient.com/ $(lsb_release -cs) main" | sudo tee /etc/apt/sources.list.d/cloudflare-client.list
sudo apt update && sudo apt install cloudflare-warp
```

### 2. 配置

创建 `config.yaml`:

```yaml
# 实例数量（每个有不同的 IPv4）
pool_size: 3

# WARP 路径
warp_bin: /usr/bin/warp-cli
data_dir: ./data

# 内部端口
socks_base_port: 10001
http_base_port: 11001

# 统一代理入口
proxy:
  socks_port: 1080
  http_port: 8118

# 管理 API
api:
  port: 9090
  token: ""

# 健康检查间隔
health_check_interval: 30

# 唯一 IPv4 模式（启动时自动确保）
unique_ipv4:
  enabled: true
  max_retries: 15
  retry_delay: 5

# 自动轮换（关闭以保持稳定）
rotation:
  enabled: false

# 直连模式
direct:
  enabled: true
  expose_external: true
```

### 3. 运行

```bash
# 编译
go build -o warp-pool .

# 运行
./warp-pool -config config.yaml
```

启动日志示例：
```
Starting warp-pool with 3 instances
[unique-ipv4] Target: 3 unique IPv4 addresses
[unique-ipv4] Attempt 1: Found 2 unique IPv4s
[unique-ipv4] Restarting instance 1...
[unique-ipv4] Success! Got 3 unique IPv4 addresses
Pool ready: 3/3 processes running
Unique IPs: 3
  - 104.28.208.119
  - 104.28.208.123
  - 104.28.240.123
```

## 使用方式

### 统一代理入口（推荐）

请求自动在 3 个不同 IPv4 间轮询：

```bash
# SOCKS5
curl -x socks5://YOUR_HOST:1080 https://example.com

# HTTP
curl -x http://YOUR_HOST:8118 https://example.com

# 环境变量
export https_proxy=socks5://YOUR_HOST:1080
export http_proxy=http://YOUR_HOST:8118
```

### 直连实例端口

固定使用某个实例的 IP：

```bash
# 实例 0
curl -x socks5://YOUR_HOST:10001 https://example.com

# 实例 1
curl -x socks5://YOUR_HOST:10002 https://example.com

# 实例 2
curl -x socks5://YOUR_HOST:10003 https://example.com
```

## 管理 API

| 端点 | 方法 | 说明 |
|------|------|------|
| `/api/status` | GET | 池状态统计 |
| `/api/processes` | GET | 所有实例列表 |
| `/api/direct` | GET | 直连端点列表（含 IP） |
| `/api/restart` | POST | 重启所有实例 |
| `/api/restart/:id` | POST | 重启指定实例 |
| `/api/health` | GET | 健康检查 |

### 示例

```bash
# 查看状态
curl http://YOUR_HOST:9090/api/status

# 查看各实例 IP
curl http://YOUR_HOST:9090/api/direct

# 手动重启实例（换 IP）
curl -X POST http://YOUR_HOST:9090/api/restart/0
```

## IP 说明

- **IPv4 数量有限**: Cloudflare WARP 在每个地区只有 2-3 个 IPv4 出口
- **启动自动去重**: `unique_ipv4.enabled: true` 会自动重启实例直到获得不同 IP
- **稳定运行**: 获得唯一 IP 后不会自动轮换，保持稳定

## 架构

```
                    ┌─────────────────────────────────────┐
                    │           warp-pool                 │
                    │                                     │
   ┌────────┐       │  ┌──────────┐    ┌──────────────┐  │
   │ Client │──────▶│  │ Unified  │───▶│ Load Balancer│  │
   │        │       │  │ Proxy    │    │ (Round Robin)│  │
   └────────┘       │  │ :1080    │    └──────┬───────┘  │
                    │  │ :8118    │           │          │
                    │  └──────────┘           ▼          │
                    │              ┌──────────────────┐  │
                    │              │   WARP Instances │  │
                    │              │  ┌────┐┌────┐┌───┐│  │
                    │              │  │IP-A││IP-B││IP-C│  │
                    │              │  └────┘└────┘└───┘│  │
                    │              └──────────────────┘  │
                    │                       ▲           │
                    │              ┌────────┴────────┐  │
                    │              │  Health Checker │  │
                    │              │  + Unique IPv4  │  │
                    │              └─────────────────┘  │
                    │                                   │
                    │  ┌──────────┐                     │
                    │  │ API :9090│                     │
                    │  └──────────┘                     │
                    └─────────────────────────────────────┘
```

## 与 CLIProxyAPI 集成

在 provider 配置中使用 WARP 代理：

```yaml
providers:
  - name: skywork
    proxy: socks5://WARP_HOST:1080
```

## 构建

```bash
# 本地
go build -o warp-pool .

# Linux
GOOS=linux GOARCH=amd64 go build -o warp-pool-linux .
```

## License

MIT
