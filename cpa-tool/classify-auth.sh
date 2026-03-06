#!/bin/bash
# 凭证配额检测工具 - 并发版

AUTH_DIR="${AUTH_DIR:-$HOME/.cli-proxy-api}"
API_URL="${API_URL:-http://localhost:8317}"
API_KEY="${API_KEY:-your-api-key-1}"
DRY_RUN="${DRY_RUN:-false}"
MAX_CHECK="${MAX_CHECK:-50}"
PARALLEL="${PARALLEL:-10}"

LIMIT_DIR="$AUTH_DIR/limit"
mkdir -p "$LIMIT_DIR"

echo "=== 凭证配额检测（并发版）==="
echo "最大检测: $MAX_CHECK 个 | 并发数: $PARALLEL"
echo ""

# 临时文件
TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

# 检测单个凭证的函数
check_credential() {
    local file=$1
    local filename=$(basename "$file")
    local email=$(jq -r '.email // ""' "$file" 2>/dev/null)
    local type=$(jq -r '.type // ""' "$file" 2>/dev/null)

    [[ "$type" != "codex" && "$type" != "openai" ]] && return

    local http_code=$(curl -s -m 15 -w "%{http_code}" -o /dev/null "$API_URL/v1/chat/completions" \
        -H "Authorization: Bearer $API_KEY" \
        -H "Content-Type: application/json" \
        -H "X-Codex-Email: $email" \
        -d '{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"max_tokens":1}')

    if [ "$http_code" = "429" ] || [ "$http_code" = "000" ]; then
        echo "❌ $filename: $http_code"
        echo "$file" >> "$TEMP_DIR/to_archive"
    elif [ "$http_code" = "200" ]; then
        echo "✅ $filename: 正常"
        echo "ok" >> "$TEMP_DIR/valid"
    else
        echo "⚠️  $filename: $http_code"
    fi
}

export -f check_credential
export API_URL API_KEY TEMP_DIR

# 收集文件列表
count=0
files=()
for file in "$AUTH_DIR"/*.json; do
    [ -f "$file" ] || continue
    [ $count -ge $MAX_CHECK ] && break
    files+=("$file")
    count=$((count + 1))
done

# 并发执行
printf '%s\n' "${files[@]}" | xargs -P $PARALLEL -I {} bash -c 'check_credential "$@"' _ {}

# 统计结果
limit=$(wc -l < "$TEMP_DIR/to_archive" 2>/dev/null || echo 0)
valid=$(wc -l < "$TEMP_DIR/valid" 2>/dev/null || echo 0)

echo ""
echo "检测: $count | 正常: $valid | 限额: $limit"

# 执行归档
if [ "$DRY_RUN" = "false" ] && [ -f "$TEMP_DIR/to_archive" ]; then
    while read file; do
        mv "$file" "$LIMIT_DIR/"
    done < "$TEMP_DIR/to_archive"
    echo "已归档 $limit 个凭证"
else
    [ "$DRY_RUN" = "true" ] && echo "干运行模式 - 未移动文件"
fi
