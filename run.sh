#!/usr/bin/env bash
set -e

# 切换到脚本所在的项目根目录
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"

# 默认将数据目录设置在项目根目录下的 data 目录（支持通过已有环境变量覆盖）
export MY_AI_SUM_DATA_DIR="${MY_AI_SUM_DATA_DIR:-${ROOT_DIR}/data}"

echo "Starting my-ai-sum..."
echo "Data directory: ${MY_AI_SUM_DATA_DIR}"
go run ./cmd/my-ai-sum "$@"
