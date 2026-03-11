# CLIProxyAPI Plus

English | [Chinese](README_CN.md)

This is the Plus version of [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), adding support for third-party providers on top of the mainline project.

All third-party provider support is maintained by community contributors; CLIProxyAPI does not provide technical support. Please contact the corresponding community maintainer if you need assistance.

The Plus release stays in lockstep with the mainline features.

## Pool Tuning

For Codex-heavy deployments with a large `auth-dir`, a tested baseline for reducing steady-state memory and cold-scan pressure is:

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

Notes:
- This keeps `active=100` and refills `reserve` up to roughly `50` while avoiding full cold-pool residency.
- The ratio settings are useful when `auth-dir` contains a very large number of file-based auths.
- `archive-failed-auth: true` should only be used if you want truly unrecoverable auths removed or moved to `auth-dir/limit`.
- Generic `401` is no longer treated as an immediate dead-auth signal; explicit unrecoverable refresh failures such as `refresh_token_reused` are still removed.
- The profile above was verified on a 2 vCPU / 8 GB VPS and reduced steady-state RSS materially compared with the original full-residency behavior.

## Contributing

This project only accepts pull requests that relate to third-party provider support. Any pull requests unrelated to third-party provider support will be rejected.

If you need to submit any non-third-party provider changes, please open them against the [mainline](https://github.com/router-for-me/CLIProxyAPI) repository.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
