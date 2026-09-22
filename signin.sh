#!/bin/bash
# 批量签到脚本：遍历 auths/ 下所有 workbuddy-*.json 账号
# 用法: ./signin.sh [auths_dir]
#
# 二进制解析：缺失、或任一构建输入（*.go / go.mod / go.sum）比它新时重编——
# 只在"不存在时"编译会让源码改动后本地二进制静默停留在旧版本（issue #191）。
set -e
cd "$(dirname "$0")"

BIN=./signin_bin
if [[ ! -x "$BIN" ]] || find . \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$BIN" -print -quit | grep -q .; then
    if ! command -v go >/dev/null 2>&1; then
        echo "需要 go 构建 signin_bin（或镜像内置 /app/signin_bin）" >&2
        exit 1
    fi
    echo "build signin_bin ..."
    go build -o "$BIN" ./cmd/signin
fi

exec "$BIN" "${1:-auths}"
