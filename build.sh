#!/bin/bash
set -euo pipefail

# 用法:
#   ./build.sh                 # 编译当前平台，输出到 bin/
#   ./build.sh linux           # 交叉编译到 linux/amd64，输出到 bin/linux_amd64/
#   ./build.sh linux arm64     # 交叉编译到 linux/arm64
#   ./build.sh windows         # 交叉编译到 windows/amd64，自动加 .exe

TARGET_OS="${1:-}"
TARGET_ARCH="${2:-amd64}"

BIN_DIR="bin"
SUFFIX=""

if [ -n "$TARGET_OS" ]; then
    export GOOS="$TARGET_OS"
    export GOARCH="$TARGET_ARCH"
    # 交叉编译建议关闭 CGO，避免依赖宿主 C 库
    export CGO_ENABLED=0

    # 分目录输出，避免覆盖本地构建产物
    BIN_DIR="bin/${GOOS}_${GOARCH}"

    if [ "$GOOS" = "windows" ]; then
        SUFFIX=".exe"
    fi
fi

mkdir -p "$BIN_DIR"

go build -o "$BIN_DIR/server$SUFFIX"    ./cmd/server
go build -o "$BIN_DIR/client$SUFFIX"    ./cmd/client
go build -o "$BIN_DIR/dashboard$SUFFIX" ./cmd/dashboard

echo "==> 编译完成: $BIN_DIR ($(go env GOOS)/$(go env GOARCH))"