# Pool Manager 设计方案

## 目标

在 CLIProxyAPIPlus 本体中增加一个 `PoolManager`，维护一个固定大小、健康概率较高的 `active` 认证池，并将现有请求路由限制在这个池子内。

该方案面向非常大的共享认证池，认证文件规模可能达到 1 万到 10 万以上，并且这些账号可能在本项目之外被其他系统同时使用。设计目标不是获得全量账号的绝对实时真相，而是尽量保证：

- 新请求尽量命中健康账号
- 坏号尽快从可路由集合中移除
- `active` 池数量始终稳定
- 在尽量低的探测成本下，保持整体响应速度稳定

## 评估目标

PoolManager 是否有效，最终必须回到用户可见结果，而不是只看内部池状态。

核心评估指标：

1. 请求成功率

- 沿用现有系统已经提供的：
  - `总请求数`
  - `成功请求`
  - `失败请求`
  - 成功率
- 目标是尽量逼近 `100%`

2. 服务健康监测

- 健康状态应更多显示为绿色、较少显示为红色
- 如果 PoolManager 有效，打到坏号后的重试应减少

3. 响应速度

- 请求不应反复撞到坏号再切换
- 在 `fill-first` 下，前排 active 号应更稳定
- 平均时延和尾部时延都应优于关闭 PoolManager 的情况

所以，PoolManager 的价值不是“多了一套池状态”，而是：

- 成功率更高
- 响应更快、更稳定

## 问题背景

当前系统已经具备这些能力：

- 使用 `fill-first` 或 `round-robin` 进行选号
- 在真实请求失败后更新认证的运行时状态
- 将限额账号移入 `auth-dir/limit`
- 删除无效账号

但在“超大共享号池”场景下，仍然有明显问题：

- 全量把所有账号加载进运行时内存，代价过高
- 某个账号是否健康，可能被外部系统随时改变
- 如果 selector 经常碰到坏号，请求会在重试中变慢

因此，这里要实现的不是“全量账号健康探测系统”，而是一个固定大小、持续维护的健康 Buffer 池。

## 非目标

- 不保证所有账号永远准确健康
- 不改变当前 `auth-dir` 根目录结构
- 不替换现有 `fill-first` / `round-robin` 逻辑
- 第一版不引入数据库作为强依赖
- 当前仓库中的 TUI 不负责展示 PoolManager 指标

## 当前落地状态

截至当前版本，后端已经落地：

- `active / reserve / low_quota / limit` 四类运行时池状态
- `Codex` 低成本探测：`wham/usage`
- `401 -> refresh -> verify`
- 真实请求选中 auth 后的 `in-flight` 保护
- active 摘除后的异步补号
- 无效文件删除 / 限额文件移入 `limit` 的异步文件处理
- `low_quota` 软隔离池
- `/v0/management/usage` 返回 `pool` 指标

当前明确边界：

- `pool` 指标保留在后端接口和日志中
- 当前仓库里的 TUI 不展示 `pool`
- `low_quota` 默认不做后台恢复探测

## 总体思路

新增一个 `PoolManager`，作为运行时扩展层，放在现有 selector 前面。

核心原则：

- 现有认证生命周期逻辑仍然是“最终处置的真相来源”
- `PoolManager` 只负责内存中的 `active / reserve` 池维护
- 真正的删文件、移到 `limit`、refresh 后回写，仍由现有系统统一处理
- selector 只看到 `active` 集合

### 为什么这样设计

这样可以同时满足：

- 原有系统改动最小
- `fill-first` 语义保留
- 新请求只在固定大小的健康 Buffer 池里路由
- 不需要把 10 万个号全部交给运行时 selector

## 配置设计

新增可选顶层配置：

```yaml
pool-manager:
  size: 100
  active-idle-scan-interval-seconds: 1800
  reserve-scan-interval-seconds: 300
  limit-scan-interval-seconds: 21600
  reserve-sample-size: 20
  low-quota-threshold-percent: 20
  provider: "codex"
```

语义：

- `size <= 0`：关闭 PoolManager
- `size > 0`：开启 PoolManager
- 第一版建议只支持 `codex`

默认值建议：

- `active-idle-scan-interval-seconds`: 1800
- `reserve-scan-interval-seconds`: 300
- `limit-scan-interval-seconds`: 21600
- `reserve-sample-size`: 20
- `low-quota-threshold-percent`: 20

## 与现有监控页面的关系

当前系统已经有现成的评估基础，包括：

- 请求使用统计页面
- 失败请求统计
- 服务健康监测

PoolManager 不应引入一套完全脱离现有页面的新指标体系，而应该在现有观测体系上补充 Pool 相关指标。

建议保留两类指标：

1. 请求结果指标

- `total_requests`
- `success_count`
- `failure_count`
- success rate

这是判断 PoolManager 是否真的改善系统效果的主指标。

2. Pool 专属指标

- `pool_active_size`
- `pool_reserve_count`
- `pool_low_quota_count`
- `pool_limit_count`
- `pool_promotions`
- `pool_active_removals`
- `pool_refresh_recoveries`
- `pool_limit_restores`

这是判断 PoolManager 是否真的在工作、以及工作方式是否合理的辅助指标。

## 与现有系统的对接方式

### 1. 候选来源

不修改当前 auth 目录结构。

仍然使用现有的：

- `auth-dir/*.json`
- `auth-dir/limit/*.json`

区别只在于：

- 不是所有账号都发布给运行时路由
- `PoolManager` 只把 `active` 子集暴露给 `coreManager`

### 2. 运行时可见集合

当 `pool-manager.size > 0` 时：

1. `PoolManager` 维护当前 `active` 集合
2. 只将 `active` 集合发布给 `coreManager`
3. 原有 selector 在这个集合中继续执行 `fill-first` 或 `round-robin`

这样现有系统看到的，始终只是一个固定大小的健康 Buffer 池。

### 3. selector 保持不变

请求链路变成：

1. `PoolManager` 过滤候选集合，只留下 `active`
2. 现有 selector 从 `active` 中继续选号

因此：

- `fill-first` 仍然会持续命中最前面的健康号
- 一旦这个号变坏，`PoolManager` 会将其移出 `active`
- 再补充一个新的健康号进入 `active`

## Pool 状态模型

第一版仅维护内存状态，不引入数据库。

对于每个账号，`PoolManager` 维护：

- `AuthID`
- `PoolState`: `active` / `reserve` / `low_quota` / `limit`
- `LastSelectedAt`
- `LastSuccessAt`
- `LastProbeAt`
- `NextProbeAt`
- `ProtectedUntil`
- `InFlightCount`
- `ConsecutiveFailures`
- `LastProbeReason`

这些状态来源于：

- 文件发现
- 真实请求结果
- 后台探测结果

重启后重新构建即可，不要求持久化。

## 组件职责边界

这是整个设计最关键的地方。

### 现有系统负责

- 401 处理链：`refresh -> verify -> alive/dead/unknown`
- 删除无效文件
- 移动限额文件到 `auth-dir/limit`
- 更新 auth 运行时状态

### PoolManager 负责

- 决定谁在 `active`
- 决定谁在 `reserve`
- 决定谁在 `low_quota`
- active 缺位时触发补号
- 接收已有处置结果并更新池子

### 明确禁止

`PoolManager` 不应直接：

- 删除文件
- 移动文件到 `limit`
- 与现有系统并行各自解释同一个 401 / quota 结果

否则一定会出现双删、双移、状态不一致。

## 必须存在的双向联通

你的理解是对的，这里必须有两条方向的事件流。

### A. 原系统 -> PoolManager

当真实请求或后台探测处理完一个账号后，必须把“最终处置结果”通知给 `PoolManager`。

建议新增：

```go
type AuthDisposition struct {
    AuthID         string
    Provider       string
    Model          string
    Healthy        bool
    PoolEligible   bool
    Deleted        bool
    MovedToLimit   bool
    Refreshed      bool
    QuotaExceeded  bool
    NextRetryAfter time.Time
    NextRecoverAt  time.Time
    Source         string // request / pool_probe
}
```

它表示的是：这个账号经过原系统处理之后，最终被判定成了什么状态。

### B. PoolManager -> 原系统

当 `active` 集合变化时，必须通知原系统更新运行时可见候选集。

最稳妥的方式是复用现有 `AuthUpdate` 链路：

- 进入 active：`Add/Modify`
- 离开 active：`Delete`

这样 `Service -> coreManager -> registry` 这条现有链路仍然成立，不需要单独重写路由器。

## 启动流程

当 PoolManager 开启时：

1. 扫描根目录 auth 文件，并排除 `limit` 文件
2. 按扫描顺序边读取边尝试建立 `active` 池
3. 对前面的候选文件执行健康检查，直到 `active` 数量填满 `size`
4. `active` 填满之后，后续扫描到的文件直接进入 `reserve` 候选集合
5. 启动阶段进入 `reserve` 的文件不做健康检查
6. `Codex` 候选如果周剩余额度 `<= low-quota-threshold-percent`，进入 `low_quota`
7. 将这批 `active` 账号发布给运行时路由

要求：

- 启动阶段优先保证 `active` 池尽快建立
- `active` 填满前，允许对候选文件执行必要的健康检查
- `active` 填满后，不再继续对后续文件做启动期健康探测
- 不要求启动阶段知道全量账号状态

## 运行时维护逻辑

### Active 池

`active` 池是线上请求唯一可见集合。

维护原则：

- 数量始终尽量保持为 `size`
- 某个 `active` 账号一旦不适合继续接流量，就立刻移出
- 然后异步从 `reserve` 补位
- 正在承接请求的账号受 `in-flight` 保护，不参与后台探测

### Reserve 池

`reserve` 不直接参与线上请求。

维护原则：

- 启动时进入 `reserve` 的文件默认不做健康检查
- 不做全量健康检查
- 只做低频随机抽样
- 被选中用于补位时，再做一次正式健康检查
- 若 probe 成功但判定为低额度，则移入 `low_quota`

### LowQuota 池

`low_quota` 是软隔离池，表示：

- 账号当前还能用
- 但对新请求不应优先再进入 `active`

当前规则：

- 仅对 `Codex` 生效
- 判定依据为周剩余额度百分比
- 阈值来自 `pool-manager.low-quota-threshold-percent`
- 默认阈值是 `20`
- `low_quota` 不参与 active 提拔
- `low_quota` 默认不做后台恢复探测

### Limit 池

`limit` 是最低优先级集合。

维护原则：

- 不参与线上路由
- 不进入运行时 active 候选
- 按 `NextRecoverAt` 或更长间隔进行恢复探测
- 恢复后先回到 `reserve`

## 探测策略

### 为什么不能全量探测

在 10 万级账号池里，全量轮询会导致：

- 文件系统扫描成本高
- 网络探测量过大
- 很多探测结果很快失效（因为账号被外部系统同时使用）

所以这里维护的是“高概率健康的 Buffer”，不是全池实时真相。

### active 探测

频率最高，但不等于高频轮询。

规则：

- 刚被真实请求成功过的 active 号，不需要立刻重探
- 最近被选中过、且长时间没有成功反馈的号，优先探测
- `fill-first` 下排在前面的号优先探测
- `InFlightCount > 0` 或仍在保护窗口内的账号必须跳过

### reserve 探测

中频、随机抽样。

规则：

- 从 reserve 中随机抽样
- 不做全量 sweep
- 目标只是补足“可补位的健康候选”
- probe 成功但低额度的账号进入 `low_quota`

### limit 探测

最低频。

规则：

- 尽量按 `NextRecoverAt` 时间来重试
- 没有恢复时间时，按长间隔重试

### low_quota 探测

- 第一版不对 `low_quota` 做后台恢复探测
- 重新评估只在这些时机发生：
  - 服务重启
  - 认证文件被重新导入或更新
  - 手动触发 quota 重扫

## 探测接口

为了满足：

- 不消耗模型额度
- 不触发高频推理调用

建议优先使用轻量接口：

- `https://chatgpt.com/backend-api/wham/usage`

这与仓库中 `temp/apitest` 的现有做法一致，适合作为 codex 健康探测接口。

不建议使用：

- `/responses`
- `/chat/completions`

作为后台保活探测接口。

## 401 处理

401 绝对不能直接等同于死号。

必须走统一链路：

1. 第一次探测得到 401
2. 尝试使用 `refresh_token`
3. 使用新 token 做一次轻量验证
4. 只把结果分成三类：
   - `alive`
   - `dead`
   - `unknown`

处置规则：

- `alive`：更新认证文件中的 token，继续保留或恢复池资格
- `dead`：删除
- `unknown`：不删，只暂时不参与 active

实际执行顺序：

1. 先把问题 auth 从 `active` 摘掉
2. 先切到新的 active auth 继续服务
3. 删除文件 / 移 `limit` / 回写 token 在后台异步执行

## 与现有失败处理逻辑的关系

项目已经具备：

- 删除无效 auth
- 将限额 auth 移到 `limit`
- quota cooldown

Pool 模式必须复用这套语义。

要求：

- 后台探测和真实请求命中的失败，都走同一条结果处理逻辑
- 最终的 delete / move-to-limit 只能有一个执行入口

## 日志设计

为了便于后续排查问题，日志必须结构化且有统一前缀。

### PoolManager 日志

统一前缀：

- `pool-manager:`

示例：

```text
pool-manager: enabled size=100 provider=codex
pool-manager: startup discovered root=98231 limit=6342
pool-manager: startup active target=100 selected=100 reserve=98090 low_quota=41
pool-manager: active removed auth=abc123 reason=quota source=request
pool-manager: active removed auth=def456 reason=deleted source=pool_probe
pool-manager: active demoted auth=ghi789 reason=low_quota remaining_percent=18 threshold=20
pool-manager: promoted auth=ghi789 from=reserve to=active
pool-manager: active set changed add=1 modify=0 delete=1 active_size=100
pool-manager: reserve probe sampled=20 healthy=6 unhealthy=3 skipped=11
pool-manager: limit probe sampled=5 restored=1 still_limited=4
pool-manager: underfilled active target=100 actual=63 reserve_exhausted=true
```

### AuthDisposition 日志

统一前缀：

- `auth-disposition:`

示例：

```text
auth-disposition: auth=abc123 model=gpt-5 action=moved_to_limit source=request retry_after=2026-03-10T00:00:00Z
auth-disposition: auth=def456 action=deleted source=pool_probe reason=dead_after_refresh
auth-disposition: auth=ghi789 action=refreshed source=pool_probe
auth-disposition: auth=jkl012 action=noop source=pool_probe reason=unknown_401
```

### Probe 日志

统一前缀：

- `pool-probe:`

示例：

```text
pool-probe: auth=abc123 bucket=active endpoint=wham/usage result=ok
pool-probe: auth=def456 bucket=active result=401 refresh=success verify=ok
pool-probe: auth=ghi789 bucket=reserve result=429 disposition=limit
pool-probe: auth=jkl012 bucket=limit result=ok disposition=restore_to_reserve
pool-probe: auth=mno345 bucket=reserve result=ok disposition=low_quota
```

### 发布日志

统一前缀：

- `pool-publish:`

示例：

```text
pool-publish: add auth=ghi789 provider=codex
pool-publish: delete auth=def456 provider=codex
pool-publish: completed add=1 modify=0 delete=1
```

### 评估汇总日志

除了事件日志，还应定期打印聚合评估日志，用于和现有统计页面、健康监测页面对照。

统一前缀：

- `pool-eval:`

示例：

```text
pool-eval: baseline interval=5m0s total_requests=0 success=0 failure=0 success_rate=0.00% active_size=100 reserve_size=98090 low_quota_size=41 limit_size=6342
pool-eval: window=5m total_requests=86 success=42 failure=44 success_rate=48.84% active_size=100 reserve_size=20 low_quota_size=41 limit_size=6342
pool-eval: window=5m active_removed=7 promoted=7 refreshed=3 moved_to_limit=5 deleted=1
pool-eval: low_success_rate warning threshold=80.00% consecutive_windows=3 current_rate=48.84% total_requests=86 failure=44 window=5m0s
```

## 失败模式

### active 不足

如果暂时找不到足够健康的账号：

- 只发布当前可用 active
- 记录 underfilled 日志
- 若 `low_quota` 池里有号，也不会自动回退提拔；第一版仍然优先保持软隔离

### 401 无法判断

如果 401 无法明确判断：

- 不删
- 不移到 `limit`
- 只从 `active` 暂时移除
- 后续再探

### 外部直接修改文件

如果有人在外部手动新增、删除、修改认证文件：

- watcher 仍需感知这些变化
- PoolManager 收到后刷新候选集合
- 必要时重算 `active`

## 分阶段落地建议

### Phase 1

- 新增配置
- 新增 PoolManager 组件
- 启动时填充 active
- active 过滤后再交给现有 selector
- 暂不做后台探测

### Phase 2

- 增加 active / reserve / limit 后台探测
- 增加 401 的 `refresh -> verify` 链路
- 增加 disposition 事件

### Phase 3

- 调整探测频率
- 增加更细的指标和管理端观测能力

## 为什么这是合适的方案

这个方案保留了原有系统的核心逻辑：

- selector 不变
- request execution 不变
- auth 失败处置逻辑不变
- 文件目录结构不变

同时只增加一个关键能力：

- 用固定大小的健康 Buffer 池保护请求延迟和可用性

它不是为了“掌握全量账号的精确实时状态”，而是为了在大规模共享号池中，让新请求尽可能只打到健康账号。

最终是否成功，必须回到两个可见结果：

- 请求成功率显著提高，并尽量接近 100%
- 服务健康监测中的异常比例下降
