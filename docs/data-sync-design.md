# CPA -> Sub2API 数据同步设计文档

## 1. 目标

CPA 启动时(commercial 模式)，自动将配置文件中的 Provider/Auth/Model 数据写入 Sub2API 的 PostgreSQL 数据库，使管理面板能展示渠道、分组、模型定价信息。

**原则:**
- 数据源(source of truth): CPA 的 config.yaml
- Sub2API 数据库: CPA 数据的"只读视图" + 计费层
- 通过 Sub2API 的 Go Service 层写入，走完整校验链路
- 幂等: 多次执行结果一致
- 向后兼容: commercial 模式关闭时零影响

---

## 2. 数据模型映射

### 2.1 总览

```
CPA config.yaml                    Sub2API PostgreSQL
---------------------------------------------------------
ClaudeKey[]           ──→  Account (platform=anthropic, type=apikey/bedrock)
GeminiKey[]           ──→  Account (platform=gemini, type=apikey)
CodexKey[]            ──→  Account (platform=openai, type=apikey)
OpenAICompatibility[] ──→  Account (platform=openai/anthropic, type=apikey)
                           (platform 根据 auth-style 和实际用途判断)

按 priority 分桶     ──→  Group
Group 包含 Account    ──→  AccountGroup (M2M)

models.json + alias   ──→  不写入 (Sub2API 自带 LiteLLM 定价库)
```

### 2.2 不同步的 CPA Provider 类型

以下 CPA provider 类型在 Sub2API 中无对应 platform，**暂不同步**:
- KiroKey (AWS CodeWhisperer/Kiro)
- VertexCompatKey (Vertex AI API key)
- 文件型 OAuth: github-copilot, cursor, codebuddy, iflow, kimi, antigravity

这些都是 CPA 独有的文件型 auth，Sub2API 无法管理其凭证生命周期。
未来可扩展，但初期优先同步 API Key 和 Bedrock 类型。

### 2.3 同步的 CPA Provider 类型

| CPA 类型 | Sub2API platform | Sub2API type | 条件 |
|----------|-----------------|--------------|------|
| ClaudeKey (有 api-key, 无 aws-*) | anthropic | apikey | - |
| ClaudeKey (有 aws-access-key-id) | anthropic | bedrock | - |
| GeminiKey | gemini | apikey | - |
| CodexKey | openai | apikey | - |
| OpenAICompatibility (auth-style=anthropic) | anthropic | apikey | TaijiAI Claude 等 |
| OpenAICompatibility (auth-style=bearer 或默认) | openai | apikey | 通用 OpenAI 兼容 |

---

## 3. Account 字段映射

### 3.1 ClaudeKey -> Account

```
ClaudeKey 字段                    Account 字段
-----------------------------------------------------------------
(生成)                         → name: "claude-{hash前6位}"
(无)                           → notes: nil
"anthropic"                    → platform: "anthropic"
(判断)                         → type: 有 aws-access-key-id ? "bedrock" : "apikey"
(构造 JSONB)                   → credentials: 见下文
(无)                           → extra: {"cpa_source":true, "cpa_stable_id":"...", ...}
(无)                           → proxy_id: nil (CPA 代理在 CPA 侧处理)
3                              → concurrency: 3 (默认)
100 - priority                 → priority: 100 - CPA priority (反转!)
1.0                            → rate_multiplier: 1.0 (默认)
!disabled                      → schedulable: !disabled
"active"                       → status: disabled ? "disabled" : "active"
```

#### credentials JSONB 构造

**type=apikey 时:**
```json
{
  "api_key": "<ClaudeKey.APIKey>",
  "base_url": "<ClaudeKey.BaseURL>"  // 仅非默认时填写
}
```

**type=bedrock 时:**
```json
{
  "auth_mode": "sigv4",
  "aws_access_key_id": "<ClaudeKey.AWSAccessKeyID>",
  "aws_secret_access_key": "<ClaudeKey.AWSSecretAccessKey>",
  "aws_region": "<ClaudeKey.AWSRegion>"
}
```

#### extra JSONB (保留 CPA 特有字段)
```json
{
  "cpa_source": true,
  "cpa_stable_id": "<sha256(type:credential_fingerprint)[:16]>",
  "cpa_imported_at": "2026-04-29T12:00:00Z",
  "cpa_provider": "claude-key",
  "excluded_models": ["model-a", "model-b"],
  "prefix": "teamA",
  "auth_style": "x-api-key",
  "error_pass_list": [...],
  "non_retryable_substrings": [...]
}
```

### 3.2 GeminiKey -> Account

```
GeminiKey 字段                    Account 字段
-----------------------------------------------------------------
(生成)                         → name: "gemini-{hash前6位}"
"gemini"                       → platform: "gemini"
"apikey"                       → type: "apikey"
{"api_key": GeminiKey.APIKey}  → credentials
100 - priority                 → priority
```

### 3.3 CodexKey -> Account

```
CodexKey 字段                     Account 字段
-----------------------------------------------------------------
(生成)                         → name: "codex-{hash前6位}"
"openai"                       → platform: "openai"
"apikey"                       → type: "apikey"
{"api_key": CodexKey.APIKey,   → credentials
 "base_url": CodexKey.BaseURL}
100 - priority                 → priority
```

### 3.4 OpenAICompatibility -> Account (每个 api-key-entry 一个 Account)

```
OpenAICompat 字段                 Account 字段
-----------------------------------------------------------------
name + api-key hash            → name: "{Name}-{hash前6位}"
(判断 auth-style)              → platform: auth-style=="anthropic" ? "anthropic" : "openai"
"apikey"                       → type: "apikey"
{"api_key": entry.APIKey,      → credentials
 "base_url": OpenAICompat.BaseURL}
100 - priority                 → priority
extra 中保留 name/endpoint-path/responses-format 等
```

---

## 4. Group 分组策略

### 4.1 分组规则

按 **CPA provider 类型 + platform + priority 区间** 分组:

```
分组名称生成规则:
  "CPA-{platform}-{provider_type}-P{priority_bucket}"

示例:
  ClaudeKey priority=10 + Bedrock   → "CPA-anthropic-bedrock-P10"
  ClaudeKey priority=9 + API Key    → "CPA-anthropic-apikey-P9"
  ClaudeKey priority=8 + API Key    → "CPA-anthropic-apikey-P8"
  GeminiKey priority=6              → "CPA-gemini-apikey-P6"
  OpenAICompat "TaijiAI" priority=9 → "CPA-anthropic-compat-P9"
  OpenAICompat "Cookie Pool" priority=8 → "CPA-openai-compat-P8"
```

### 4.2 Group 字段映射

```
Account 字段                       Group 字段
-----------------------------------------------------------------
(规则生成)                      → name: "CPA-{platform}-{type}-P{priority}"
(规则生成)                      → description: "Auto-synced from CPA config"
platform                       → platform: "anthropic"/"openai"/"gemini"
1.0                            → rate_multiplier: 1.0
"standard"                     → subscription_type: "standard"
"active"                       → status: "active"
```

### 4.3 AccountGroup 关系

每个 Account 在创建时通过 `GroupIDs` 字段关联到对应的 Group。
一个 Account 只属于一个 Group (与 CPA 的一对多模型一致)。

---

## 5. Priority 转换

**CPA**: 数字越大越优先 (priority=10 比 priority=5 优先)
**Sub2API**: 数字越小越优先 (priority=1 比 priority=50 优先)

转换公式:
```go
func convertPriority(cpaPriority int) int {
    // CPA range: 0~10 (typical), Sub2API default: 50, range: 1~100
    // CPA 10 -> Sub2API 1 (最高)
    // CPA 0  -> Sub2API 50 (默认)
    sub2apiPriority := 50 - (cpaPriority * 5)
    if sub2apiPriority < 1 {
        return 1
    }
    if sub2apiPriority > 100 {
        return 100
    }
    return sub2apiPriority
}
```

---

## 6. Stable ID 与幂等

### 6.1 Stable ID 生成

每个 CPA 凭证生成一个稳定标识，存入 Account.extra.cpa_stable_id:

```go
func stableID(providerType string, credential string) string {
    h := sha256.Sum256([]byte(providerType + ":" + credential))
    return hex.EncodeToString(h[:8]) // 16 字符 hex
}
```

各类型的 credential fingerprint:
- ClaudeKey (apikey): api-key 值
- ClaudeKey (bedrock): aws-access-key-id + ":" + aws-region
- GeminiKey: api-key 值
- CodexKey: api-key 值
- OpenAICompat: name + ":" + base-url + ":" + api-key

### 6.2 Upsert 逻辑

```go
func (s *DataSyncer) SyncAccounts(ctx context.Context, accounts []AccountMapping) error {
    // 1. 查询所有 CPA 来源的 Account
    existing, _, _ := s.adminService.ListAccounts(ctx, 1, 10000,
        "", "", "", "cpa_source", 0, "", "", "")
    existingMap := buildStableIDMap(existing) // extra.cpa_stable_id -> Account

    newIDs := make(map[string]bool)

    for _, mapping := range accounts {
        newIDs[mapping.StableID] = true

        if old, ok := existingMap[mapping.StableID]; ok {
            // 已存在: 比较字段，有变化则 Update
            if needsUpdate(old, mapping) {
                s.adminService.UpdateAccount(ctx, old.ID, mapping.ToUpdateInput())
            }
        } else {
            // 不存在: Create
            s.adminService.CreateAccount(ctx, mapping.ToCreateInput())
        }
    }

    // 2. 数据库有但新配置没有的: 标记 disabled
    for stableID, old := range existingMap {
        if !newIDs[stableID] {
            s.adminService.UpdateAccount(ctx, old.ID, &UpdateAccountInput{
                Status: "disabled",
            })
        }
    }
    return nil
}
```

Group 的 upsert 以 name 做唯一键 (Group 名称在 Sub2API 中有 unique 约束)。

---

## 7. 前置修改: embed.go 导出 Service

### 7.1 修改 Result struct

文件: `/Users/wowdd1/Dev/sub2api/backend/pkg/embed/embed.go`

```go
type Result struct {
    BillingService       *service.BillingService
    BillingCacheService  *service.BillingCacheService
    APIKeyService        *service.APIKeyService
    SubscriptionService  *service.SubscriptionService
    APIKeyAuthMiddleware gin.HandlerFunc
    Cleanup              func()
    // --- 新增 ---
    AdminService         service.AdminService       // Account/Group CRUD
    ChannelService       *service.ChannelService     // Channel/Pricing CRUD
}
```

在 `initWithConfig()` return 时补充赋值:
```go
return &Result{
    ...
    AdminService:   adminService,
    ChannelService: channelService,
}, nil
```

### 7.2 CPA 侧使用

文件: `internal/commercial/bootstrap.go`

```go
func Start(engine *gin.Engine, cfg config.CommercialConfig, cpaConfig *config.Config) func() {
    result, err := sub2apiEmbed.InitFromMap(engine, cfg.Sub2API)
    ...
    // 数据同步
    syncer := NewDataSyncer(result.AdminService, result.ChannelService)
    if err := syncer.Sync(context.Background(), cpaConfig); err != nil {
        log.Printf("[commercial] data sync warning: %v", err)
        // 非致命错误，不阻断启动
    }
    ...
}
```

---

## 8. 正确性保证

### 8.1 第一道防线: Service 层校验

所有写入通过 `AdminService.CreateAccount()` / `CreateGroup()` 进行。
Service 层自带校验:
- Platform 枚举: 非 anthropic/openai/gemini/antigravity 直接报错
- Type 枚举: 非 oauth/apikey/bedrock/setup-token 直接报错
- Group name 唯一性: 重名直接报错
- RateMultiplier > 0: 否则报错
- GroupIDs 指向已存在的 Group: 否则报错
- Channel-Group 排他性: 一个 Group 只能属于一个 Channel

如果 CPA 的数据映射有误，Service 层会返回 error 而不是写入脏数据。

### 8.2 第二道防线: Read-Back 验证

同步完成后，从数据库读回所有 CPA 来源的 Account/Group，逐条与 CPA 配置对比:

```go
func (s *DataSyncer) Verify(ctx context.Context, cpaConfig *config.Config) *SyncReport {
    report := &SyncReport{}

    // Account 数量
    dbAccounts := listCPAAccounts(ctx)
    configEntries := countSyncableEntries(cpaConfig)
    report.AccountCount = CountCheck{
        Expected: configEntries,
        Actual:   len(dbAccounts),
    }

    // 逐条字段比对
    for _, dbAcc := range dbAccounts {
        stableID := dbAcc.Extra["cpa_stable_id"].(string)
        configEntry := findByStableID(cpaConfig, stableID)
        if configEntry == nil {
            report.Orphans = append(report.Orphans, dbAcc.ID)
            continue
        }
        diffs := compareFields(configEntry, dbAcc)
        if len(diffs) > 0 {
            report.Mismatches = append(report.Mismatches, FieldMismatch{
                AccountID: dbAcc.ID,
                StableID:  stableID,
                Diffs:     diffs,
            })
        }
    }

    return report
}
```

### 8.3 第三道防线: Dry-Run 模式

支持 `--dry-run` 模式(或配置项 `commercial.sync-dry-run: true`):
只打印转换结果，不写入数据库。便于人工审查:

```
[DRY-RUN] Would create Group: "CPA-anthropic-bedrock-P10" (platform=anthropic)
[DRY-RUN] Would create Account: "bedrock-a1b2c3" (platform=anthropic, type=bedrock, priority=1, group=CPA-anthropic-bedrock-P10)
[DRY-RUN] Would create Account: "bedrock-d4e5f6" (platform=anthropic, type=bedrock, priority=1, group=CPA-anthropic-bedrock-P10)
...
[DRY-RUN] Summary: 8 groups, 47 accounts (skipped: 547 file-based auth)
```

### 8.4 第四道防线: 功能验证

同步完成后，通过 Sub2API 管理面板验证:
1. 渠道(Accounts)页面: 能看到所有同步的 API 凭证
2. 分组(Groups)页面: 能看到按优先级分好的组
3. 每个 Account 的 platform/type/status 显示正确
4. Account 关联到正确的 Group

---

## 9. Channel/定价策略

**初期不创建 Channel。** 理由:
- Sub2API 自带 LiteLLM 定价库(219 个模型)，开箱即用
- Channel 是可选的覆盖层，不配置时使用默认价格
- 管理员可以后续在 UI 上手动创建 Channel 自定义定价
- 减少初期同步复杂度

未来扩展: 如果需要自动创建 Channel，可以:
- 每个 Group 关联一个同名 Channel
- Channel 的 ModelPricing 留空 (使用 LiteLLM 默认价格)
- 管理员在 UI 上调整

---

## 10. 新增文件

| 文件 | 职责 |
|------|------|
| `internal/commercial/data_syncer.go` | DataSyncer 主逻辑: Sync(), Verify() |
| `internal/commercial/data_mapping.go` | CPA config -> Sub2API input 的转换函数 |
| `internal/commercial/data_mapping_test.go` | 映射逻辑的单元测试 |

## 11. 修改文件

| 文件 | 变更 |
|------|------|
| `sub2api/backend/pkg/embed/embed.go` | Result 新增 AdminService, ChannelService 字段 |
| `internal/commercial/bootstrap.go` | Start() 中调用 DataSyncer.Sync() |
| `internal/config/config.go` | CommercialConfig 新增 SyncDryRun bool 字段 |

---

## 12. 同步触发时机

1. **启动时**: `commercial.Start()` 初始化 Sub2API 后，自动执行 Sync()
2. **配置热更新(未来)**: CPA 的 config watcher 检测到配置变化时，触发增量 Sync()
3. **手动触发(未来)**: Management API 提供 `POST /v0/management/sync` 端点

---

## 13. 风险与应对

| 风险 | 应对 |
|------|------|
| Priority 转换错误导致调度异常 | 单元测试覆盖边界值; Verify() 检查 priority 范围 |
| Platform 映射错误 | 明确的映射规则表; OpenAICompat 通过 auth-style 判断 |
| Credentials JSONB 结构不匹配 | 按 Sub2API handler 的 CreateAccountRequest 构造; Service 层校验 |
| 重复执行产生脏数据 | Stable ID + upsert; Group name unique 约束 |
| CPA 配置中删除的凭证 | 标记 disabled 而非删除, 保护关联的 UsageLog/Subscription |
| 同步失败阻断 CPA 启动 | Sync 错误降级为 warning log, 不阻断 |
| CPA 独有字段丢失 | 保存到 Account.extra JSONB, 管理面板可查看 |
| 文件型 OAuth auth 无法同步 | 明确跳过, 日志打印跳过数量 |
