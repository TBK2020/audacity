#!/usr/bin/env bash
# 在 macOS 上用 Apple 官方 container CLI（github.com/apple/container）
# 构建并运行 labdeck，容器名 haos-dashboard。
#
# 前置：Apple Silicon Mac + `container system start` 已执行过一次。
# 用法：
#   export LABDECK_PASSWORD=面板密码
#   export LABDECK_MASTER_KEY=一段随机长口令     # 需要 WebSSH 时必填
#   ./run-apple-container.sh
set -euo pipefail
cd "$(dirname "$0")"

NAME=haos-dashboard

if ! command -v container >/dev/null; then
  echo "未找到 container CLI。安装见 https://github.com/apple/container/releases" >&2
  exit 1
fi

mkdir -p data
if [ ! -f data/services.yaml ]; then
  cp services.example.yaml data/services.yaml
  echo "已生成 data/services.yaml —— 请编辑填入你的真实服务后重新运行本脚本。"
  exit 0
fi

# 注意：macOS 自带 bash 3.2 在 $VAR 后紧跟多字节字符时会解析出错，
# 因此本脚本所有变量一律用 ${VAR} 花括号形式。
echo "==> 构建镜像 ${NAME} (linux/arm64)"
container build -t "${NAME}" .

# 旧容器存在则移除（container delete 是官方命令名，rm 是别名）
container stop "${NAME}" >/dev/null 2>&1 || true
container delete "${NAME}" >/dev/null 2>&1 || true

RUN_ARGS=(
  --detach
  --name "${NAME}"
  --volume "$(pwd)/data:/data"
  --env "LABDECK_PASSWORD=${LABDECK_PASSWORD:-}"
  --env "LABDECK_MASTER_KEY=${LABDECK_MASTER_KEY:-}"
  --env "LABDECK_TOTP=${LABDECK_TOTP:-}"
  --env "TELEGRAM_BOT_TOKEN=${TELEGRAM_BOT_TOKEN:-}"
  --env "TELEGRAM_CHAT_ID=${TELEGRAM_CHAT_ID:-}"
)

# 新版 CLI 支持 --publish；旧版没有，此时直接用容器自己的 IP 访问
if container run --help 2>/dev/null | grep -q -- --publish; then
  RUN_ARGS+=(--publish 8383:8383)
  PUBLISHED=1
else
  PUBLISHED=0
fi

echo "==> 启动容器 ${NAME}"
container run "${RUN_ARGS[@]}" "${NAME}"

sleep 1
echo
container ls | sed -n '1p;/'"${NAME}"'/p'
echo
if [ "$PUBLISHED" = 1 ]; then
  echo "打开: http://localhost:8383"
else
  echo "此版本 container CLI 不支持端口发布，请用上表 ADDR 列的容器 IP 访问："
  echo "  http://<容器IP>:8383"
fi
echo "日志: container logs -f ${NAME}"
