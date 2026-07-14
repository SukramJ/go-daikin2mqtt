# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A pure-Go daemon that bridges the **Daikin ONECTA cloud API** to **MQTT** with optional
**Home Assistant** auto-discovery and a diagnostic web UI. It polls Daikin devices through
the cloud, publishes their state to MQTT, and applies HA commands back as ONECTA PATCHes.
An optional **local-first mode** instead reads/writes the indoor units over their local
**Faikin/Faikout** MQTT interface (see "Local-first & multi-split" below and `docs/design.md`).
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
- **Release procedure** (used for the 0.1.x tags): bump the version in **five** places in one
  commit — `internal/version/version.go` (the `Version` default), `addon/config.yaml`,
  `addon/Dockerfile` (`BUILD_VERSION`), add a `# Version X.Y.Z (YYYY-MM-DD)` section at the
  top of `changelog.md`, **and** a matching (condensed) section at the top of
  `addon/CHANGELOG.md` — that file is what Home Assistant shows as the add-on changelog in
  its UI, so it must never be forgotten. Then branch → PR → squash-merge → tag **`vX.Y.Z`** (note the `v`
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
- **Manual refresh** (`refresh.go`): an HA **button** entity on the outdoor unit (topic
  `refresh`, `scope: outdoor` → one per outdoor unit) whose `/set` press makes the poll loop
  run a cycle **now** instead of waiting out the interval. It is a daemon action, not device
  data: the catalog entry matches the synthetic characteristic `daemonRefresh` (never reported
  by the cloud), so its point is synthesized in `refreshPoints` for discovery only and
  publishes no state (HA's MQTT button has none). Presses within `refreshMinInterval` (30s) of
  the last poll are dropped and concurrent presses coalesce — the ONECTA daily request quota
  must not be spendable from an automation.
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
optional nested `value_path`/`path` with a `{mode}` token, `precision`, energy `kind`, and
`scope` (`outdoor` = one entity per outdoor unit with write fan-out; see below)). Lookups:
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
the individual power/mode/setpoint controls. Each entity also advertises a `json_attributes_topic`;
the coordinator publishes `{"data_source":"cloud"|"local"}` there (`publishDataSources`/`dataSource`)
so HA shows whether a value came from the ONECTA cloud or the local Faikin path. Static device
identity (model, serial, sw/firmware version, MAC) lives in the HA `device` object (from
`deviceInfos`), **not** as separate sensors. **Orphan cleanup:** `Discovery.Publish` returns the set
of config topics it published; `reconcileOrphans` then collects the retained configs under the
discovery prefix and clears this daemon's own (`IsOwnConfig`: `daikin_…` uid + state topic under our
root) that are no longer published — so entities removed/moved/renamed across versions don't linger
in HA. Other integrations' configs are never touched.

**Entity-ID invariant (do not break):** `unique_id` and `default_entity_id` are English and
language-independent. `default_entity_id` is built by `entityObjectID(deviceName, topic)` =
`<domain>.<slug(deviceName)>_<english_topic>` (umlauts transliterated ä→a/ö→o/ü→u/ß→ss,
adjacent duplicate tokens collapsed) — HA otherwise derives the entity_id from the *localized*
display name, producing German IDs. Only the HA display `name` is localized. HA does **not**
rename already-registered entities, so an entity-ID scheme change requires deleting/recreating
(or renaming via the registry API) the existing entities.

### Local-first & multi-split (`internal/faikin/`, `internal/coordinator/{backend,local,outdoor}.go`)
Opt-in via `LOCAL_MODE` + `LOCAL_DEVICE_MAP` (ONECTA device ID → Faikin host; accepts a YAML map
or an `id=host,…` string). Three concerns, all sitting on top of the existing cloud path:

- **Control backend seam** (`backend.go`, `setCharacteristic`): every write routes to the local
  Faikin per-setting command topic (`command/<host>/<suffix>`, payload `true`/`false` for switches,
  built by `faikinCommand`) when the device is mapped AND the characteristic is locally
  controllable; otherwise the cloud PATCH. Anything Faikin does not model falls back to the cloud.
  (The combined `command/control` JSON does not take effect for outdoor silent on multi-split units.)
- **Local reads** (`local.go`): subscribe `state/<host>` per mapped device (the firmware's
  canonical state topic — the one its own HA discovery reads from; app form: `mode` word,
  `temp`=room, `target`=setpoint), translate and
  republish onto the **same** per-unit state topics (so HA sees identical entities); the cloud
  poll skips those topics (`localTopics`). In local mode each mapped device's HA `configuration_url`
  is pointed at its Faikin web UI (`http://<ipv4>/`, from the module's reported `ipv4`/`ipv6`),
  mirroring Faikin's own discovery (`applyFaikinConfigURLs`). Faikin may interleave **OS/heartbeat docs** (no AC fields)
  on `state/<host>` — `faikin.ParseState` sets `HasAC` (presence of `power`) and the read path
  **skips** them, else every entity would reset to its zero value. For settings the cloud does not
  expose for a unit (econo/streamer/outdoor silent/demand on the FTXA range, plus local-only
  telemetry), `localOnlyPoints` **synthesizes discovery points** so the entities still appear (state
  fed from Faikin). These catalog entries match a synthetic characteristic (`faikinLocal`) the cloud
  never reports, so they are only ever published via the local path. Telemetry placement follows the
  physics (verified on a live multi-split): power and lifetime energy are each indoor unit's **own**
  reading → published **per indoor unit** (`energy_total`, `power_consumption`; energy **held** per
  unit in `lastEnergy` so an idle unit reading 0 doesn't reset its `total_increasing` counter) **and**
  as a **system SUM per outdoor unit** (`outdoor_power`, `outdoor_energy_total`, … — `scope: outdoor`;
  energy sum never republished as 0). Values that are identical on every indoor unit are shared
  outdoor-unit readings shown **once** per outdoor unit (`scope: outdoor`, aggregated **max**):
  compressor + fan frequency, refrigerant temperature, outdoor temperature. **Faikin dependency:** the firmware's
  `ha.enable` gates *both* its own HA discovery *and* the AC fields in `state/<host>`
  (`revk_state_extra` returns early when off), so local reads require `ha.enable = true`; duplicate
  Faikin entities are avoided by redirecting its `topic.ha` prefix, not by disabling HA — see
  `docs/faikin-home-assistant.md`.
- **Multi-split / dependency engine** (`outdoor.go`): outdoor groups keyed by outdoor serial
  (`groupMembers`). `scope: outdoor` catalog entries (`outdoor_silent`, `econo_mode`,
  `demand_control`) dedup to **one entity per outdoor unit** (in `hass.entityIdentity`) and
  **fan out** writes to all members. Mode sync propagates heat/cool across the group (a standard
  multi-split can't cool+heat at once); powerful ⇄ econo are mutually exclusive. econo is
  `scope: outdoor` (it limits the shared compressor) but powerful stays per indoor unit: turning
  econo on clears powerful group-wide, and a powerful on **any** member suspends econo group-wide
  and restores it when the boost ends — manually or after the 20-min hardware timeout (the hardware
  does not restore it). This save/restore is an edge-driven, group-keyed state machine
  (`reconcileEconoSuspend` + `Coordinator.econoSuspend`) fed from both the local read path
  (`publishLocalState`) and the cloud poll (`reconcileEconoSuspendCloud`, skips locally-active
  groups). On by default; gated by `MULTISPLIT_MODE_SYNC` / `MULTISPLIT_OUTDOOR_AGGREGATE` /
  `ENFORCE_MUTUAL_EXCLUSIVE`.

The Faikin broker defaults to the main MQTT broker (connection reused); a distinct
`LOCAL_FAIKIN_SERVER` opens a second connection. The dependency engine runs **above** the backend
seam, so fan-out works through either cloud or local.

### i18n rules (`internal/catalog/localize.go`, `internal/web/assets/i18n/`)
`LANGUAGE` is `en` (default/fallback) or `de`. **Localized:** catalog/entity display names,
enum labels, web-UI text. **Not localized:** MQTT topics, entity_ids, characteristic keys, and
command *values*. **The one documented exception:** the climate fan/swing/preset dropdowns emit
the German *label as the command value* (HA's MQTT climate platform has no separate label/value
field), reversed on write by `canonicalAux` (see `coordinator/climate.go`).

### MQTT (`github.com/SukramJ/go-mqtt`, `github.com/SukramJ/go-mqtt/protocol`)
A **custom pure-Go MQTT 3.1.1 implementation** (no paho/library), extracted into the shared
`go-mqtt` module (formerly a locally duplicated `internal/mqtt`): packet codec in `protocol/`,
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
Local-first / multi-split keys (`LOCAL_MODE`, `LOCAL_FAIKIN_*`, `LOCAL_DEVICE_MAP`,
`MULTISPLIT_*`, `ENFORCE_MUTUAL_EXCLUSIVE`) are validated only when `LOCAL_MODE` is on; the
add-on surfaces them as options and `script/run.sh` maps them to env (`local_device_map` as a
list joined into the `id=host,…` scalar form).

## Testing conventions

Table-driven tests with stub `CloudClient`/`MQTT`/`TokenSource` (narrow interfaces) and an
injectable clock for rate-limit/scan-ignore logic. Golden-style assertions against JSON
fixtures under `internal/process/testdata` and `internal/daikin/model/testdata`. Run with
`-race` (what `make test` does).
