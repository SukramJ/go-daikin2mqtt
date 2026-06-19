# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A pure-Go daemon that bridges the **Daikin ONECTA cloud API** to **MQTT** with optional
**Home Assistant** auto-discovery and a diagnostic web UI. It polls Daikin devices through
the cloud, publishes their state to MQTT, and applies HA commands back as ONECTA PATCHes.
Two binaries: `daikin2mqtt` (the daemon) and `daikin2mqtt-util` (auth/diagnostics CLI).

## Commands

```bash
make build            # both binaries → bin/  (ldflags inject version/commit/date)
make test             # full suite with -race
make check            # the pre-commit/pre-push gate: vet + fmt-check + lint + test
make lint             # golangci-lint (config in .golangci.yaml; version "2", many linters)
make fmt              # gofumpt + goimports, in place
make run              # build daemon + run against ./config.yaml
make setup            # install dev tooling AND wire git hooks (run once per clone)

# single test / package
go test -run TestEntityObjectID ./internal/hass/
go test ./internal/coordinator/

# util CLI — add --mock <example-id> to devices/points to hit the ONECTA mock endpoint
go run ./cmd/daikin2mqtt-util devices --mock altherma-air-to-water-wlan
```

Go ≥ 1.26, `CGO_ENABLED=0`. Minimal deps (`golang.org/x/sync`, `yaml.v3`) — keep it that way.

## Workflow gotchas

- **Direct commits to `main`/`master` are blocked** by `.githooks/pre-commit` (wired via
  `make setup`/`make hooks`). Always branch + open a PR. Override once with
  `ALLOW_MAIN_COMMIT=1 git commit …` only if truly needed. Merging/pulling into main is fine.
- **Release procedure** (used for the 0.1.x tags): bump the version in **four** places in one
  commit — `internal/version/version.go` (the `Version` default), `addon/config.yaml`,
  `addon/Dockerfile` (`BUILD_VERSION`), and add a `# Version X.Y.Z (YYYY-MM-DD)` section at the
  top of `changelog.md`. Then branch → PR → squash-merge → tag **`vX.Y.Z`** (note the `v`
  prefix; `0.1.0` is the lone exception). `make release` reads the version from
  version.go's default; `release-on-tag.yml` cross-compiles and extracts release notes from
  changelog.md.

## Architecture

### Runtime wiring (`cmd/daikin2mqtt/main.go`, `run()`)
Load config → load catalog → build `auth.TokenSource` → `client.Client` → connect MQTT
(with LWT) → optional `hass.Discovery` → launch `coordinator` + optional `web` server in an
`errgroup`; block until signal or first goroutine error.

### Coordinator — the orchestration core (`internal/coordinator/`)
`Run()` spawns a **poll loop** and a **write drain**, coordinated by errgroup.

- **Poll cycle** (`pollOnce`): `cloud.GetDevices()` → `model.ParseDevices()` →
  `updateModeCache()` (records each MP's current `operationMode`, needed for mode-scoped
  setpoint PATCH paths) → `process.ResolveAt()` (flatten to `Point`s) → publish discovery
  *only when the point-set signature changes* → publish each point's state → publish the
  **synthetic** climate topics (`hvac_mode`, `fan_mode`, `swing_mode`, `swing_h_mode`,
  `preset_mode`) that the combined HA climate entity consumes but that are not in the catalog.
- **Adaptive interval**: `cfg.PollInterval(hour)` returns a day vs. night value.
- **Write path**: subscribes `<root>/+/+/+/set`, queues onto a channel, drains sequentially.
  Synthetic topics route to climate handlers; catalog topics resolve via `catalog.ByTopic`,
  coerce the payload (select label → raw code, string → number), substitute `{mode}` in the
  PATCH path from `modeCache`, then `cloud.Patch(...)`. The climate fan/swing/preset reverse
  mapping lives in `climate.go` (`canonicalAux`).

### Cloud client — four ONECTA quirks in one place (`internal/daikin/client/client.go`)
1. **Single global `cloudLock` mutex** — ONECTA allows only one in-flight request; every GET
   and PATCH serializes through it.
2. **Scan-ignore window** — after a PATCH the cloud returns stale data for ~`SCAN_IGNORE`s, so
   `GetDevices` returns `ErrScanIgnore` during that window and the poll is skipped.
3. **Rate-limit accounting** — parses `X-RateLimit-*` headers; HTTP 429 → `ErrRateLimited`
   and is **not** a circuit-breaker fault.
4. **Retry + circuit breaker** — GETs retry 3× with exponential backoff + jitter on net/5xx;
   a breaker opens after repeated 5xx. 401/429/auth errors never trip the breaker. A 401
   triggers one token refresh + retry.

### Auth (`internal/daikin/auth/`)
OAuth2 Authorization-Code + **PKCE**. `callback.go` runs a temporary HTTP server for the
redirect (or the web UI's server hosts `/callback`). `tokensource.go` caches/persists the
token via `store.go` (JSON file, `0600`), refreshes ~60s before expiry, **re-persists rotated
refresh tokens**, and returns `ErrReauthRequired` when the refresh token is dead (daemon stays
up; user re-auths via UI or `daikin2mqtt-util auth`). `client_id`/`client_secret` come from
config/env, never the token store.

### Catalog-driven mapping (`characteristics.yaml` + `internal/catalog/`)
Coverage is **curated and deterministic**: only characteristics with an explicit `match` entry
are published. An `Entry` maps one ONECTA characteristic → MQTT/HA (`topic`, `name`/`name_de`,
`platform`, `device_class`, `unit`, `settable`, enum `values` with `label`/`label_de`,
optional nested `value_path`/`path` with a `{mode}` token, `precision`, energy `kind`). Lookups:
`ByTopic` (write path), `Match(mpType, char)` (read path). `daikin2mqtt-util catalog-check`
reports live characteristics missing from the catalog.

### Process — JSON → Points (`internal/process/process.go`)
`ResolveAt` walks device → management point → catalog entry and extracts a scalar `Point`.
Three shapes: plain scalar; **nested** scalar via `value_path` (descends ONECTA "wrapper"
objects `{value, unit, minValue, maxValue, stepValue, settable}`, substituting `{mode}` with
the live `operationMode`); and **energy** (sums `consumptionData[...]` arrays into
daily/weekly/monthly totals). Live min/max/step from the wrapper feed HA `number` entities.

### HA discovery (`internal/hass/discovery.go` + `climate.go`)
Retained configs at `<baseTopic>/<platform>/<unique_id>/config`. Devices are grouped; gateway
and outdoor units become sub-devices (`via_device`). The combined **climate** entity replaces
the individual power/mode/setpoint controls.

**Entity-ID invariant (do not break):** `unique_id` and `default_entity_id` are English and
language-independent. `default_entity_id` is built by `entityObjectID(deviceName, topic)` =
`<domain>.<slug(deviceName)>_<english_topic>` (umlauts transliterated ä→a/ö→o/ü→u/ß→ss,
adjacent duplicate tokens collapsed) — HA otherwise derives the entity_id from the *localized*
display name, producing German IDs. Only the HA display `name` is localized. HA does **not**
rename already-registered entities, so an entity-ID scheme change requires deleting/recreating
(or renaming via the registry API) the existing entities.

### i18n rules (`internal/catalog/localize.go`, `internal/web/assets/i18n/`)
`LANGUAGE` is `en` (default/fallback) or `de`. **Localized:** catalog/entity display names,
enum labels, web-UI text. **Not localized:** MQTT topics, entity_ids, characteristic keys, and
command *values*. **The one documented exception:** the climate fan/swing/preset dropdowns emit
the German *label as the command value* (HA's MQTT climate platform has no separate label/value
field), reversed on write by `canonicalAux` (see `coordinator/climate.go`).

### MQTT (`internal/mqtt/`, `internal/mqtt/protocol/`)
A **custom pure-Go MQTT 3.1.1 implementation** (no paho/library): packet codec in `protocol/`,
TCP adapter, and a `Lifecycle` wrapper that auto-reconnects with backoff and re-fires
`OnConnect` (used to re-announce availability). Narrow `Publisher`/`Subscriber`/`Client`
interfaces make the coordinator and discovery testable with stubs.

### Topic layout
```
<MQTT_TOPIC>/<deviceID>/<embeddedID>/<topic>/state    # retained, QoS0
<MQTT_TOPIC>/<deviceID>/<embeddedID>/<topic>/set      # subscribed, settable entities
<MQTT_TOPIC>/bridge/status                            # LWT: online/offline (availability)
homeassistant/<platform>/<unique_id>/config           # HA discovery, retained
```

### Web UI (`internal/web/`)
Optional (`WEB_ENABLE`, default off, bind `127.0.0.1:8080`, optional basic auth). Vanilla
embedded SPA (`//go:embed`, no build step). Same server hosts the OAuth `/callback`, so no
inbound port forwarding is needed. **HA-ingress aware** (relative asset URLs; can derive the
OAuth redirect URI from the request behind ingress).

## Config

Flat YAML (`config-template.yaml` documents every key). Every key is overridable by a
`DAIKIN_<KEY>` env var (bool/int/float coerced; else string). Loader pipeline: file → env
override → defaults → validate. Required: `DAIKIN_CLIENT_ID`/`SECRET`, `MQTT_SERVER`. With
credentials missing the daemon starts **idle** (web UI reachable for setup) rather than crashing.

## Testing conventions

Table-driven tests with stub `CloudClient`/`MQTT`/`TokenSource` (narrow interfaces) and an
injectable clock for rate-limit/scan-ignore logic. Golden-style assertions against JSON
fixtures under `internal/process/testdata` and `internal/daikin/model/testdata`. Run with
`-race` (what `make test` does).
