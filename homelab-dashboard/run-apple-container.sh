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

# 构建策略：优先 container build；builder VM 起不来（常见报错
# "Timeout waiting for connection to builder"）时自动降级为
# "golang 镜像内编译 + alpine 直接运行"，完全绕开 BuildKit。
IMAGE="${NAME}"
BUILD_MODE=image

try_build() {
  container build -t "${NAME}" . 2>&1
}

echo "==> 构建镜像 ${NAME} (linux/arm64)"
if ! OUT="$(try_build)"; then
  echo "${OUT}" | tail -2
  echo "==> container build 失败，尝试重建 builder 后重试一次…"
  container builder delete >/dev/null 2>&1 || true
  container builder start >/dev/null 2>&1 || true
  if ! OUT="$(try_build)"; then
    echo "${OUT}" | tail -2
    echo "==> builder 仍不可用，降级：在 golang 容器内编译二进制（不走 BuildKit）"
    BUILD_MODE=binary
    mkdir -p bin .gocache
    container run --rm \
      --volume "$(pwd):/src" \
      --volume "$(pwd)/.gocache:/go" \
      --env CGO_ENABLED=0 \
      golang:1.24-alpine \
      sh -c 'cd /src && go build -trimpath -o bin/labdeck ./cmd/labdeck'
    IMAGE="alpine:3.20"
  fi
fi

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
if [ "${BUILD_MODE}" = image ]; then
  container run "${RUN_ARGS[@]}" "${IMAGE}"
else
  # 降级模式：alpine + 挂载编译好的静态二进制（含内嵌前端）
  container run "${RUN_ARGS[@]}" \
    --volume "$(pwd)/bin:/opt/labdeck" \
    "${IMAGE}" \
    /opt/labdeck/labdeck -config /data/services.yaml -db /data/labdeck.db
fi

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
