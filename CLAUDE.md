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

Go ≥ 1.27, `CGO_ENABLED=0`. Minimal deps (`golang.org/x/sync`, `yaml.v3`) — keep it that way.

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

## Which model does which work

Set by the owner for the go-fabric family on 2026-10-10 (go-fabric's
`CLAUDE.md`); the review split of 2026-11-08 (go-fabric #456) likewise.
Adopted here on 2026-11-08. A statement about the ONECTA cloud API, Faikin or Home Assistant discovery that
lands in this repo is cited later as fact — a comment that names a caller
or a consumer, a docstring that states what the other side does, a
`changelog.md` line — so the work that produces such statements gets
the strongest model, and the work a guard checks may use a cheaper one. The
agents in [Sub-Agent Delegation](#sub-agent-delegation) carry a default
model each; the rows below decide when a call overrides it (`model:` on the
Agent tool). Implementation defaults to Opus.

| Work | Model |
| --- | --- |
| Analysis, planning, reviews of design, audits; anything written into `notes/`, `changelog.md` or a comment that names a consumer, or a comment that names an ONECTA API, Faikin or Home Assistant discovery behaviour; OAuth2/PKCE and the token store (`internal/daikin/auth/`), the MQTT TLS and credential settings; *which* guard gets built and its bite line; rebases and merge conflicts where `main` may have overtaken the change | **Fable** (the default for this repo; also the `hunt` agent) |
| Implementation of a finished plan with guards behind it: a characteristic in `characteristics.yaml` once the pattern exists, a discovery field, a test on a recorded response, a `changelog.md` entry for a finished diff | **Opus** (the `impl` default; also what fast mode runs on) |
| **Review of a slice's pull request, split (owner decision 2026-11-08, go-fabric #456).** The code review — correctness, concurrency, error paths, tests, the fixed report shape — is **Opus**. On top of it **Fable** reads, bounded: every new comment or docstring that names an ONECTA API, Faikin or Home Assistant discovery behaviour against the ONECTA documentation, a recorded response, go-hamqtt at its tag, every line that enters `notes/`, `changelog.md` or a comment that names a consumer, the paths of the first row, and the brief's decisions against the reference — the behavioural gaps a code review does not see. The load-bearing claims of the Opus review are verified before they reach the implementer. | **Opus**, then **Fable** |
| Work a test or generator checks immediately: `make fmt`, a table case on an existing pattern; search and first reading of a tree whose result a stronger model or the owner reads again | **Sonnet** (the `guard` default; `impl` with `model: "sonnet"`) |
| Mechanical inventories whose answer is a list the caller can re-grep: which files reference X, which links are dead, which entries lack a counterpart | **Haiku** (`sweep` default) |

Sonnet never writes a claim about an ONECTA API, Faikin or Home Assistant discovery behaviour into the repo, and
never touches the paths named in the first row. Haiku returns lists, never
verdicts. When in doubt about which row a task is in, it is in the first.

---

## Sub-Agent Delegation

As go-fabric's and openccu-loom's rule (owner, 2026-10-10). The main
conversation owns planning, contract and wire decisions, guard
specifications, sub-agent steering, and final verification. It does not own
typing.

**Delegate what a gate can prove; keep what only a reader can accept.**
Delegate freely: table cases, fakes, scaffolding, fixture updates, doc and
link cleanups, version bumps across the changelog set, inventories and grep
sweeps. Never delegate: `internal/daikin/auth/`, the MQTT topic layout (mqtt-smarthome 2.0, ADR 0083), the discovery payloads, and *which* guard gets
built. Locating is delegable; reading is not.

| Agent | Default model | Use for |
|---|---|---|
| `impl` | Opus | scoped implementation with a stated acceptance command |
| `guard` | Sonnet | tests from a caller-written guard spec, plus the bite proof |
| `sweep` | Haiku | read-only inventories and grep sweeps, high fan-out |
| `hunt` | Fable | adversarial read-only defect hunt, ranked candidates |

Each default is one row of
[Which model does which work](#which-model-does-which-work); a call that
falls into another row passes `model:` on the Agent tool — `impl` with
`model: "sonnet"` for work a generator or formatter checks immediately.

**Size CPU-bound fan-out from the host, never from a constant** — the owner
works from a 4-core box and a 14-core box:

```sh
cores=$(nproc); agents=$(( (cores - 1) / share ))   # share = cores per agent
```

Check the 1-minute load first; if it is at or above the core count, run one
CPU-bound agent and queue the rest. Eight concurrent agents is a practical
ceiling whatever the core count. Every CPU-bound agent must **pin** its share
in the command it runs (`GOMAXPROCS=<share> go test -p <share> ./internal/x/...`, `golangci-lint run --concurrency=<share>`) — handing out a number without
pinning it changes nothing. Read-only agents cost no core worth counting;
6–8 is a practical ceiling there.

Two hard rules: **no sub-agent runs the full gate (`make check` (vet, fmt-check, lint, race tests)) or a
repo-wide lint** — the full gate runs once, in the main conversation, at the
end of the slice — and **no two writing agents in the same package**
(disjoint file sets, or `isolation: "worktree"`).

Every brief carries five things: the files and public surface plus one style
reference; the acceptance command the agent runs itself; the invariants it
touches; **stop conditions** (new dependency, API or wire change, credential
handling, an ADR-shaped decision — a stop is a successful outcome); and a
report format ≤ 250 words, large artefacts to the scratchpad as a path.

**A report is a claim.** Read the diff, run the gate yourself. For `guard`,
the acceptance artefact is not a green test but the **bite proof**: the named
production line removed, the test observed red with its message, the line
restored, green again.

---

## Token use

Set by the owner on 2026-10-14 ("Wichtig ist mir eine effiziente Nutzung
der Token"), as go-fabric's rule of the same date. A slice costs on
the order of half a million to a million tokens per implementing agent
before the owner's review; the rules below are what keeps that from
doubling.

- **Read slices, not files.** `sed -n a,bp`, `grep -n` with a tight
  pattern and `head`; a whole file only when the task is the file. Never
  re-read what is already in context; cite the earlier read.
- **Delegate first reads.** A source tree's inventory (go-hamqtt, go-mqtt, openccu-loom's ADRs, this repo)
  is a `sweep` or Sonnet read written to the scratchpad with `file:line`
  for every fact; the owner spot-checks the load-bearing lines and verifies
  every fact about the ONECTA cloud API, Faikin or Home Assistant discovery before it enters a brief or
  `notes/`, `changelog.md` or a comment that names a consumer — the agent's result is a map, never a citation.
- **Logs by their summary.** The gate (`make check` (vet, fmt-check, lint, race tests)) runs in the
  background with a summary file (one line per gate, `FAIL`/`ok` lines
  only); read the summary, and open a log only on failure, with `grep` for
  the failure markers and the error strings — never `cat`. The same for CI:
  the job log is fetched once to a file and grepped.
- **One turn, every independent call.** Reads, checks and background
  starts that do not depend on each other go out together; a wait is a
  `Monitor` or an `until` loop in the background, not polling by hand.
- **Short artefacts.** A brief names files, decisions and the acceptance
  command, not prose; a report follows the fixed shape of
  [Sub-Agent Delegation](#sub-agent-delegation) (≤ 250 words, large
  artefacts as a scratchpad path); status messages to the owner are short,
  outcome first, in German.
- **Skip what cannot have changed.** No regeneration for a comment change;
  no second gate run for a notes-only commit — say what was skipped and
  why.
- **Edit by script.** Several replacements in several files are one
  scripted call with asserted anchors, not a chain of interactive edits;
  a generated or formatted artefact is regenerated, never patched.
- **Agents read what the brief names.** An implementing agent gets the
  file list and the pinned sources in the brief, reads those, and reports
  in the fixed shape; it does not survey the tree. One agent per brief;
  two agents only on disjoint paths, at most one Opus agent at a time.
- **Push, pull request and tag are the session's own calls.** An
  implementing agent works locally — branch, commits, guards — and
  reports; the owner's session reads the diff, runs the gate, pushes,
  opens the pull request and tags the merge commit; the owner merges. A
  delegated push, pull request or tag is refused by the auto-mode
  classifier ("Create Public Surface", 2026-11-07), and the review in
  [Which model does which work](#which-model-does-which-work) sits
  between the agent's report and the push anyway.

---

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
- **Write path**: subscribes `<name>/set/+/+/+` at QoS 1 through go-hamqtt's
  `CommandRouter.HandleSet` (mqtt-smarthome §5.3: `{"val":x}` unwrapped, empty and retained
  payloads dropped), queues onto a channel, drains sequentially. Synthetic topics route to
  climate handlers; catalog topics resolve via `catalog.ByTopic`, coerce the value
  (`coerceWriteValue`: booleans in every §5.3 spelling → `on`/`off`, select token
  case-insensitive or a label → raw code, numbers rounded/clamped to the live range),
  substitute `{mode}` in the PATCH path from `modeCache`, then `cloud.Patch(...)`. Every
  rejected or failed set is logged at warn with topic and payload (`rejectWrite`). The
  climate fan/swing/preset label → token reversal lives in `climate.go` (`canonicalAux`).

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
  never reports, so they are only ever published via the local path. The same mechanism carries the
  per-unit **`faikin_restart` button**: not an AC setting but the firmware's own restart command
  (`command/<host>/restart`, revk `revk_command("restart")`, payload ignored) — `handleWrite` branches
  on the topic before the catalog lookup (`handleFaikinRestart`), forwards the `PRESS` payload to the
  module and never touches the cloud; unmapped devices are rejected. Telemetry placement follows the
  physics (verified on a live multi-split): power and lifetime energy are each indoor unit's **own**
  reading → published **per indoor unit** (`energy_total`, `power_consumption`; energy **held** per
  unit in `lastEnergy` so an idle unit reading 0 doesn't reset its `total_increasing` counter) **and**
  as a **system SUM per outdoor unit** (`outdoor_power`, `outdoor_energy_total`, … — `scope: outdoor`;
  energy sum never republished as 0). The fan and the liquid-line temperature are each unit's own too:
  `fan_speed` per indoor unit in rpm (= Faikin `fanfreq` Hz × 60; the S21 `RL` report is the
  responding unit's own fan) and `refrigerant_temperature` per indoor unit (its own coil reading) —
  0.8.2 fixed the earlier outdoor-shared misclassification of both. Values that are identical on
  every indoor unit are shared outdoor-unit readings shown **once** per outdoor unit
  (`scope: outdoor`, aggregated **max**): compressor frequency, outdoor temperature. **Faikin dependency:** the firmware's
  `ha.enable` gates *both* its own HA discovery *and* the AC fields in `state/<host>`
  (`revk_state_extra` returns early when off), so local reads require `ha.enable = true`; duplicate
  Faikin entities are avoided by redirecting its `topic.ha` prefix, not by disabling HA — see
  `docs/faikin-home-assistant.md`.
- **Multi-split / dependency engine** (`outdoor.go`): outdoor groups keyed by outdoor serial
  (`groupMembers`). `scope: outdoor` catalog entries (`outdoor_silent`, `econo_mode`,
  `demand_control`) dedup to **one entity per outdoor unit** (in `hass.entityIdentity`) and
  **fan out** writes to all members. Mode sync resolves heat/cool conflicts across the group (a
  standard multi-split can't cool+heat at once), **last write wins** — but only members *known to
  be running* in the opposite compressor direction (`heating` vs `cooling`/`dry`) are switched;
  off/unknown units are **never** written to (the Faikin `mode` command force-powers a unit on, so
  a blind sync would switch on the whole house). Power/mode come from `powerCache`/`modeCache`,
  fed by cloud poll + Faikin state + successful writes (`noteWrite`); for locally-mapped devices
  the lagging cloud poll only bootstraps missing entries. powerful ⇄ econo are mutually exclusive. econo is
  `scope: outdoor` (it limits the shared compressor) but powerful stays per indoor unit: turning
  econo on clears powerful group-wide, and a powerful on **any** member suspends econo group-wide
  and restores it when the boost ends — manually or after the 20-min hardware timeout (the hardware
  does not restore it). This save/restore is an edge-driven, group-keyed state machine
  (`reconcileEconoSuspend` + `Coordinator.econoSuspend`) fed from both the local read path
  (`publishLocalState`) and the cloud poll (`reconcileEconoSuspendCloud`, skips locally-active
  groups). **Eco readback latch:** only a *running* unit reports econo on the serial bus — a
  standby unit executes the command but its status keeps reading econo off (Faikin gives up with
  `failed-set`), so `localOutdoorAgg` takes econo from the running members when any member runs
  (refreshing `Coordinator.econoLatch`) and from the latch (also fed by econo writes via
  `noteWrite`) when the whole group is off; otherwise the HA switch snapped back to off after the
  2-min outdoor hold. On by default; gated by `MULTISPLIT_MODE_SYNC` /
  `MULTISPLIT_OUTDOOR_AGGREGATE` / `ENFORCE_MUTUAL_EXCLUSIVE`.

The Faikin broker defaults to the main MQTT broker (connection reused); a distinct
`LOCAL_FAIKIN_SERVER` opens a second connection. That second connection is always plain
`tcp://` — `MQTT_TLS` covers the main broker only. With `MQTT_TLS` on and both
`LOCAL_FAIKIN_SERVER`/`LOCAL_FAIKIN_PORT` unset, the Faikin port defaults to `MQTT_PORT`
(not 1883), so the TLS connection is shared instead of a plaintext one opened to the same host. The dependency engine runs **above** the backend
seam, so fan-out works through either cloud or local.

### Weekly schedules (`internal/schedule/`, `internal/coordinator/schedule.go`)
Opt-in via `SCHEDULE_ENABLE`. A seven-day programme persisted to `schedules.json`
(atomic, `0600`, same pattern as `auth.Store`), edited in the web UI's calendar and
switchable from HA. Full rationale in `docs/schedule-design.md`.

- **The week is a ring of 336 half-hour slots** (`ring.go`). A block whose `end` is not
  after its `start` wraps modulo `SlotsPerWeek`, which covers both midnight and the
  Sunday→Monday seam with no special case. Per slot the highest `priority` wins; at
  equal priority the block that started more recently. `Segments` collapses equal
  neighbours into the effective blocks the calendar draws.
- **Writes happen at switch points only** (`engine.go`). A single `time.Timer` is armed
  on the next switch point computed as *wall-clock* time (`time.Date` in the configured
  zone), so DST shifts the schedule with the clock. Two mechanisms keep "manual wins
  until the next block": an idempotence cache per device (an action equal to the last
  applied one is not rewritten — this also absorbs the doubled hour in autumn) and the
  **catch-up window** (`SCHEDULE_CATCHUP`, default 30 min): apply only when the block
  start is at most that long ago. On a timer tick the age is ~0; a daemon restarted deep
  inside a block records the state without writing. A **gap clears** the cache, because
  after "no intervention" the same target state is a fresh instruction again.
- **No second write path.** `Coordinator.ApplySchedule` produces the same `writeReq`
  values an inbound `/set` produces and feeds the same channel, so multi-split mode sync,
  the local/cloud backend seam and the cloud lock all apply. **Order is load-bearing:**
  `hvac_mode` before `temperature_setpoint`, because the setpoint path contains `{mode}`
  resolved from `modeCache`, which `noteWrite` fills. All three scheduled characteristics
  are modelled by `faikinCommand`, so a mapped device is driven entirely locally (no
  ONECTA quota, no `scan_ignore` window). A device the poll has not resolved yields
  `ErrDeviceUnknown` ("not yet"); each completed poll calls `engine.Wake()`.
- **Two schedule types.** `Schedule.Type` is `indoor` (default, and what a 0.9.x file
  omits) or `outdoor`. An outdoor schedule targets an outdoor unit by serial and its
  blocks set `outdoor_silent` / `econo` / `demand` (each optional: unset = leave alone);
  validation rejects the fields and targets of the other type. `Target.Key()` maps both
  onto one opaque string (`outdoor:<serial>`), which is why the ring, priorities,
  catch-up and idempotence are literally the same code for both. Outdoor writes go to
  **one** group member — the `scope: outdoor` topics already fan out in `handleWrite`,
  so looping here would write everything twice.
- **HA:** one switch per schedule on the daemon's own device (`daikin_scheduler`), plus
  the per-device sensors `schedule_state` / `schedule_next_change` (synthetic catalog
  entries like the refresh button). The switch item
  `<name>/set/scheduler/<id>/enabled` fits the one `<name>/set/+/+/+` filter, so
  `handleWrite` just branches on the reserved item `scheduler` — no second
  subscription. Schedule configs join the published set, so orphan reconcile clears a
  deleted schedule.
- **Slugs are frozen.** A schedule's id is derived from its name once at creation (same
  transliteration as `hass.slugify`: `ä→a`, not `ae`) and never changes again — renaming
  edits only `name`, so `switch.daikin_schedule_<slug>` survives it. `main` imports
  `_ "time/tzdata"`: the distroless image has no system zone database.

### i18n rules (`internal/catalog/localize.go`, `internal/web/i18n/`, `internal/web/assets/tools-i18n/`)
`LANGUAGE` is `en` (default/fallback) or `de`. **Localized:** catalog/entity display names,
enum labels, web-UI text. **Not localized:** MQTT topics, entity_ids, characteristic keys, and
command *values*. Schedules add a third category: a schedule's `name` and a block's `label` are **user
data** — no `name_de`, published verbatim in every language. The only daemon-produced
string in that path is the idle state of `schedule_state`, which is a catalog enum value
and therefore localized like every other label. The scheduler UI takes mode names from
the existing `operation_mode` catalog entry (served via `/api/schedules`) and weekday /
time formatting from `Intl`, so no second translation table exists; API errors carry a
stable `code` the SPA resolves as `sched.err.<code>`. The week starts on Monday in both
languages (the stored day keys are Monday-based). `TestI18nBundlesCoverEveryKey` enforces
that both bundles carry the same keys and that every key referenced from the HTML or JS
exists (tools page); `TestDashboardBundlesMatch` does the same for the dashboard's labels.

**Tokens on the wire, labels in discovery** (since 0.14, openccu-loom ADR 0083): every
enum status item — selects, `schedule_state`'s `idle`, the climate fan/swing/preset — carries
the API token; the discovery payload lists the localized labels and maps token ↔ label in its
value/command templates (`go-hamqtt` does it for selects under `StatusObjectEncoding`; the
climate builder and the schedule sensor state theirs). The set path still accepts a label
(`CodeForLabel` / `canonicalAux`).

### MQTT (`github.com/SukramJ/go-mqtt`, `github.com/SukramJ/go-mqtt/protocol`)
A **custom pure-Go MQTT 3.1.1 implementation** (no paho/library), extracted into the shared
`go-mqtt` module (formerly a locally duplicated `internal/mqtt`): packet codec in `protocol/`,
TCP adapter, and a `Lifecycle` wrapper that auto-reconnects with backoff and re-fires
`OnConnect` (used to re-announce `connected`, `info` and the cached status). Narrow
`Publisher`/`Subscriber`/`Client` interfaces make the coordinator and discovery testable with
stubs.

### Topic layout (mqtt-smarthome 2.0, openccu-loom ADR 0083, since 0.14)
```
<name>/status/<deviceID>/<embeddedID>/<topic>             # retained {"val","ts","lc"}, QoS 0
<name>/status/<deviceID>/<embeddedID>/<topic>/attributes  # data_source document in val
<name>/set/<deviceID>/<embeddedID>/<topic>                # subscribed QoS 1 (refresh: action item)
<name>/status/<deviceID>/online                           # device reachability (bool)
<name>/{status,set}/scheduler/<scheduleID>/enabled        # per-schedule enable switch
<name>/connected                                          # 0 will/stop, 1 upstream down, 2 operational
<name>/info, <name>/maintenance/{set/loglevel,set/restart,stats}
homeassistant/device/<node id>/config                     # HA device documents, retained
```
`<name>` is `MQTT_TOPIC` (default `daikin`). All of it is composed in `internal/layout`
(`layout.Root`); `layout.Legacy` is the 0.13 tree, read only by the start-up sweep
(`coordinator/legacy_sweep.go`, exact owned shapes only) and by the frozen per-entity
discovery builders the retraction goldens are pinned against. `connected` is driven by
`coordinator/upstream.go` (cloud: last poll; local mode: Faikin link + known topology).
Maintenance (`MQTT_MAINTENANCE`, `MQTT_STATS_INTERVAL`) is go-hamqtt's `publisher.Instance`,
wired in `main` with the daemon's `slog.LevelVar` and its signal-context cancel; restart is
refused unless `publisher.DetectSupervised("DAIKIN_SUPERVISED")`. The HA identities
(`unique_id`, node ids, device identifiers) must not move — `TestHAIdentityUnchangedByTheTopicMigration`
pins them against v0.13.0.

### Web UI (`internal/web/`, `internal/state/`)
Optional (`WEB_ENABLE`, default off, bind `127.0.0.1:8080`, optional basic auth). Two pages
behind one server and one auth:
- **The dashboard (`/`) is go-bridgeui**, the one every go-*2mqtt bridge embeds.
  `adapter.go` maps `internal/state` onto its contract: every ONECTA device a device, every
  published point an entry (grouped climate / hot water / energy / schedule / local /
  diagnostics), every settable number/select/switch/button an item. The store is fed by the
  coordinator — `recordPoints` describes the points of each poll, `publishState` hands it
  every published status item (cloud, Faikin and scheduler alike) — so the dashboard shows
  what MQTT carries and costs no cloud request. A write goes through
  `Coordinator.Submit`, i.e. the same queue, validation and PATCH path as an MQTT `set`.
- **The tools page (`tools.html`)**, a vanilla embedded page (`//go:embed`, no build step),
  registered through go-bridgeui's `Options.Routes` and linked from the dashboard's
  navigation: OAuth login, raw cloud device tree, weekly-schedule editor, manual PATCH
  test, rate limit. Its config route is `/api/tools/config` (`/api/config` is the
  dashboard's). The routes that wait on the cloud extend their write deadline (`slow`).

The same server hosts the OAuth `/callback` (it returns to `tools.html`), so no inbound
port forwarding is needed. **HA-ingress aware** (relative asset URLs; can derive the
OAuth redirect URI from the request behind ingress).

## Config

Flat YAML (`config-template.yaml` documents every key). Every key is overridable by a
`DAIKIN_<KEY>` env var (bool/int/float coerced; else string). Loader pipeline: file → env
override → defaults → validate. Required: `DAIKIN_CLIENT_ID`/`SECRET`, `MQTT_SERVER`. With
credentials missing the daemon starts **idle** (web UI reachable for setup) rather than crashing.
Local-first / multi-split keys (`LOCAL_MODE`, `LOCAL_FAIKIN_*`, `LOCAL_DEVICE_MAP`,
`MULTISPLIT_*`, `ENFORCE_MUTUAL_EXCLUSIVE`) and the scheduler keys (`SCHEDULE_ENABLE`,
`SCHEDULE_STORE_PATH`, `SCHEDULE_TIMEZONE`, `SCHEDULE_CATCHUP`) are validated only when
their feature switch is on; the
add-on surfaces them as options and `script/run.sh` maps them to env (`local_device_map` as a
list joined into the `id=host,…` scalar form). `MQTT_MAINTENANCE` (default true) and
`MQTT_STATS_INTERVAL` (seconds, default 60, 0 = off) are `*bool`/`*int` so an unset key keeps
its default; `MQTT_TLS`/`MQTT_TLS_CA`/`MQTT_TLS_INSECURE` select TLS to the main broker (port
default 8883 when TLS is on and `MQTT_PORT` unset; CA + insecure is refused; the tls.Config is
built at startup in `mainBrokerConfig`); `MQTT_TOPIC` is refused when it holds a wildcard, NUL or a leading `$`.

## Testing conventions

Table-driven tests with stub `CloudClient`/`MQTT`/`TokenSource` (narrow interfaces) and an
injectable clock for rate-limit/scan-ignore logic. Golden-style assertions against JSON
fixtures under `internal/process/testdata` and `internal/daikin/model/testdata`. Run with
`-race` (what `make test` does).
