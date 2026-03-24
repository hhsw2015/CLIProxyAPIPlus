# CLIProxyAPI Plus

[English](README.md) | 中文 | [日本語](README_JA.md)

这是 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的 Plus 版本，在原有基础上增加了第三方供应商的支持。

所有的第三方供应商支持都由第三方社区维护者提供，CLIProxyAPI 不提供技术支持。如需取得支持，请与对应的社区维护者联系。

该 Plus 版本的主线功能与主线功能强制同步。

## 池子调优建议

如果你在 Codex 场景下使用大量文件型账号，一个已经验证过的基线配置如下：

```yaml
archive-failed-auth: true

pool-manager:
  size: 100
  provider: "codex"
  active-idle-scan-interval-seconds: 1800
  active-quota-refresh-interval-seconds: 60
  active-quota-refresh-sample-size: 10
  background-probe-budget-window-seconds: 10
  background-probe-budget-max: 2
  reserve-scan-interval-seconds: 300
  limit-scan-interval-seconds: 21600
  reserve-sample-size: 20
  reserve-refill-low-ratio: 0.25
  reserve-refill-high-ratio: 0.50
  cold-batch-load-ratio: 0.05
  low-quota-threshold-percent: 20
```

说明：
- 这个配置会保持 `active=100`，并把 `reserve` 补到大约 `50`，同时避免把整个 cold 池长期常驻在内存里。
- 当 `auth-dir` 里的文件型账号很多时，建议开启这些 ratio 参数。
- `archive-failed-auth: true` 适合你确实希望把真正不可恢复的账号删除，或者把额度耗尽的账号移到 `auth-dir/limit` 的场景。
- 普通 `401` 不再直接视为死号；只有像 `refresh_token_reused` 这种明确不可恢复的 refresh 失败才会被删除。
- 以上配置已经在 2 vCPU / 8 GB 的 VPS 上验证过，常驻 RSS 相比最初的全量常驻方案明显下降。
## 贡献

该项目仅接受第三方供应商支持的 Pull Request。任何非第三方供应商支持的 Pull Request 都将被拒绝。

如果需要提交任何非第三方供应商支持的 Pull Request，请提交到[主线](https://github.com/router-for-me/CLIProxyAPI)版本。

## 许可证

此项目根据 MIT 许可证授权 - 有关详细信息，请参阅 [LICENSE](LICENSE) 文件。
