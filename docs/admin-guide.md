# 管理员配置指南

## 前提条件

系统已部署运行，可通过 `http://你的域名:8318` 访问。

---

## 第一步: 登录管理后台

1. 打开浏览器访问 `http://你的域名:8318`
2. 使用管理员账号登录:
   - 邮箱: `admin@cpa.local` (部署时设置)
   - 密码: 部署时设置的密码
3. 登录后进入管理面板

---

## 第二步: 配置支付 (必须)

进入 **设置 → 支付设置**:

### 2.1 开启支付

- 启用支付: **开启**
- 最低金额: 建议 `1`
- 最高金额: 按需设置(留空不限制)
- 订单超时时间: 建议 `30` 分钟

### 2.2 添加支付服务商

进入 **服务商管理 → 添加服务商**，选择一种:

| 服务商 | 需要的信息 | 适合场景 |
|--------|-----------|---------|
| EasyPay(易支付) | 商户ID + 密钥 + API地址 | 最简单，第三方聚合 |
| 支付宝官方 | AppID + 应用私钥 + 支付宝公钥 | 手续费低，需要审核 |
| 微信官方 | AppID + 商户号 + APIv3密钥 + 证书 | 手续费低，需要审核 |
| Stripe | Secret Key + Publishable Key + Webhook Secret | 国际支付 |

### 2.3 配置 Webhook

添加服务商后，系统自动生成回调地址。将回调地址配置到支付平台:

| 服务商 | 回调地址 |
|--------|---------|
| EasyPay | `https://你的域名/api/v1/payment/webhook/easypay` |
| 支付宝 | `https://你的域名/api/v1/payment/webhook/alipay` |
| 微信 | `https://你的域名/api/v1/payment/webhook/wxpay` |
| Stripe | `https://你的域名/api/v1/payment/webhook/stripe` |

> 注意: 回调地址必须是 HTTPS。如果使用 HTTP，需要在支付平台允许。

---

## 第三步: 开启用户注册

进入 **设置 → 通用设置**:

- 允许注册: **开启**
- 邮箱验证: 按需(开启需配置邮件服务)
- 新用户默认余额: 建议 `0` (需充值后使用)
- 新用户默认并发: 建议 `5`

---

## 第四步: 配置用户分组权限

### 4.1 理解分组

系统已自动创建以下分组(从 CPA 配置同步):

| 分组名称 | 平台 | 说明 |
|---------|------|------|
| CPA-anthropic-bedrock-P10 | anthropic | Bedrock Claude (最高优先级) |
| CPA-anthropic-apikey-P10 | anthropic | Claude API Key (高优先级) |
| CPA-anthropic-apikey-P9 | anthropic | TaijiAI Claude 等 |
| CPA-gemini-apikey-P10 | gemini | Gemini API Key |
| CPA-openai-compat-P10 | openai | Azure/Bedrock OpenAI |
| CPA-openai-compat-P8 | openai | Cookie Pool 等 |
| ... | ... | ... |

### 4.2 分配分组给用户

进入 **用户管理**:

1. 点击用户 → 编辑
2. 在"允许的分组"中，选择该用户可以使用的分组
3. 保存

> **建议**: 创建一个"默认分组"包含所有常用分组，新用户注册后自动分配。

### 4.3 设置默认分组(新用户自动获得)

进入 **设置 → 通用设置**:
- 找到"默认分组"设置
- 选择新用户注册后自动获得的分组

---

## 第五步: 渠道定价 (可选)

系统自带 219 个模型的默认定价(来自 LiteLLM 定价库)，开箱即用。

如果需要自定义价格:

1. 进入 **渠道管理 → 渠道定价**
2. 点击 CPA-Claude / CPA-OpenAI / CPA-Gemini
3. 添加模型定价规则:
   - 选择平台
   - 输入模型名(支持通配符 `claude-*`)
   - 设置输入/输出价格(每百万 token 美元)
4. 保存

> 不设置自定义价格时，系统使用各模型的官方定价。

---

## 第六步: 验证系统

### 6.1 管理员验证

1. **渠道管理**: 应看到 3 个渠道(CPA-Claude/CPA-OpenAI/CPA-Gemini)
2. **账号管理**: 应看到所有 CPA 凭证(带"CPA 托管"标记)
3. **分组管理**: 应看到 15+ 个 CPA 分组
4. **代理设置**: 点击侧边栏"代理设置"，应看到 CPA 管理面板(无需再次登录)

### 6.2 用户验证

1. 退出管理员账号
2. 注册一个新用户
3. 充值测试金额
4. 创建 API Key
5. 用 API Key 发请求:

```bash
curl http://你的域名:8318/v1/chat/completions \
  -H "Authorization: Bearer sk-你的apikey" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}'
```

6. 返回管理面板 → **使用记录**: 应看到刚才的请求记录和计费信息

---

## 日常运维

### 查看系统状态

- **仪表盘**: 查看请求量、收入、活跃用户
- **使用记录**: 查看每个请求的详细信息(模型、token、成本)
- **账号管理**: 查看 AI 凭证状态(活跃/错误/限流)

### 管理用户

- **用户管理**: 查看/编辑/禁用用户
- **充值**: 手动给用户加余额
- **兑换码**: 创建兑换码给用户自助充值

### 管理代理

点击侧边栏 **代理设置** 进入 CPA 管理面板:
- 查看 provider 状态
- 查看模型列表
- 查看路由配置
- CLI 接管管理

### 查看日志

```bash
# VPS 上查看
ssh 你的VPS
tmux attach -t cpa-new
# Ctrl+C 停止, 方向键翻日志
# 或直接看日志文件:
tail -f ~/CLIProxyAPIPlus-new/cpa-new.log
```

---

## 常见问题

### Q: 用户请求返回 401

**原因**: API Key 无效、过期、或未分配到有效分组。

**解决**: 
1. 管理面板 → 用户管理 → 查看该用户的 API Key
2. 确认 Key 状态是 active
3. 确认用户有分配分组权限
4. 确认用户余额 > 0

### Q: 渠道管理/账号管理页面显示"CPA 托管"无法编辑

**正常**: CPA 托管的数据由 CPA 配置文件管理，不支持在 Web 面板编辑。如需修改凭证/优先级，修改 CPA 的 `config.yaml` 后重启服务。

### Q: 新加的 API Key 没有出现在管理面板

**原因**: 数据同步每 5 分钟执行一次，或者需要重启服务。

**解决**: 修改 config.yaml 后，重启 CPA 服务即可立即同步。

### Q: 怎么修改管理员密码

```bash
# 方法1: 在管理面板 → 个人设置 → 修改密码
# 方法2: 通过数据库直接修改 (需要 bcrypt hash)
PGPASSWORD=changeme psql -U sub2api -h localhost -d sub2api \
  -c "UPDATE users SET password_hash = '新的bcrypt_hash' WHERE email = 'admin@cpa.local';"
```

### Q: 如何添加新的 AI 提供商

修改 CPA 的 `config.yaml`，在对应的 key 数组中添加:

```yaml
claude-key:
  - api-key: "新的API Key"
    priority: 8
    base-url: "https://api.provider.com"
```

保存后重启服务，5 分钟内自动同步到管理面板。

---

## 配置清单 (上线前确认)

- [ ] 支付服务商已添加并测试
- [ ] Webhook 回调地址已配置且可访问 (HTTPS)
- [ ] 用户注册已开启
- [ ] 默认分组已设置
- [ ] 测试用户注册 → 充值 → 发请求 → 计费 完整流程通过
- [ ] 管理面板所有页面可正常访问
- [ ] 域名/端口防火墙已开放
