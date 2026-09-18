#!/usr/bin/env bash
set -e

# 切换到脚本所在的项目根目录
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"

# 默认构建输出路径：Windows 环境下补充 .exe 后缀
DEFAULT_OUTPUT="build/my-ai-sum"
if [[ "$OSTYPE" == "msys" || "$OSTYPE" == "cygwin" || "$OS" == "Windows_NT" ]]; then
  DEFAULT_OUTPUT="build/my-ai-sum.exe"
fi

OUTPUT="${1:-$DEFAULT_OUTPUT}"

mkdir -p "$(dirname "$OUTPUT")"

echo "Tidying go modules..."
go mod tidy

echo "Building $OUTPUT..."
go build -ldflags="-s -w" -o "$OUTPUT" ./cmd/my-ai-sum

echo "Successfully built: $OUTPUT"
