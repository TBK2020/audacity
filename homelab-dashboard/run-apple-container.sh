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

# 自动加载 .env（git 忽略，见 .env.example）——避免密码出现在 shell 历史里
if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

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
# "Timeout waiting for connection to builder"，或干脆无输出卡死）时自动
# 降级为 "golang 镜像内编译 + alpine 直接运行"，完全绕开 BuildKit。
# 构建输出全程实时透传，卡住超过 BUILD_TIMEOUT 秒自动放弃。
IMAGE="${NAME}"
BUILD_MODE=image
BUILD_TIMEOUT="${BUILD_TIMEOUT:-300}"

# macOS 没有 timeout 命令，用后台看门狗实现（bash 3.2 兼容）
run_with_timeout() {
  local secs="$1"; shift
  "$@" &
  local pid=$!
  # 看门狗关闭自己的标准输出/错误，避免孤儿 sleep 占住调用方的管道
  ( sleep "${secs}"; kill "${pid}" 2>/dev/null ) >/dev/null 2>&1 &
  local dog=$!
  local rc=0
  wait "${pid}" || rc=$?
  kill "${dog}" 2>/dev/null
  wait "${dog}" 2>/dev/null || true
  return "${rc}"
}

# 镜像源候选：官方仓库直连在部分网络下极慢，按顺序尝试国内镜像。
# 可用 REGISTRY_MIRROR=你的镜像域名 置顶自定义源；"direct" 表示直连。
CANDIDATES=()
if [ -n "${REGISTRY_MIRROR:-}" ]; then CANDIDATES+=("${REGISTRY_MIRROR}"); fi
CANDIDATES+=("docker.m.daocloud.io" "docker.1panel.live" "docker.1ms.run" "direct")

# find_image library/alpine:3.20 → 设置 FOUND_IMAGE 为第一个能拉动的引用
find_image() {
  local path="$1" m ref
  for m in "${CANDIDATES[@]}"; do
    if [ "${m}" = "direct" ]; then ref="${path#library/}"; else ref="${m}/${path}"; fi
    echo "    尝试镜像源: ${ref}"
    if run_with_timeout 180 container run --rm "${ref}" /bin/true >/dev/null 2>&1; then
      FOUND_IMAGE="${ref}"
      return 0
    fi
  done
  return 1
}

if [ "${FORCE_BUILD:-0}" != 1 ] && [ "$(uname -m)" = "arm64" ] && [ -f prebuilt/labdeck-linux-arm64 ]; then
  # 首选路径：仓库自带预编译静态二进制（前端已内嵌），只需拉 ~3MB 的
  # alpine 基础镜像，完全绕开 builder 和大镜像下载。FORCE_BUILD=1 可强制本地构建。
  echo "==> 使用仓库预编译二进制（跳过镜像构建）"
  BUILD_MODE=binary
  mkdir -p bin
  cp prebuilt/labdeck-linux-arm64 bin/labdeck && chmod +x bin/labdeck
  if ! find_image "library/alpine:3.20"; then
    echo "所有镜像源都无法拉取 alpine 基础镜像，请检查网络或设置 REGISTRY_MIRROR" >&2
    exit 1
  fi
  IMAGE="${FOUND_IMAGE}"
else
  echo "==> 构建镜像 ${NAME} (linux/arm64)，最多等 ${BUILD_TIMEOUT}s，输出实时显示："
  if ! run_with_timeout "${BUILD_TIMEOUT}" container build -t "${NAME}" .; then
    echo "==> container build 失败或超时，重建 builder 后重试一次…"
    container builder delete >/dev/null 2>&1 || true
    if ! run_with_timeout "${BUILD_TIMEOUT}" container build -t "${NAME}" .; then
      echo "==> builder 仍不可用，降级：在 golang 容器内编译二进制（不走 BuildKit）"
      BUILD_MODE=binary
      mkdir -p bin .gocache
      if ! find_image "library/golang:1.24-alpine"; then
        echo "所有镜像源都无法拉取 golang 镜像，请检查网络或设置 REGISTRY_MIRROR" >&2
        exit 1
      fi
      container run --rm \
        --volume "$(pwd):/src" \
        --volume "$(pwd)/.gocache:/go" \
        --env CGO_ENABLED=0 \
        "${FOUND_IMAGE}" \
        sh -c 'cd /src && go build -trimpath -o bin/labdeck ./cmd/labdeck'
      if ! find_image "library/alpine:3.20"; then
        echo "无法拉取 alpine 基础镜像" >&2
        exit 1
      fi
      IMAGE="${FOUND_IMAGE}"
    fi
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
  --env "PVE_TOKEN=${PVE_TOKEN:-}"
  --env "ESXI_PASS=${ESXI_PASS:-}"
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
