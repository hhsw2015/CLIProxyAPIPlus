# CPA x Sub2API 集成架构文档

## 概述

CPA (代理层) 和 Sub2API (商业层) 通过 Go build tag 整合为单一二进制。用户感知为一个系统，底层由六层胶水连接。

## 架构图

```
                    用户浏览器
                        |
                   http://host:8318
                        |
            +-----------+-----------+
            |                       |
    Sub2API 用户面板 (/)       CPA 管理面板 (/management.html)
    (Vue SPA)                  (React SPA)
            |                       |
            +--- SSO (JWT 互信) ----+
            |                       |
    Sub2API Auth Middleware    CPA Management Middleware
    (APIKey → JWT → User)     (Password 或 JWT)
            |                       |
            +--- gin.Engine (共享) --+
            |
    +-------+--------+--------+
    |        |        |        |
   /v1    /v1beta  /gpt-proxy  /admin (Sub2API)
    |
    proxyAuthMiddleware()
    |
    +--- 商业模式: Sub2API APIKeyAuth → 验证余额/订阅/限额
    +--- 纯CPA模式: CPA AuthMiddleware → 验证 api-keys
    |
    CPA 代理路由 → 上游 AI API
    |
    BillingPlugin.HandleUsage()
    |
    UsageService.Create() → PG (UsageLog + 扣费)
```

## 六层胶水

### 1. 进程整合 (build tag)

- `//go:build commercial` 控制编译
- `go.mod replace` 指向本地 sub2api/backend
- 非 commercial 构建: stub 文件提供空实现，零依赖

**文件:**
- `internal/commercial/bootstrap.go` (commercial)
- `internal/commercial/bootstrap_stub.go` (!commercial)

### 2. 数据同步 (DataSyncer)

CPA 启动时将 config.yaml 中的凭证/分组信息写入 Sub2API 的 PG 数据库。

```
CPA config.yaml → DataSyncer → PG (accounts + groups + channels + account_groups)
```

**映射规则:**
- ClaudeKey → Account (platform=anthropic, type=apikey/bedrock)
- GeminiKey → Account (platform=gemini, type=apikey)
- CodexKey → Account (platform=openai, type=apikey)
- OpenAICompatibility → Account (platform 由 auth-style 决定)
- 按 platform+type+priority 自动分组 → Group
- 按 platform 自动创建 → Channel (CPA-Claude / CPA-OpenAI / CPA-Gemini)

**幂等:** stable ID (sha256 of credential fingerprint) 存入 Account.extra.cpa_stable_id
**定时:** 每 5 分钟重新读取 config 文件并增量同步

**文件:**
- `internal/commercial/data_mapping.go` - 转换逻辑
- `internal/commercial/data_syncer.go` - Sync/SyncAuthStatus
- `internal/commercial/data_mapping_test.go` - 28 个测试

### 3. 认证桥接 (proxyAuthMiddleware)

代理路由根据模式动态选择认证中间件:

```go
func (s *Server) proxyAuthMiddleware() gin.HandlerFunc {
    if s.commercialAuthRef != nil && *s.commercialAuthRef != nil {
        (*s.commercialAuthRef)(c)  // Sub2API APIKeyAuth
    } else {
        cpaAuth(c)                 // CPA api-keys
    }
}
```

**认证优先级:**
1. CPA api-keys (配置文件中的 `api-keys`) → 直通，不走计费。和商业层关闭时行为完全一致。
2. Sub2API APIKey (用户注册后获取的 `sk-xxx`) → 商业层鉴权 + 计费。
3. 都不匹配 → 401

**向后兼容:** 商业层开/关对 CPA admin api-key 用户零影响。CPA api-key 永远走第一条路径，商业层代码不被触碰。

**指针延迟初始化:** commercialAuth 变量在 routerConfigurator 中赋值(路由注册之后、HTTP 监听之前)。proxyAuthMiddleware 在请求时解引用，保证值已设置。

**覆盖路由:** /v1, /v1beta, /backend-api/codex, /gpt-proxy, /kling, /suno, WebSocket, Amp

**文件:**
- `internal/api/server.go` - proxyAuthMiddleware, WithCommercialAuthRef
- `internal/cmd/run.go` - 指针变量声明和赋值

### 4. 计费桥接 (BillingPlugin)

CPA 每个请求完成后调用 BillingPlugin.HandleUsage:

```
gin context → 读 userID/apiKeyID/rateMultiplier
    → BillingService.CalculateCost(model, tokens, multiplier)
    → UsageService.Create(usageLog + 扣费, 事务原子)
```

**关键:** 商业数据存在 gin context (c.Set) 而非 request context。因为 CPA handler 的 GetContextWithCancel 从 context.Background() 创建新 context，request context 上的值会丢失。gin context 通过 ctx.Value("gin") 传递。

**文件:**
- `internal/commercial/billing_plugin.go`
- `internal/commercial/middleware.go` (WrapAuthMiddleware)

### 4b. 配置变更同步

使用 fsnotify 监听 config.yaml 文件变化(和 CPA 自身的 watcher 相同机制):

```
config.yaml 被修改 → fsnotify 事件 → 2s debounce → DataSyncer.Sync()
```

- 文件没变时: 零 CPU 开销(内核事件驱动)
- 文件变了时: ~2 秒内触发增量同步(新 key 创建、删除的 key disable)
- 不再使用 5 分钟轮询

### 5. 状态回写

每 60 秒从 CPA auth manager 读取运行时状态，更新 PG 中对应 Account 的 status:

```
CPA auth.Status → mapCPAAuthStatus() → Sub2API Account.status
  active          → "active"
  error           → "error"
  disabled        → "disabled"
  unavailable/quota_exceeded → "error"
```

**Provider 匹配:** authToStableID 通过 Attributes 识别 provider 类型，不依赖 provider name 字符串匹配。

### 6. SSO (单点登录)

**Sub2API → CPA 方向:**
- Sub2API 登录后 JWT 存入 localStorage (key: `auth_token`)
- CPA 管理面板读取同一 localStorage → 用 JWT 作为 management key
- CPA 后端 commercialJWTValidator 验证: admin role + user active + token version

**CPA → Sub2API 方向:**
- CPA 管理面板检测商业模式 → 无 JWT 时跳转 `/login?redirect=/management.html`
- Sub2API 登录页支持 redirect 参数 → 登录后跳回 CPA

**管理面板互嵌:**
- Sub2API Admin: "代理设置" tab → iframe /management.html
- CPA 管理面板: "商业管理" tab → iframe Sub2API Admin

**CPA 托管数据只读:**
- Account 的 `extra.cpa_source === true` → 编辑/删除按钮禁用，显示 "CPA 托管" badge
- Group 名称以 `CPA-` 开头 → 同样处理

## 配置

```yaml
commercial:
  enabled: true                    # 启用商业层
  sync-dry-run: false              # true 时只打印不写入
  sub2api:
    database:
      host: localhost
      port: 5432
      user: sub2api
      password: changeme
      dbname: sub2api
    redis:
      addr: localhost:6379
    server:
      port: 8318
    jwt:
      secret: "your-jwt-secret"
    # ... 其他 sub2api 配置
```

## 优先级转换

CPA: 数字越大越优先 (priority=10 最高)
Sub2API: 数字越小越优先 (priority=1 最高)

公式: `sub2api_priority = 50 - (cpa_priority * 5)`, clamp [1, 100]

## 开发环境

### 三个项目

| 项目 | 路径 | 框架 | 说明 |
|------|------|------|------|
| CPA (主项目) | /Users/wowdd1/Dev/CLIProxyAPIPlus | Go + gin | 代理核心 + 商业层胶水 |
| Sub2API | /Users/wowdd1/Dev/sub2api | Go (backend) + Vue (frontend) | 商业层 (用户/计费/支付) |
| CPA 管理面板 | /Users/wowdd1/Dev/Cli-Proxy-API-Management-Center | React + Vite | CPA 管理 UI (单文件 SPA) |

### 依赖关系

```
CPA go.mod → replace github.com/Wei-Shaw/sub2api => /Users/wowdd1/Dev/sub2api/backend
CPA binary → embeds sub2api/backend/internal/web/dist/ (Sub2API 前端编译产物)
CPA /management.html → 从 CPA 管理面板项目编译的 dist/index.html
```

### 编译步骤

```bash
# 1. Sub2API 前端 (Vue) → 输出到 backend/internal/web/dist/
cd /Users/wowdd1/Dev/sub2api/frontend
pnpm install    # 首次
pnpm build      # 每次前端改动后

# 2. CPA 管理面板 (React) → 输出到 dist/index.html (单文件 SPA)
cd /Users/wowdd1/Dev/Cli-Proxy-API-Management-Center
npm install     # 首次
npm run build   # 每次前端改动后
# 产物: dist/index.html → 部署时放到 VPS static/management.html

# 3. CPA 主二进制 (包含 Sub2API 前端 embed)
cd /Users/wowdd1/Dev/CLIProxyAPIPlus
# 本地测试 (Mac):
go build -tags commercial,embed -o cpa-server ./cmd/server/
# 交叉编译 (Linux VPS):
GOOS=linux GOARCH=amd64 go build -tags commercial,embed -o /tmp/cpa-commercial-linux ./cmd/server/

# 纯 CPA 模式 (无商业层):
go build -o cpa-server ./cmd/server/
```

### Build Tag 说明

| Tag | 作用 |
|-----|------|
| `commercial` | 启用商业层代码 (data_mapping, data_syncer, billing_plugin, middleware, bootstrap) |
| `embed` | 启用 Sub2API 前端 embed (嵌入 internal/web/dist/ 到二进制) |
| 无 tag | 纯 CPA 模式，bootstrap_stub.go 提供空实现 |

### 修改后的编译部署流程

**只改了 CPA Go 代码:**
```bash
GOOS=linux GOARCH=amd64 go build -tags commercial,embed -o /tmp/cpa-commercial-linux ./cmd/server/
scp /tmp/cpa-commercial-linux vps:/path/cpa-new-server
# VPS 重启 tmux session
```

**改了 Sub2API 前端:**
```bash
cd sub2api/frontend && pnpm build     # 重新编译前端
cd CLIProxyAPIPlus
GOOS=linux GOARCH=amd64 go build -tags commercial,embed -o /tmp/cpa-commercial-linux ./cmd/server/
# 上传 + 重启
```

**改了 CPA 管理面板:**
```bash
cd Cli-Proxy-API-Management-Center && npm run build
scp dist/index.html vps:/path/static/management.html
# 不需要重启 CPA (静态文件)
```

**改了 Sub2API 后端 (pkg/embed, pkg/types):**
```bash
# Sub2API 后端改动通过 go.mod replace 自动生效
GOOS=linux GOARCH=amd64 go build -tags commercial,embed -o /tmp/cpa-commercial-linux ./cmd/server/
# 上传 + 重启
```

## 部署 (VPS)

```bash
# 编译
GOOS=linux GOARCH=amd64 go build -tags commercial,embed -o /tmp/cpa-commercial-linux ./cmd/server/

# 上传
scp -i ~/Downloads/pikapk3219_vps_key.pem /tmp/cpa-commercial-linux azureuser@4.151.241.30:/tmp/

# 停止旧进程、替换、重启
ssh azureuser@4.151.241.30
  tmux send-keys -t cpa-new C-c
  sleep 2
  cp /tmp/cpa-commercial-linux ~/CLIProxyAPIPlus-new/cpa-new-server
  chmod +x ~/CLIProxyAPIPlus-new/cpa-new-server
  tmux send-keys -t cpa-new "./cpa-new-server -config cpa-new-config.yaml 2>&1 | tee cpa-new.log" Enter
```

需要: PostgreSQL + Redis (systemd 管理)

## 维护注意事项

1. **CPA 配置是 source of truth**: 代理路由/凭证/模型映射从 config.yaml 来，PG 是只读视图
2. **Sub2API 面板修改不反写 CPA**: 在面板上改 CPA 托管的 Account/Group 无效(UI 已禁用)
3. **密码同步**: 需要手动保持 CPA management-password 和 Sub2API admin 密码一致
4. **Sub2API 前端更新**: 改完后需要 `pnpm build`，然后重新编译 CPA 二进制(embed 会打包新前端)
5. **CPA 管理面板更新**: `npm run build` 后 scp dist/index.html 到 VPS (不需要重新编译 Go)
6. **go.mod replace**: CPA 通过绝对路径引用 Sub2API backend，`go mod tidy` 时需确保路径正确
7. **clean cache**: 切换 build tag 组合时可能需要 `go clean -cache` 避免缓存问题
