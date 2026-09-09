# CLIProxyAPI Quota Router

Quota-aware account routing for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). The plugin routes configured protected models away from OAuth accounts when provider usage reaches a configurable cutoff, while leaving other models on CLIProxyAPI's native scheduler. It supports Anthropic's overall seven-day quota and model-specific `weekly_scoped` limits such as Fable.

The default specifically protects `claude-fable-5` at 50%. This matches Anthropic's [June 30 redeployment announcement](https://www.anthropic.com/news/redeploying-fable-5), which included Fable 5 for up to 50% of weekly usage through July 7, 2026; Anthropic said access would use usage credits afterward, so both the model and cutoff remain configurable.

## Install

Download the archive for your platform from [Releases](https://github.com/Smarty-Pants-Inc/cpa-plugin-quota-router/releases), extract the library into CLIProxyAPI's plugin directory, and configure it by plugin ID:

```yaml
plugins:
  enabled: true
  configs:
    quota-router:
      enabled: true
      priority: 100
      protected-models: [claude-fable-5-1]
      protected-accounts: [dad@example.com]
      usage-scope: fable
      block-on-unknown: true
      cutoff-percent-used: 75
      near-cutoff-percent-used: 70
      poll-interval: 5m
      near-cutoff-poll-interval: 1m
      request-timeout: 10s
```

The library basename must be `quota-router` or a semver-suffixed form such as `quota-router-v0.5.0`, with `.dylib`, `.so`, or `.dll` for the host platform.

Published releases include Darwin (`amd64`, `arm64`), Linux (`amd64`, `arm64`), and Windows (`amd64`) builds.

`poll-interval` is the minimum cached-usage age before another refresh, not a continuous polling timer.

## Configuration

| Field | Default | Meaning |
| --- | --- | --- |
| `protected-models` | `[claude-fable-5]` | Exact, case-insensitive model IDs governed by the cutoff. |
| `protected-accounts` | `[]` | Exact auth ID, auth index, file name, email, or account selectors governed by the cutoff. Empty preserves the upstream all-account behavior. |
| `usage-scope` | `seven_day` | `seven_day` for the overall weekly meter, or a model display name such as `fable` for its `weekly_scoped` meter. |
| `block-on-unknown` | `false` | When true, a protected account is unavailable while its selected quota is missing, expired, or older than `poll-interval`. |
| `cutoff-percent-used` | `50` | Blocks a protected account at or above this utilization percentage for the selected scope. A cutoff of 75 reserves the final 25%. |
| `near-cutoff-percent-used` | disabled | Optional utilization threshold that switches to the shorter near-cutoff polling interval. Must be below the cutoff. |
| `poll-interval` | `5m` | Minimum cache age below the near-cutoff threshold. |
| `near-cutoff-poll-interval` | `1m` | Minimum cache age at or above the near-cutoff threshold. |
| `request-timeout` | `10s` | Timeout for the provider usage request, expressed as a Go duration. |

## Behavior

- Refreshes enabled physical Claude OAuth credentials when the worker starts; startup reconfiguration retries discovery only while the cache is empty. There is no time-driven polling.
- With `protected-accounts` configured, native CLIProxyAPI scheduling is preserved while those accounts are eligible. If a protected account must be withheld, the plugin selects the highest-priority remaining candidate.
- A protected-model request queues one asynchronous refresh when cached usage reaches the applicable normal or near-cutoff age. With `block-on-unknown: true`, that account is withheld while the refresh is pending.
- Coalesces concurrent refreshes, and does not refresh a known blocked account again before its reported reset time.
- A rejection-only design cannot enforce a pre-exhaustion cutoff: the rejection arrives only after the hard limit is reached.
- Applies only to exact, case-insensitive `protected-models` matches.
- Blocks an account at or above `cutoff-percent-used`; unknown or reset-expired quota state follows `block-on-unknown` while a refresh is queued.
- Never changes auth files or CLIProxyAPI's permanent disabled state.
- Exposes authenticated status at `GET /v0/management/plugins/quota-router/status`.

## Security

- Reads eligible Claude credentials through CLIProxyAPI's host API and never writes auth files.
- Sends the access token only to Anthropic's fixed HTTPS usage endpoint and refuses redirects.
- Never logs access tokens, refresh tokens, or provider response bodies.
- Exposes status only through CLIProxyAPI's authenticated Management API.

## Build and test

Requires Go 1.26 and CLIProxyAPI v7.2.100 or newer.

```bash
make test
make test-race
make vet
make build
```

Release tags matching `v*` build store-compatible archives and `checksums.txt` through GitHub Actions.

Originally created and published by [Smarty Pants Inc](https://github.com/Smarty-Pants-Inc). Scoped-account and model-specific quota support maintained in this fork by [jrneulight](https://github.com/jrneulight). MIT licensed.
