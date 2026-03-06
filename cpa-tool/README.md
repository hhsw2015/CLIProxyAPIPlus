# fetchQuota（中文说明）

`fetchQuota` 用于批量读取本地认证文件（`auth` 目录），请求对应服务的 quota / usage 接口，并将原始结果写入 SQLite 数据库。

当前支持的 provider：
- `codex`
- `antigravity`
- `gemini-cli`

## 1. 环境要求

- Python 3.10+
- 无第三方依赖（`requirements.txt` 已说明）

## 2. 目录结构

```text
fetchQuota/
  auth/                 # 默认认证文件目录
  db/
    quota_results.db    # 默认数据库
  config.py             # CLI 参数与常量
  quota_service.py      # 核心逻辑（请求、判定、入库）
  start.py              # 启动入口
```

## 3. 快速使用

在项目根目录执行：

```bash
python3 start.py
```

默认行为：
- 读取 `./auth` 下所有文件（不递归）
- 请求 quota 接口
- 写入 `./db/quota_results.db`

### 可选参数

```bash
python3 start.py \
  --auth-dir ./auth \
  --db ./db/quota_results.db \
  --recursive \
  --timeout 20
```

参数说明：
- `--auth-dir`：认证文件目录
- `--db`：SQLite 数据库路径
- `--recursive`：递归扫描子目录
- `--timeout`：HTTP 超时秒数（默认 20）

### 每次运行后自动生成的删除脚本

程序每次执行完成后，会在项目根目录生成脚本：

- `./delete/{鉴权文件夹名}/delete_is_normal_1.sh`：删除 `is_normal = 1` 的文件（状态码 200 但结构异常）
- `./delete/{鉴权文件夹名}/delete_is_normal_2.sh`：删除 `is_normal = 2` 的文件（状态码异常）

脚本会先 `cd` 到鉴权目录，然后执行 `sudo rm -f -- <文件名>`（仅文件名，不含父路径）。执行前建议先打开确认。

## 4. 数据库设计

数据库表：`quota_results`

建表语句（当前版本）：

```sql
CREATE TABLE IF NOT EXISTS quota_results (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    file_name TEXT NOT NULL,
    file_path TEXT NOT NULL,
    provider TEXT NOT NULL,
    fetched_at TEXT NOT NULL,
    success INTEGER NOT NULL,
    result_json TEXT NOT NULL,
    is_normal INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_quota_results_provider ON quota_results (provider);
CREATE UNIQUE INDEX IF NOT EXISTS idx_quota_results_file_name_unique ON quota_results (file_name);
```

### 字段含义

- `id`：自增主键
- `file_name`：认证文件名（唯一）
- `file_path`：认证文件绝对路径
- `provider`：识别出的服务提供方（如 `codex`）
- `fetched_at`：抓取时间（UTC，ISO8601）
- `success`：抓取是否成功（1=成功，0=失败/跳过）
- `result_json`：完整原始结果 JSON（字符串形式）
- `is_normal`：结果健康状态码
  - `0`：正常（状态码 200 且结构正常，为team号）
  - `1`：状态码 200，但结构异常，不是team号
  - `2`：状态码异常（非 200 或缺失，基本上就是封号了）

> 说明：目前“结构校验”按 `codex` 的预期结构进行严格判定；`antigravity` / `gemini-cli` 目前主要依据状态码判断。

## 5. 入库规则

- 按 `file_name` 做唯一键 Upsert：
  - 若同名文件重复抓取，会更新该行，而不是新增。
- `result_json` 保存完整返回（便于后续追溯和二次分析）。

## 6. 常用查询

查看状态分布：

```sql
SELECT is_normal, COUNT(*) AS cnt
FROM quota_results
GROUP BY is_normal
ORDER BY is_normal;
```

查看异常记录（结构异常或状态码异常）：

```sql
SELECT id, file_name, provider, fetched_at, is_normal,
       json_extract(result_json, '$.quota.status_code') AS status_code
FROM quota_results
WHERE is_normal <> 0
ORDER BY fetched_at DESC;
```

只看状态码异常：

```sql
SELECT id, file_name,
       json_extract(result_json, '$.quota.status_code') AS status_code
FROM quota_results
WHERE is_normal = 2;
```

## 7. 注意事项

- 当前代码不包含自动迁移逻辑，请确保目标库的 `quota_results` 表已包含 `is_normal` 字段。
- 认证文件必须是可解析 JSON；否则会记录为失败。
- 网络异常、鉴权失败等会进入 `success=0`，并在 `result_json` 中保留错误信息。
