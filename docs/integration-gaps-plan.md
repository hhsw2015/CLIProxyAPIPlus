# CPA x Sub2API 集成缺口修复计划

## 问题总览

经过 4 轮 review，确认数据同步逻辑正确，但 CPA+Sub2API 的整体集成存在以下缺口:

| 缺口 | 严重度 | 现状 | 影响 |
|------|--------|------|------|
| Auth 中间件未挂载到代理路由 | CRITICAL | Sub2API 的 APIKeyAuth 没有拦截代理请求 | 用户鉴权/余额检查/订阅限额全部失效 |
| 无 UsageLog 记录 | HIGH | BillingPlugin 只扣余额，不写使用日志 | 管理面板"使用记录"为空 |
| Group rate_multiplier 不生效 | MEDIUM | BillingPlugin 硬编码 1.0 | 不同分组无法差异化定价 |
| Account 状态快照过时 | MEDIUM | 同步时写入，运行时不更新 | 管理面板看到的 auth 状态可能不准 |
| 配置变更不实时同步 | LOW | 只在启动时同步 | 热更新配置后需重启才能反映到面板 |

---

## P0: Auth 中间件挂载 (CRITICAL)

### 问题

`WrapAuthMiddleware(result.APIKeyAuthMiddleware)` 创建了但没注入到 CPA 的代理路由。
导致: userID=0 -> BillingPlugin 直接 return -> 零计费。

### 方案

在 CPA 的代理路由组上，插入 Sub2API 的 auth 中间件作为**可选层**:

```
代理请求进入
    |
    v
[Sub2API APIKeyAuthMiddleware] -- 验证 sk-xxx, 检查余额/订阅/额度
    |                               如果 commercial 未启用，跳过
    v
[WrapAuthMiddleware] -- 复制 AuthSubject 到 request context
    |
    v
[CPA 原有 AuthMiddleware] -- 验证 CPA 自己的 api-keys (如果配了)
    |
    v
CPA 代理处理
```

### 关键设计决策

**CPA 的 api-keys 和 Sub2API 的 APIKey 如何共存？**

两种模式:
- **纯 CPA 模式** (commercial.enabled=false): 只用 CPA 的 api-keys 鉴权
- **商业模式** (commercial.enabled=true): Sub2API 的 APIKey 鉴权优先。CPA 的 api-keys 作为"管理员直通"密钥(不走计费)

实现: server.go 中根据 commercial layer 是否存在，决定中间件链:

```go
// server.go 代理路由组
proxyGroup := engine.Group("/v1")

if commercialLayer != nil && commercialLayer.AuthMiddleware() != nil {
    // 商业模式: Sub2API 鉴权 -> 计费
    proxyGroup.Use(commercialLayer.AuthMiddleware())
} else if len(cfg.APIKeys) > 0 {
    // 纯 CPA 模式: CPA 自己的 api-keys
    proxyGroup.Use(AuthMiddleware(accessManager))
}
```

### 修改文件

| 文件 | 变更 |
|------|------|
| `internal/api/server.go` | 代理路由组挂载 commercial auth 中间件 |
| `internal/commercial/bootstrap.go` | Start() 返回的 Layer 已有 AuthMiddleware()，无需改 |
| `internal/cmd/run.go` | 传 commercialLayer 到 server 构建流程 |

### 验证

1. 商业模式: 用 Sub2API 发放的 sk-xxx 发请求 -> 鉴权通过 -> BillingPlugin 收到 userID
2. 纯 CPA 模式: 用 CPA config 的 api-keys 发请求 -> 正常代理
3. 商业模式 + 无效 key: 返回 401

---

## P1: UsageLog 记录 (HIGH)

### 问题

BillingPlugin.HandleUsage 只调用 CalculateCost + QueueDeductBalance。
不创建 UsageLog 条目。管理面板"使用记录"页面为空。

### 方案

BillingPlugin 增加 UsageLog 写入:

```go
func (p *BillingPlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
    // ... 现有的 CalculateCost + QueueDeductBalance ...

    // 新增: 写 UsageLog
    p.usageSvc.Create(ctx, &CreateUsageLogInput{
        UserID:       userID,
        APIKeyID:     apiKeyID,   // 从 ctx 取
        AccountID:    0,          // CPA 内部 auth ID，Sub2API 无对应
        Model:        record.Model,
        InputTokens:  record.Detail.InputTokens,
        OutputTokens: record.Detail.OutputTokens,
        CacheReadTokens: record.Detail.CachedTokens,
        TotalCost:    cost.TotalCost,
        ActualCost:   cost.ActualCost,
        RateMultiplier: rateMultiplier,
        Stream:       record.Stream,
        DurationMS:   record.DurationMS,
    })
}
```

### 需要解决

- UsageLog 需要 `account_id` (FK 到 accounts 表)，但 CPA 的 auth ID 和 Sub2API 的 account ID 不是一个体系
- 方案 A: account_id 填 0 或 null (如果 DB 允许)
- 方案 B: 通过 stable_id 反查 Sub2API 的 account ID (有性能开销)
- 方案 C: 修改 UsageLog 允许 account_id 为 optional

### 修改文件

| 文件 | 变更 |
|------|------|
| `internal/commercial/billing_plugin.go` | 增加 UsageService 依赖，写 UsageLog |
| `sub2api/pkg/embed/embed.go` | Result 导出 UsageService |
| `sub2api/pkg/types/types.go` | 导出 UsageService 和相关类型 |
| `sub2api/ent/schema/usage_log.go` | 检查 account_id 是否可以为 0/null |

### 验证

1. 发一个代理请求 -> 管理面板"使用记录"出现一条记录
2. 记录包含: 模型名、token 数、成本、用户 ID、时间
3. 不影响请求延迟(异步写入)

---

## P2: Group rate_multiplier 传递 (MEDIUM)

### 问题

BillingPlugin 从 context 读 rate_multiplier，但 SetRateMultiplier 从未被调用，硬编码 1.0。

### 方案

在 auth 中间件 (WrapAuthMiddleware) 中，查到用户的 Group 后，把 rate_multiplier 写入 context:

```go
func WrapAuthMiddleware(inner gin.HandlerFunc) gin.HandlerFunc {
    return func(c *gin.Context) {
        inner(c)
        subject := middleware.GetAuthSubjectFromContext(c)
        if subject != nil {
            ctx := c.Request.Context()
            ctx = SetUserID(ctx, subject.UserID)
            ctx = SetRateMultiplier(ctx, subject.GroupRateMultiplier)
            c.Request = c.Request.WithContext(ctx)
        }
    }
}
```

### 需要确认

- AuthSubject 是否包含 GroupRateMultiplier 字段
- 如果不包含，需要在 auth 中间件中额外查询

### 修改文件

| 文件 | 变更 |
|------|------|
| `internal/commercial/middleware.go` | WrapAuthMiddleware 设置 rate_multiplier |

---

## P3: Account 状态实时回写 (MEDIUM)

### 问题

Sub2API 数据库中的 Account 状态是启动时的快照。CPA 运行时的 auth 状态变化(rate-limited, cooldown, quota-exceeded)不会反映到 PG。

### 方案

**方案 A: 定期批量回写 (推荐)**

每 60 秒，DataSyncer 读 CPA 的 auth manager 状态，批量 UPDATE Sub2API 中对应 Account 的状态字段:

```go
func (s *DataSyncer) SyncStatus(ctx context.Context, authManager AuthManager) {
    for _, auth := range authManager.ListAll() {
        stableID := computeStableID(auth)
        account := lookupByStableID(stableID)
        if account == nil { continue }

        newStatus := mapCPAStatusToSub2API(auth.Status)
        if account.Status != newStatus {
            s.adminSvc.UpdateAccount(ctx, account.ID, &UpdateAccountInput{
                Status: newStatus,
            })
        }
    }
}
```

**方案 B: 事件驱动**

CPA auth 状态变化时发事件，DataSyncer 订阅事件并实时更新 PG。更精确但更复杂。

### 状态映射

| CPA Auth Status | Sub2API Account Status |
|----------------|----------------------|
| active | active |
| disabled | disabled |
| error / quota_exceeded | error |
| pending / refreshing | active (不影响) |

### 修改文件

| 文件 | 变更 |
|------|------|
| `internal/commercial/data_syncer.go` | 新增 SyncStatus() 方法 |
| `internal/commercial/bootstrap.go` | 启动定时器调用 SyncStatus |

---

## P4: 配置热更新同步 (LOW)

### 问题

CPA 配置热更新后，Sub2API 数据库不同步。

### 方案

CPA 的 config watcher 已有回调机制。在配置重载回调中触发 DataSyncer.Sync():

```go
watcher.OnConfigReload(func(newCfg *config.Config) {
    syncer.Sync(ctx, newCfg)
})
```

### 修改文件

| 文件 | 变更 |
|------|------|
| `internal/commercial/bootstrap.go` | 注册配置重载回调 |

---

## 实施顺序

```
P0 (auth 中间件)     ← 必须先做，其他都依赖它
    |
    v
P1 (UsageLog)       ← P0 完成后才有 userID 可用
    |
    v
P2 (rate_multiplier) ← 可以和 P1 并行
    |
    v
P3 (状态回写)        ← 独立，任何时候可做
P4 (热更新同步)      ← 独立，任何时候可做
```

### 工作量估算

| 阶段 | 预估 | 风险 |
|------|------|------|
| P0 | 中等 (改 server.go 路由中间件链) | 需要处理"CPA api-keys 和 Sub2API APIKey 共存"的鉴权逻辑 |
| P1 | 中等 (BillingPlugin + UsageLog) | account_id 映射问题需要确认 DB schema 是否允许 null |
| P2 | 低 (middleware 一行改动) | 需确认 AuthSubject 是否已有 rate_multiplier |
| P3 | 中等 (定时器 + 状态映射) | 需要访问 CPA 的 auth manager 内部状态 |
| P4 | 低 (注册回调) | 需要确认 watcher 回调接口 |
