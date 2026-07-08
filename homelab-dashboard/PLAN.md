# Home Lab Dashboard 项目规划书

> 定位：一个自托管的家庭实验室统一控制面板 —— 集中展示家中所有服务的健康状态，并提供一键跳转（Web UI）与一键连接（SSH/终端）能力。
>
> 视角：本文以高级运维工程师（SRE）的思路编写，覆盖需求分析、竞品调研、架构设计、集成清单、安全设计、技术选型与迭代路线。

---

## 1. 项目目标与边界

### 1.1 核心目标（Must Have）

1. **状态总览**：单页看清所有服务的存活状态（up/down/degraded）、响应延迟、证书有效期、版本信息。
2. **快速访问**：每个服务卡片可一键打开其 Web 界面（自动处理内网/外网/Tailscale 等多地址场景）。
3. **快速运维通道**：对宿主机/虚拟机/容器提供浏览器内 SSH 终端（WebSSH），无需本地终端工具。
4. **深度集成**：不只是"ping 一下"，而是通过各应用的原生 API 拉取业务级指标（如 Jellyfin 正在播放数、qBittorrent 下载速度、Proxmox 节点 CPU）。
5. **告警通知**：服务宕机/恢复、磁盘告急、证书临期时主动推送。

### 1.2 非目标（Out of Scope，第一年内不做）

- 不做全功能监控系统（不重造 Prometheus/Grafana，可与其对接）。
- 不做配置管理/编排（不重造 Ansible/Portainer）。
- 不做日志聚合（可留跳转链接到 Loki/Graylog）。
- 不做多租户 SaaS，目标是单家庭、少量用户的自托管场景。

### 1.3 设计原则

- **Agent 可选，默认无侵入**：核心探测全部走网络（HTTP/TCP/ICMP/API），不强制在被管机器上装 agent；宿主级深度指标（温度、SMART）通过可选轻量 agent 或复用 Glances/Netdata 的 API。
- **单二进制/单容器部署**：homelab 用户最反感复杂依赖，`docker run` 一条命令能起来。
- **配置即代码**：所有服务定义可用 YAML 声明并纳入 git 管理，同时提供 UI 编辑（双向同步）。
- **降级可用**：任何集成挂了不影响面板本身；无外网也能完整工作。

---

## 2. 竞品调研（为什么还要自研）

| 项目 | 长处 | 短板（即我们的机会点） |
|---|---|---|
| Homepage (gethomepage) | 集成 widget 极多、YAML 配置 | 纯只读展示，无 SSH、无历史数据、无告警 |
| Homarr | UI 友好、可拖拽 | 集成深度一般，无终端能力 |
| Dashy | 高度可定制 | 状态检查简单（仅 HTTP ping），项目维护放缓 |
| Uptime Kuma | 探测与告警成熟 | 只是拨测工具，不是"门户"，无业务指标、无跳转组织 |
| Heimdall | 简单直接 | 功能停留在书签墙 |
| Grafana + Prometheus | 指标能力天花板 | 上手门槛高，非"开箱即用门户"，无 SSH |

**结论**：市面上"导航墙"（Homepage/Dashy）与"拨测告警"（Uptime Kuma）与"终端网关"（如 NextTerm/Sshwifty）是三个割裂的品类。本项目的差异化定位 = **导航 + 拨测告警 + 业务指标 + WebSSH 四合一**，即"homelab 的 NOC 值班台"。

---

## 3. 总体架构

```
┌────────────────────────────────────────────────────────────┐
│                     Web 前端 (SPA)                          │
│  React + TypeScript + Tailwind + xterm.js                  │
│  仪表盘 / 服务详情 / 终端 / 告警中心 / 配置管理               │
└──────────────┬─────────────────────────────┬───────────────┘
               │ REST + WebSocket            │ WebSocket (终端流)
┌──────────────▼─────────────────────────────▼───────────────┐
│                    后端核心 (Go 单二进制)                    │
│                                                            │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────────┐   │
│  │ 探测引擎  │ │ 集成适配层│ │ 告警引擎  │ │ SSH 网关      │   │
│  │ HTTP/TCP │ │ 各应用API │ │ 规则+路由 │ │ ssh2+审计     │   │
│  │ ICMP/DNS │ │ Adapter  │ │ 静默/聚合 │ │ 凭据代管      │   │
│  └────┬─────┘ └────┬─────┘ └────┬─────┘ └──────┬───────┘   │
│       └────────────┴───┬───────┴───────────────┘           │
│                 ┌──────▼───────┐                           │
│                 │ 存储层        │  SQLite(配置/告警/审计)     │
│                 │              │  内嵌TSDB或VictoriaMetrics │
│                 └──────────────┘  (状态历史/指标)            │
└────────────────────────────────────────────────────────────┘
        │            │              │                │
   Proxmox API   Docker API    各服务健康端点     SSH 到各主机
```

### 3.1 组件职责

| 组件 | 职责 | 关键设计点 |
|---|---|---|
| **探测引擎** | 通用存活检测：HTTP(S)、TCP、ICMP、DNS、证书检查、任意脚本 | 每个探测独立 goroutine + 超时熔断；抖动去噪（连续 N 次失败才判宕）；支持依赖拓扑（路由器挂了不重复报下游 30 个服务） |
| **集成适配层** | 每个高频应用一个 Adapter，拉业务指标 | 统一接口 `Probe() Status` + `Metrics() []Metric` + `Links() []QuickLink`；插件式注册，社区可贡献 |
| **告警引擎** | 状态变化 → 规则匹配 → 通知路由 | 支持恢复通知、告警聚合（1 分钟内多条合并）、静默窗口（维护模式）、升级策略 |
| **SSH 网关** | 浏览器 ↔ 后端 ↔ 目标机的终端桥接 | 凭据加密存储、会话审计录像、支持跳板链、Docker exec 直连容器 |
| **存储层** | 配置、状态历史、告警记录、审计日志 | 默认 SQLite 零依赖；历史指标默认内嵌环形存储（保留 30 天），可外接 VictoriaMetrics 长存 |

### 3.2 关键数据模型

```yaml
# 一切围绕 "Host" 与 "Service" 两级模型
hosts:
  - id: pve-01
    name: Proxmox 主节点
    address: 10.0.0.10
    ssh: { port: 22, credential: pve-root }   # 引用凭据库，不明文
    tags: [虚拟化, 核心]

services:
  - id: jellyfin
    name: Jellyfin
    host: pve-01                    # 归属主机，用于依赖拓扑
    icon: jellyfin                  # 内置 dashboard-icons 图标库
    urls:
      internal: https://jf.lan.example.com
      external: https://jf.example.com
      tailscale: http://100.x.y.z:8096
    checks:
      - type: http
        url: https://jf.lan.example.com/health
        interval: 30s
    integration:
      type: jellyfin                # 启用深度适配器
      api_key: ${JELLYFIN_API_KEY}  # 支持环境变量/密钥库引用
    group: 媒体
```

---

## 4. 高频 Homelab 应用集成清单（第一梯队按此排优先级）

原则：每个应用支持三层能力 —— **L1 存活检测**（通用探测即可）、**L2 业务指标**（原生 API）、**L3 快捷操作**（跳转/SSH/常用动作）。

### 4.1 虚拟化与宿主平台（P0，homelab 的地基）

| 应用 | 集成方式 | 展示指标 | 快捷能力 |
|---|---|---|---|
| **Proxmox VE** | REST API (`/api2/json`) + API Token | 节点 CPU/内存/存储、VM/LXC 列表及状态、备份任务结果 | 跳转控制台、SSH 到节点、启停 VM（v2） |
| **TrueNAS SCALE** | REST API v2 / WebSocket API | 池健康、SMART 告警、快照/复制任务、温度 | 跳转 UI、SSH |
| **Unraid** | GraphQL API（官方 Connect API） | 阵列状态、奇偶校验、Docker/VM 概览 | 跳转 UI、SSH |
| **Synology DSM** | DSM Web API | 存储池、磁盘健康、套件状态 | 跳转 UI、SSH |
| **ESXi/vCenter** | vSphere API（VIM/SOAP，经 govmomi）；REST vAPI 仅 vCenter 有 | 宿主 CPU/内存、每 VM quickStats（CPU MHz、内存、balloon/swap）、硬件传感器、数据存储用量 | 跳转 UI、SSH（需开启）、启停 VM（v2，免费许可证 API 只读不支持写操作） |

### 4.2 容器与编排（P0）

| 应用 | 集成方式 | 展示指标 | 快捷能力 |
|---|---|---|---|
| **Docker (裸)** | Docker Engine API（socket 代理或 TLS TCP） | 容器列表、状态、重启次数、镜像更新提示 | `docker exec` 进容器终端、查日志 |
| **Portainer** | Portainer API | 多 endpoint 容器/Stack 概览 | 跳转对应 Stack 页面 |
| **k3s/k8s** | kube-apiserver（kubeconfig） | 节点/Pod 健康、Pending/CrashLoop 计数 | 跳转 Dashboard、kubectl exec（v2） |
| **Komodo / Dockge** | 各自 API | Stack 状态 | 跳转 |

### 4.3 网络层（P0，挂了全家瘫痪，探测优先级最高）

| 应用 | 集成方式 | 展示指标 | 快捷能力 |
|---|---|---|---|
| **OPNsense / pfSense** | 官方 REST API | WAN 状态、网关延迟、防火墙告警、更新提示 | 跳转 UI、SSH |
| **OpenWrt** | ubus/LuCI RPC 或 SSH 采集 | WAN、无线客户端数、负载 | 跳转 LuCI、SSH |
| **AdGuard Home** | REST API | 拦截率、QPS、上游延迟 | 跳转 UI、一键暂停拦截 5 分钟 |
| **Pi-hole** | FTL API（v6） | 同上 | 同上 |
| **Nginx Proxy Manager** | REST API | 各 Proxy Host 状态、证书到期日 | 跳转 UI |
| **Traefik / Caddy** | Metrics/Admin API | 路由健康、4xx/5xx 速率 | 跳转 Dashboard |
| **Tailscale** | Local API / 官方 API | 各节点在线状态、DERP 延迟 | 复制节点 IP、经 Tailscale 地址 SSH |
| **WireGuard** | `wg show`（经 SSH/agent） | Peer 握手时间、流量 | — |

### 4.4 存储、备份与同步（P1，重点是"任务是否成功"而非"进程是否活着"）

| 应用 | 集成方式 | 展示指标 |
|---|---|---|
| **Syncthing** | REST API | 同步完成度、离线设备、冲突文件数 |
| **MinIO** | Admin API / Prometheus 端点 | 桶容量、节点健康 |
| **Duplicati / Restic / Kopia** | API 或 healthchecks 回报 | **最近一次备份是否成功、距今多久** —— 备份静默失败是 homelab 最大隐患，需"超过 N 小时无成功备份即告警" |
| **Nextcloud** | `serverinfo` API | 存储用量、后台任务、版本 |
| **Immich** | REST API | 任务队列、库容量、版本更新 |

### 4.5 媒体栈（P1，用户感知度最高）

| 应用 | 集成方式 | 展示指标 |
|---|---|---|
| **Jellyfin / Emby / Plex** | 各自 REST API | 正在播放会话数、转码负载、库统计 |
| **Sonarr / Radarr / Prowlarr / Lidarr** | *arr 通用 API v3 | 队列、缺失、健康检查告警（*arr 自带 health 端点，直接透传） |
| **qBittorrent / Transmission** | Web API / RPC | 上下行速度、活动任务、Tracker 错误 |
| **Navidrome / Audiobookshelf** | REST API | 会话/库统计 |

### 4.6 智能家居与自动化（P1）

| 应用 | 集成方式 | 展示指标 |
|---|---|---|
| **Home Assistant** | REST/WebSocket API + 长效令牌 | 实体总数、不可用实体、可选透传任意 sensor（如 UPS 电量、机柜温度） |
| **Frigate** | HTTP API | 摄像头在线状态、检测 FPS、存储占用 |
| **Node-RED** | Admin API | 流状态 |
| **Mosquitto (MQTT)** | TCP 探测 + `$SYS` 主题订阅 | 连接数、消息速率 |
| **ESPHome** | Dashboard API | 设备在线数 |

### 4.7 开发与工具类（P2）

| 应用 | 集成方式 | 展示指标 |
|---|---|---|
| **Gitea / Forgejo** | REST API | 版本、CI 任务状态 |
| **Vaultwarden** | `/alive` 端点 | 存活 + 证书（密码库必须 HTTPS 健康） |
| **Uptime Kuma** | API/Socket.io | 可反向聚合其已有监控项，避免迁移成本 |
| **Grafana / Prometheus** | Health API / Alertmanager API | 透传 Alertmanager 现有告警到本面板 |
| **Netdata / Glances / Beszel** | REST API | 借用其宿主级指标（CPU 温度、SMART），**替代自研 agent 的捷径** |
| **PVE Backup Server** | API | 备份任务、datastore 用量 |
| **Alist / OpenList** | API | 存储挂载状态 |

> 适配器采用插件式注册（Go interface + 编译期内置 / 后期支持外部脚本适配器），上表 P0 全量 + P1 主要项即可覆盖市面 90% 的 homelab 组合。长尾应用由"通用 HTTP JSON 适配器"兜底：用户配置一个 URL + JSONPath 映射即可自定义指标。

---

## 5. 关键子系统详细设计

### 5.1 探测引擎

- **探测类型**：`http`（状态码/关键字/JSON 断言/响应时间阈值）、`tcp`、`icmp`、`dns`、`tls-cert`（临期天数）、`script`（自定义命令，沙箱执行）、`push`（被动心跳，供 cron 任务/备份脚本回报，类 healthchecks.io）。
- **状态机**：`UP → DEGRADED → DOWN`，加 `MAINTENANCE`（静默）与 `PENDING`（首测）。连续失败阈值与恢复阈值分开配置，防抖。
- **依赖拓扑**：service 可声明 `depends_on: [router, pve-01]`；父节点 DOWN 时子节点标记为 `UNREACHABLE` 且不触发告警风暴——这是 Uptime Kuma 缺失、SRE 视角必须有的能力。
- **调度**：带抖动的定时轮询（避免整点惊群打爆弱路由器），全局并发上限可配。

### 5.2 SSH 网关（本项目的差异化核心，也是最大安全面）

- **技术路径**：前端 xterm.js ↔ WebSocket ↔ 后端 Go（golang.org/x/crypto/ssh）↔ 目标主机。
- **凭据管理**：
  - 凭据库独立加密（AES-256-GCM），主密钥来自启动时环境变量/文件，不落库明文；
  - 支持密码、私钥、SSH Agent 转发三种；**默认推荐面板持有专用低权限密钥**，而非复用 root 密码；
  - 支持每主机独立凭据与凭据引用复用。
- **会话安全**：
  - 打开终端需二次确认（可配置要求重输面板密码/TOTP，即 sudo-mode）；
  - 全量会话审计：记录谁、何时、连了哪台机，可选 asciinema 格式录像回放；
  - 空闲超时自动断开；并发会话数限制。
- **扩展形态**：`docker exec` 进容器、Proxmox VNC/xterm.js 串口台代理（v2）、SFTP 小文件上传下载（v2）。

### 5.3 告警与通知

- **通知渠道**（按国内 homelab 用户习惯排序）：Telegram、Bark（iOS）、企业微信/钉钉机器人、Gotify/ntfy、SMTP 邮件、通用 Webhook、飞书。
- **规则示例**：`服务 DOWN 持续 2 分钟`、`证书 14 天内到期`、`磁盘 > 90%`、`备份 26 小时未上报`、`Proxmox 节点 CPU > 90% 持续 10 分钟`。
- **值班体验**：告警带上下文（最近一次探测原始响应、关联主机的其它服务状态），通知内直接带"打开面板/打开该服务/SSH 过去"的深链。

### 5.4 前端信息架构

1. **总览页（NOC 墙）**：按分组的服务卡片网格；全局健康横幅（"37/40 正常"）；宕机置顶。支持电视/平板挂墙模式（大字号、自动轮播、深色）。
2. **资源 Top 视图**：以"物理节点 → VM/LXC → 容器/服务"的层级树展示实时资源消耗，类 htop 树形模式，详见 5.5。
3. **服务详情页**：状态历史时间线（30 天条带图）、响应时间曲线、业务指标 widget、事件/告警记录、快捷入口（多 URL + SSH）。
4. **主机页**：该主机上的服务列表、宿主指标（借道 Netdata/agent）、终端入口。
5. **告警中心**：当前触发中/历史、静默管理。
6. **网络拓扑视图**（v2）：依赖关系可视化。

### 5.5 资源 Top 视图（"整机 htop"）

> 动机：homelab 的典型形态是"一台 PVE 上跑十几个 VM/LXC，每个里面再套 Docker"。传统面板只回答"服务活没活"，回答不了"是谁把我 CPU 吃满了"。本视图让用户像看 htop 一样看整个机房，但粒度是**应用**而不是进程。

#### 数据来源（分层采集，逐层降级）

| 层级 | 数据来源 | 指标 | 说明 |
|---|---|---|---|
| 物理节点 | Proxmox API `/nodes/{node}/status`；ESXi 经 govmomi 读 `HostSystem.summary.quickStats` | CPU、内存、IO 等待、负载 | 已有 P0/P1 适配器顺带产出，无额外成本 |
| VM / LXC | Proxmox API `/cluster/resources`（一次调用拿全集群）；ESXi 经 govmomi PropertyCollector + ContainerView 一次批量取回所有 VM 的 `summary.quickStats` | 每 guest 的 CPU%、内存、磁盘、netin/netout、运行状态 | **无需在 guest 内装任何东西**，这是本视图的性价比核心。注意单机 ESXi 只有 SOAP VIM API（REST vAPI 仅 vCenter 提供），免费许可证下 API 只读——读指标不受影响 |
| 容器 | Docker Engine API `/containers/.../stats` 或 Podman API | 每容器 CPU%、内存、网络、块 IO | 要求该 VM 的 Docker socket 已接入（P0 适配器已覆盖） |
| 裸进程（可选） | Netdata/Glances API 或轻量 agent | 进程级 top | 仅对未容器化的服务需要，默认不做 |

采集到的三层数据通过 **归属关系（`runs_on`）** 拼成一棵树：容器/服务 → VM/LXC → 物理节点。归属关系尽量自动发现（Proxmox 适配器枚举 guest；Docker endpoint 在配置时声明它属于哪个 VM），发现不了的允许在 YAML 里手工声明。

#### 交互设计

- **树形表格，类 htop tree 模式**：每行 = 一个节点/VM/LXC/服务，列为 CPU、内存、网络、磁盘 IO + 迷你 sparkline；可按任意列排序（排序时树自动打平成全局 Top-N 榜），可折叠/展开层级。
- **双口径切换**：
  - *相对口径*：LXC 用了它被分配的 2 核的 80%；
  - *绝对口径*：折算成物理节点的多少 %——找"全场最吃资源的应用"必须用绝对口径，两种口径一键切换并在 UI 上明确标注，避免误读（这是资源视图最常见的坑）。
- **视角切换器**：同一份数据支持三种分组视角——按物理位置（node → guest → 服务，回答"这台机器上谁在吃资源"）、按业务分组（媒体/网络/存储，回答"媒体栈总共花了我多少资源"）、全局打平 Top-N（回答"现在谁最热"）。
- **超分透视**：每个物理节点显示"已分配 vCPU/内存 vs 物理容量"的超分比，homelab 普遍超分，这是容量规划的第一参考。
- **热行动**：任意行右侧直接挂快捷操作——SSH 到该 VM、`docker exec` 进该容器、跳转 Proxmox 控制台、（v2）重启该 guest。发现问题到处置问题不换页面。
- **刷新与开销**：默认 10s 轮询、页面激活时才采集（Page Visibility API），经 WebSocket 推送增量；Proxmox 侧每周期只有 1 次 `/cluster/resources` 调用 + 每个 Docker endpoint 1 次 stats 批量流，对弱主机无压力。历史回看复用存储层的 30 天指标（降采样到 1 分钟粒度）。

#### 排期归属

- 树形模型与 Proxmox/Docker 两层数据在 **M2**（随 P0 适配器一起落地，视图先出"按物理位置"视角与全局 Top-N）；
- 双口径折算、业务分组视角、超分透视在 **M3**；
- 进程级下钻（agent/Netdata 借道）与 guest 重启等写操作在 **M4**。

---

## 6. 安全设计（面板本身就是家里权限最高的应用，必须最硬）

| 层面 | 措施 |
|---|---|
| 认证 | 本地账号 + TOTP 必选支持；对接 OIDC（Authelia/Authentik/Pocket ID），homelab 已普遍自建 SSO |
| 授权 | 至少两级角色：admin（可 SSH/改配置）与 viewer（只读看板，适合家人/挂墙屏） |
| 传输 | 强制 HTTPS（内置 ACME 或明确文档要求置于反代后）；WebSocket 同源校验 |
| 凭据 | 见 5.2；API key 等敏感配置支持 `${ENV}` 与外部文件引用，导出配置时自动脱敏 |
| 暴露面 | 默认只监听内网；文档强烈建议经 Tailscale/VPN 访问而非公网直暴；登录失败限速 + IP 封禁 |
| 审计 | 登录、配置变更、SSH 会话、告警操作全量审计日志 |
| 供应链 | 单静态二进制 + distroless 镜像，SBOM 随版本发布 |

---

## 7. 技术选型汇总

| 层 | 选型 | 理由 |
|---|---|---|
| 后端语言 | **Go** | 单静态二进制、goroutine 天然适合大量并发探测、x/crypto/ssh 成熟、交叉编译覆盖 ARM（很多人跑在 N100/树莓派上） |
| Web 框架 | chi 或 echo + gorilla/websocket | 轻、稳 |
| 前端 | **React + TypeScript + Vite + Tailwind + shadcn/ui**，终端用 xterm.js，图表用 uPlot（轻量高性能时序） | 生态与贡献者基数最大 |
| 主存储 | SQLite（WAL 模式），经 sqlc/ent 访问 | 零运维依赖 |
| 指标历史 | 自研轻量环形表（SQLite 内），预留 remote-write 到 VictoriaMetrics | 默认简单，进阶可扩 |
| 配置 | YAML 文件 + UI 双向编辑（文件为准，UI 写回） | GitOps 友好 |
| 部署 | Docker 镜像（amd64/arm64）+ docker-compose 示例 + 裸二进制 + systemd unit | 覆盖全部主流玩法 |
| CI/CD | GitHub Actions：lint + 单测 + 集成测试（compose 拉起真实 Jellyfin/AdGuard 等打桩验证适配器）+ goreleaser 发版 | 适配器回归测试是质量生命线 |

---

## 8. 迭代路线（里程碑）

### M1 — MVP（约 4–6 周人力）
- 通用探测引擎（http/tcp/icmp/tls-cert）+ 状态机 + 防抖
- YAML 配置装载、服务卡片总览页、多 URL 跳转
- SQLite 存储 + 30 天状态历史条带图
- Telegram + Webhook 两个通知渠道
- Docker 一键部署、本地账号 + TOTP

**验收标准**：一个 20+ 服务的真实 homelab 全量接入，面板 7 天稳定运行，宕机 2 分钟内收到通知。

### M2 — 深度集成 + WebSSH（+6–8 周）
- SSH 网关（凭据库、审计、sudo-mode 确认）
- P0 适配器：Proxmox、ESXi（govmomi）、Docker、OPNsense/pfSense、AdGuard Home、Tailscale、TrueNAS
- 资源 Top 视图初版（node → VM/LXC → 容器 树形视图 + 全局 Top-N，见 5.5）
- 依赖拓扑与告警抑制、维护模式
- viewer 角色 + 挂墙模式

### M3 — 生态化（+8 周）
- P1 适配器全量（媒体栈、*arr、Home Assistant、备份类 push 心跳）
- 通用 JSON 适配器（JSONPath 自定义）
- 资源 Top 视图完善：绝对/相对双口径折算、业务分组视角、超分透视
- OIDC、Bark/ntfy/钉钉等渠道补全
- 配置导入器：从 Homepage/Uptime Kuma 一键迁移（降低换用成本的关键增长手段）

### M4 — 进阶（按需求投票）
- docker exec / kubectl exec 终端、SFTP、Proxmox 控制台代理
- 网络拓扑可视化、指标 remote-write、移动端 PWA 优化

---

## 9. 风险与对策

| 风险 | 对策 |
|---|---|
| 适配器随上游 API 变更而腐烂 | CI 中用真实容器做适配器集成测试；适配器声明兼容版本范围；社区插件机制分摊维护 |
| SSH 网关安全事故（面板被攻破 = 全家被攻破） | 默认最小暴露、强制 TOTP 于 SSH 功能、审计留痕、安全设计文档先于代码评审 |
| 与 Homepage/Uptime Kuma 正面竞争难获用户 | 差异化打"四合一值班台"+ 提供迁移导入器；先服务好自己的真实机房，用真实场景驱动 |
| 单人项目范围失控 | 严格按 M1–M4 切分，M1 不含 SSH 与深度适配器，先把探测与展示做扎实 |

---

## 10. 下一步行动

1. 为项目建独立仓库（建议名：`labdeck` / `homedock` 之类，与本仓库分离）。
2. 按第 3 节脚手架：Go 后端 + React 前端 monorepo，先落 M1 的探测引擎与数据模型。
3. 用自己家中真实服务清单写第一份 `services.yaml` 作为验收基准。
