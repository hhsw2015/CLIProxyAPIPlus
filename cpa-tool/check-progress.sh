#!/bin/bash
# 查看分类进度

echo "=== 凭证分类进度 ==="
echo ""
echo "活跃凭证: $(ls ~/.cli-proxy-api/*.json 2>/dev/null | wc -l | tr -d ' ')"
echo "无效凭证: $(ls ~/.cli-proxy-api/invalid/*.json 2>/dev/null | wc -l | tr -d ' ')"
echo "限额凭证: $(ls ~/.cli-proxy-api/limit/*.json 2>/dev/null | wc -l | tr -d ' ')"
echo ""
echo "脚本运行状态:"
ps aux | grep "classify-auth.sh" | grep -v grep || echo "  未运行"
echo ""
echo "最新处理的文件:"
tail -5 /tmp/classify-full.log 2>/dev/null || echo "  无日志"
