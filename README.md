# CLIProxyAPI Quota Router

Quota-aware account routing for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). The plugin routes configured protected models away from OAuth accounts when provider usage reaches a configurable cutoff, while leaving other models on CLIProxyAPI's native scheduler. It supports Anthropic's overall seven-day quota and model-specific `weekly_scoped` limits such as Fable. An optional Bedrock compatibility interceptor also makes Claude Code requests safe for an Amazon Bedrock fallback after that fallback has been selected.

The default specifically protects `claude-fable-5` at 50%. This matches Anthropic's [June 30 redeployment announcement](https://www.anthropic.com/news/redeploying-fable-5), which included Fable 5 for up to 50% of weekly usage through July 7, 2026; Anthropic said access would use usage credits afterward, so both the model and cutoff remain configurable.

## Install

Download the archive for your platform from [this fork's Releases](https://github.com/jrneulight/cpa-plugin-quota-router/releases), extract the library into CLIProxyAPI's plugin directory, and configure it by plugin ID:

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
      bedrock-compat: true
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
| `cutoffs` | unset | Optional list of per-window rules that replaces `usage-scope`, `cutoff-percent-used` and `protected-models`. See [Per-window reserves](#per-window-reserves). |
| `usage-source` | `endpoint` | Where usage readings come from. `endpoint` polls Anthropic's usage endpoint with each protected account's token. `seat-pacer-file` reads the readings the claude-seat-pacer plugin saves and sends nothing to Anthropic; it requires `cutoffs`. See [Sharing Seat Pacer's readings](#sharing-seat-pacers-readings). |
| `seat-pacer-file` | `~/.cli-proxy-api/plugins/claude-seat-pacer/history.json` | Seat Pacer's `history.json`, read when `usage-source` is `seat-pacer-file`. A leading `~/` is the home directory of the user running CLIProxyAPI; a relative path resolves against CLIProxyAPI's working directory, so prefer an absolute one. |
| `bedrock-compat` | `false` | After a Bedrock Claude model is selected, removes Claude-native metadata, unsupported beta flags, and unsupported advisor/tool-search features. Native Claude OAuth requests are unchanged. |

## Behavior

- Refreshes enabled physical Claude OAuth credentials when the worker starts; startup reconfiguration retries discovery only while the cache is empty. There is no time-driven polling unless `cutoffs` is set.
- With `protected-accounts` configured, native CLIProxyAPI scheduling is preserved while those accounts are eligible. If a protected account must be withheld, the plugin selects the highest-priority remaining candidate.
- A protected-model request queues one asynchronous refresh when cached usage reaches the applicable normal or near-cutoff age. With `block-on-unknown: true`, that account is withheld while the refresh is pending.
- Coalesces concurrent refreshes, and does not refresh a known blocked account again before its reported reset time.
- A rejection-only design cannot enforce a pre-exhaustion cutoff: the rejection arrives only after the hard limit is reached.
- Applies only to exact, case-insensitive `protected-models` matches.
- Blocks an account at or above `cutoff-percent-used`; unknown or reset-expired quota state follows `block-on-unknown` while a refresh is queued.
- Never changes auth files or CLIProxyAPI's permanent disabled state.
- With `bedrock-compat: true`, request rewriting runs only after the selected upstream model has an Amazon Bedrock Anthropic prefix (`anthropic.`, `us.anthropic.`, `global.anthropic.`, `eu.anthropic.`, or `apac.anthropic.`). It removes `metadata`, unsupported beta flags, unsupported advisor/tool-search server tools, and advanced tool-only properties. This degrades those unavailable features instead of failing the entire Bedrock fallback request.
- Exposes authenticated status at `GET /v0/management/plugins/quota-router/status`.

## Per-window reserves

`cutoffs` holds back part of several usage windows on the protected accounts at once. Each rule names a window, the percent used at which the account is refused, and the models it covers:

```yaml
quota-router:
  enabled: true
  protected-accounts: [charles@example.com]
  block-on-unknown: true          # refuse when the reading is missing or stale
  poll-interval: 2m
  near-cutoff-percent-used: 60     # must sit below every cutoffs percent-used
  near-cutoff-poll-interval: 1m
  cutoffs:
    - scope: five_hour             # keeps 30% of the 5-hour session
      percent-used: 70
    - scope: seven_day             # keeps 30% of the all-models week
      percent-used: 70
    - scope: fable                 # keeps 30% of the Fable week
      percent-used: 70
      models: [claude-fable-5, claude-fable-5-1]
```

- `scope` is `five_hour`, `seven_day`, or a `weekly_scoped` model display name such as `fable`. `models` defaults to every model; `"*"` says so explicitly.
- A protected account is refused once **any** rule covering the requested model is at or above its `percent-used`. A window whose reset time has passed counts as reset. With `block-on-unknown: true`, a reading older than three `poll-interval`s, or a window the usage endpoint did not report, also refuses.
- The reserve is enforced in two places. As the scheduler, the plugin skips refused protected accounts. After CLIProxyAPI has chosen an account, the request interceptor refuses a request routed to a refused protected account with `429 rate_limit_error` (code `quota_router_reserve`) and a `Retry-After` until the window resets. The second check is what holds when another plugin, such as a pacing scheduler, is the active scheduler: CLIProxyAPI consults only the highest-priority scheduler plugin.
- While `cutoffs` is set, the worker re-reads the protected accounts every `poll-interval`, or `near-cutoff-poll-interval` once any window reaches `near-cutoff-percent-used`, because no scheduler pick may prompt it.

## Sharing Seat Pacer's readings

[claude-seat-pacer](https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer) already polls Anthropic's usage endpoint for every Claude account. When quota-router polls the same endpoint for the same account, Anthropic answers one of the two readers with `429`, quota-router's reading goes stale, and a strict reserve (`block-on-unknown: true`) refuses an account that has plenty left. One poller per account, with other readers sharing its saved reading, avoids the double read.

```yaml
quota-router:
  enabled: true
  protected-accounts: [charles@example.com]
  block-on-unknown: true
  usage-source: seat-pacer-file
  seat-pacer-file: /Users/me/.cli-proxy-api/plugins/claude-seat-pacer/history.json
  poll-interval: 5m
  cutoffs:
    - scope: five_hour
      percent-used: 70
    - scope: seven_day
      percent-used: 70
    - scope: fable
      percent-used: 70
      models: [claude-fable-5]
```

- quota-router makes no request to Anthropic and reads no credential. On the same cadence it would have polled (`poll-interval`, `near-cutoff-poll-interval`, and when a refused request finds a reading stale, at most once every 5s) it reads the file's `readings` and records each protected account's windows. It never writes the file.
- Seat Pacer's 0..1 utilization becomes percent points; usage past a cap, or a window Anthropic is refusing (`status: rejected`), reads as 100%. `weekly_scoped` windows are keyed by their lowercased model name, so `Fable` is `fable`. Readings refreshed from response headers count the same as usage-endpoint ones.
- A reading counts as taken at its own `observed_at`, not when quota-router read the file, so the reserve's staleness rule (three `poll-interval`s plus `request-timeout`) measures how old Seat Pacer's reading is.
- A missing, unreadable, unparseable or unknown-version file, or no reading for an account, is recorded as `seat_pacer_file_missing`, `seat_pacer_file_unreadable`, `seat_pacer_file_invalid`, `seat_pacer_file_version` or `seat_pacer_no_reading` and logged at warn. The last good reading stays until it is stale; after that `block-on-unknown` decides, and strict refuses. Files over 4 MiB are refused.

**Trade-off: the reserve is only as fresh as Seat Pacer's last reading.** Seat Pacer polls on its own `quota.poll-interval` (2m by default, slower when Anthropic throttles it) and keeps trusting a reading up to its `quota.max-staleness` (15m by default; often raised, for example to 45m). quota-router's reserve treats a reading older than `3 × poll-interval + request-timeout` as unknown — about 15m at the default `poll-interval: 5m`. If Seat Pacer polls an account less often than that, a strict reserve refuses the account between its polls. Set quota-router's `poll-interval` so `3 × poll-interval` covers Seat Pacer's poll cadence for idle accounts (for example `poll-interval: 15m`, about 45m of tolerance, against a Seat Pacer `max-staleness: 45m`), and accept that a cutoff can be overshot by whatever the account uses between two Seat Pacer readings. Seat Pacer also moves a reading's `observed_at` forward when response headers update some of its windows, so a window the headers do not carry, such as a model's weekly window, can be older than `observed_at` says.

## Security

- Reads eligible Claude credentials through CLIProxyAPI's host API and never writes auth files.
- Sends the access token only to Anthropic's fixed HTTPS usage endpoint and refuses redirects.
- With `usage-source: seat-pacer-file`, reads no credential and sends nothing to Anthropic; it only reads Seat Pacer's file and never writes it or logs its contents.
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
