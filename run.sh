#!/bin/sh
# obpool 每晚运行包装: 切到脚本所在目录, 带时间戳写日志
cd "$(dirname "$0")" || exit 1
{
    echo "===== $(date '+%Y-%m-%d %H:%M:%S') 开始 ====="
    ./obpool-arm64 -subs subs.txt
    echo "===== $(date '+%Y-%m-%d %H:%M:%S') 结束 ====="
} >> run.log 2>&1
