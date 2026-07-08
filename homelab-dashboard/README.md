# labdeck — Home Lab NOC Dashboard

自托管的家庭实验室值班台：集中展示所有服务的健康状态，快速跳转 Web 界面，（规划中）浏览器内 SSH。项目规划见 [PLAN.md](PLAN.md)。

当前进度：**M1（探测 + 看板 + 告警）已可用**。

## 功能

- 探测类型：`http`（状态码/关键字/慢响应降级）、`tcp`、`icmp`、`tls-cert`（证书临期降级）
- 防抖状态机：连续 N 次失败才判宕机、连续 N 次成功才判恢复，杜绝抖动告警
- 深色优先的 NOC 看板：分组卡片、全局健康横幅、30 天可用性条带、最近事件表，WebSocket 实时刷新（断线自动降级为轮询）
- SQLite 历史存储（默认保留 30 天，自动清理）
- 通知：Telegram、通用 Webhook（宕机/恢复，启动时的首次转正不打扰）
- 可选 HTTP Basic 认证；配置中 `${VAR}` 自动展开环境变量

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

## 路线图

M2：SSH 网关（xterm.js + 凭据库 + 审计）、Proxmox/ESXi/Docker 等深度适配器、资源 Top 视图、依赖拓扑告警抑制。详见 [PLAN.md](PLAN.md)。
