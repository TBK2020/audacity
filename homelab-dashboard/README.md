# labdeck — Home Lab NOC Dashboard

自托管的家庭实验室值班台：集中展示所有服务的健康状态，快速跳转 Web 界面，（规划中）浏览器内 SSH。项目规划见 [PLAN.md](PLAN.md)。

当前进度：**M1（探测 + 看板 + 告警）+ M2 第一部分（WebSSH 网关）已可用**。

## 功能

- 探测类型：`http`（状态码/关键字/慢响应降级）、`tcp`、`icmp`、`tls-cert`（证书临期降级）
- 防抖状态机：连续 N 次失败才判宕机、连续 N 次成功才判恢复，杜绝抖动告警
- 深色优先的 NOC 看板：分组卡片、全局健康横幅、30 天可用性条带、最近事件表，WebSocket 实时刷新（断线自动降级为轮询）
- **浏览器内 SSH 终端**（xterm.js）：服务卡片一键连到所属主机
  - 凭据库 AES-256-GCM 加密存储（主密钥来自 `LABDECK_MASTER_KEY`，不落库）
  - 支持密码与私钥认证；主机指纹首次连接固定（TOFU），变更即拒绝
  - 空闲超时自动断开、并发会话上限、全量会话审计（谁/何时/连哪台/流量/关闭原因）
- SQLite 历史存储（默认保留 30 天，自动清理）
- 通知：Telegram、通用 Webhook（宕机/恢复，启动时的首次转正不打扰）
- 可选 HTTP Basic 认证；配置中 `${VAR}` 自动展开环境变量

## 启用 WebSSH

```bash
# 1. 启动时提供主密钥（务必妥善保管，丢失后凭据库无法解密）
export LABDECK_MASTER_KEY='一段足够长的随机口令'

# 2. 录入凭据（密码或 PEM 私钥）
curl -u admin:$LABDECK_PASSWORD -X POST http://localhost:8383/api/credentials \
  -d '{"id":"pve-root","type":"password","username":"root","secret":"..."}'

# 3. 在配置的 host 上声明 ssh 块（见 services.example.yaml），重启后
#    对应服务卡片出现 "SSH ▸" 按钮
```

安全建议：为面板专门发一把低权限密钥而不是复用 root 密码；面板务必置于 HTTPS 反代或 Tailscale 之后。

## 快速开始

```bash
# 二进制
go build -o labdeck ./cmd/labdeck
cp services.example.yaml services.yaml   # 按需编辑
LABDECK_PASSWORD=changeme ./labdeck -config services.yaml

# 或 Docker
mkdir -p data && cp services.example.yaml data/services.yaml
docker compose up -d
```

打开 `http://<host>:8383`。

## 配置

见 [services.example.yaml](services.example.yaml)，要点：

```yaml
services:
  - id: jellyfin
    name: Jellyfin
    group: 媒体
    urls:                     # 卡片上的快捷跳转，可配 internal/external/tailscale
      internal: http://10.0.0.20:8096
    checks:
      - type: http
        url: http://10.0.0.20:8096/health
        keyword: Healthy      # 响应体必须包含
        degraded_latency: 1s  # 超过则显示"降级"
```

## API

| 端点 | 说明 |
|---|---|
| `GET /api/summary` | 全部服务当前状态 |
| `GET /api/services/{id}/uptime?days=30` | 按日可用性 |
| `GET /api/services/{id}/history?hours=24` | 原始探测记录 |
| `GET /api/events?limit=100` | 状态变更事件 |
| `GET /api/ws` | WebSocket 实时推送 |
| `GET /api/hosts` | 主机列表（含 SSH 可用性） |
| `GET/POST /api/credentials`、`DELETE /api/credentials/{id}` | 加密凭据管理（不回显密文） |
| `GET /api/ssh/{host}/ws` | WebSSH 终端通道 |
| `GET /api/ssh/sessions` | SSH 会话审计 |

## 路线图

M2 剩余：Proxmox/ESXi/Docker 等深度适配器、资源 Top 视图、依赖拓扑告警抑制、终端二次确认（TOTP）。详见 [PLAN.md](PLAN.md)。
