# ADR 0070 phase 8 — measurement for go-daikin2mqtt

- Status: measurement, not a decision
- Date: 2026-09-13
- Subject: [ADR 0070](https://github.com/SukramJ/openccu-loom/blob/main/docs/adr/0070-shared-ha-discovery-model-module.md)
  and its rollout table, row *"8 | `go-daikin2mqtt` (846) | Proves composite
  entities and multi-source fusion"*
  (`notes/concepts/shared-ha-discovery-model.md:870`)
- Measured against: this repository at `origin/main` (`9deee36`),
  `github.com/SukramJ/go-hamqtt` v0.32.0 (`7efac2f`),
  `github.com/SukramJ/go-mqtt` v1.3.0 (consumed before this PR) / v1.5.1
  (required by go-hamqtt, and bumped to in this PR)
- Precedents: `go-zendure2mqtt`'s `docs/adr0070-pilot-measurement.md` (phase 5),
  `go-mtec2mqtt`'s `notes/adr0070-phase6-measurement.md` (phase 6, findings
  F1–F11) and `go-homeconnect2mqtt`'s `notes/adr0070-phase7-measurement.md`
  (phase 7, findings F1–F12)

This document measures what go-daikin2mqtt actually publishes to an MQTT broker
today: every discovery payload, every topic, every identity string, the
availability model, the delivery guarantee, and the places where the same topic
is composed more than once. It takes no decision. It states, per item, what was
measured and how.

No production Go file in this repository was modified by the measurement. What
this PR adds is the *pins* — two test files and their goldens, plus a
`.gitattributes` and the `go-mqtt` bump — because the precedent from phases 5, 6
and 7 is that defects are corrected in their own step *before* the migration, so
the later byte-equality proof compares against corrected bytes.

> **Why `notes/`.** `docs/` in this repository is operator- and
> design-facing (`docs/design.md`, `docs/schedule-design.md`,
> `docs/faikin-home-assistant.md`, `docs/api/`) and ships in the release
> archive's neighbourhood. This is a working document for one programme,
> superseded when phase 8 ends. Phases 6 and 7 made the same call; the
> phase-5 pilot put its equivalent in `docs/` and both successors said in
> writing that they would not.

---

## 1. What is actually there

### 1.1 The whole repository

```sh
git ls-files '*.go' | grep -v _test.go | xargs cat | wc -l   # 10594
git ls-files '*_test.go'                | xargs cat | wc -l   #  8886  (at origin/main)
wc -l < characteristics.yaml                                  #   688
```

| Package | Go non-test | Go test |
| --- | ---: | ---: |
| `cmd/daikin2mqtt` | 302 | 95 |
| `cmd/daikin2mqtt-util` | 482 | 0 |
| `internal/catalog` | 364 | 311 |
| `internal/config` | 792 | 258 |
| `internal/coordinator` | 2 900 | 2 885 |
| `internal/daikin/auth` | 538 | 233 |
| `internal/daikin/client` | 514 | 424 |
| `internal/daikin/model` | 301 | 295 |
| `internal/faikin` | 131 | 96 |
| **`internal/hass`** | **838** | **648** |
| `internal/process` | 307 | 281 |
| `internal/schedule` | 1 622 | 1 740 |
| `internal/version` | 28 | 0 |
| `internal/web` | 1 475 | 1 620 |

This is by a wide margin the largest bridge in the programme: 10 594 non-test Go
lines against homeconnect's 8 403 and mtec's 6 722, and the only one with a
second data *source* (the local Faikin MQTT interface) and a second *writer*
(the weekly scheduler) feeding the same published surface.

### 1.2 The 846 LOC figure

The rollout table says 846 and attributes it to `internal/hass`. Measured:

```sh
cat internal/hass/discovery.go internal/hass/climate.go internal/hass/schedule.go | wc -l   # 838
```

846 was `internal/hass` before `#74` dropped the dead `object_id` key. Right
about what it covers and eight lines stale — the same cause as zendure's 375,
mtec's 591 and homeconnect's 716.

It is also, as in every prior phase, about half the real addressable surface.
Measured by brace-matched function extents:

| Where | Lines | What it owns |
| --- | ---: | --- |
| `internal/hass/{discovery,climate,schedule}.go` | 838 | every discovery payload and the four public topic builders |
| `internal/coordinator/coordinator.go` (12 funcs) | 254 | bridge status, state publish, HVAC-mode synthesis, discovery gating, orphan reconcile, attributes, the `/set` subscription |
| `internal/coordinator/local.go` (7 funcs) | 226 | the Faikin read path's republish onto the same state topics, local-only discovery points, outdoor aggregation |
| `internal/coordinator/schedule.go` (9 funcs) | 140 | the scheduler's own state plane and discovery points |
| `internal/coordinator/climate.go` (4 funcs) | 84 | the synthetic fan/swing/preset state topics and their reverse mapping |
| `internal/coordinator/refresh.go` (1 func) | 22 | the synthetic refresh button's discovery point |
| `internal/coordinator/coordinator.go` `pollOnce` publish loop | ~30 | the per-point state publish |
| `cmd/daikin2mqtt/main.go` wiring | ~25 | `hass.New`, the LWT topic, the will |
| **Total** | **≈ 1 619** | |

**1 619, not 846 — a factor of 1.9**, in line with zendure (1.8×), mtec (1.6×)
and homeconnect (1.8×). Its tests, before this PR, are ≥ 1 500 lines
(`internal/hass` 648 plus the discovery- and topic-touching parts of
`internal/coordinator`'s 2 885).

### 1.3 Dependency state

```
require golang.org/x/sync v0.23.0
require gopkg.in/yaml.v3 v3.0.1
require github.com/SukramJ/go-mqtt v1.3.0     -> bumped to v1.5.1 in this PR
```

No `go-hamqtt`, no `go-ha-catalog`. go-hamqtt v0.32.0 requires go-mqtt v1.5.1,
so the bump is a prerequisite and is taken here, alone, where it can be seen to
move nothing: the pins were generated before it and pass after it.

`internal/mqtt` is gone — this repo already consumes the extracted `go-mqtt`.

**Subscriptions the daemon makes:**

| Filter | QoS | Lifetime | Source |
| --- | ---: | --- | --- |
| `daikin/+/+/+/set` | 0 | permanent | `coordinator.go:510` |
| `homeassistant/+/+/config` | 0 | transient (2 s, per reconcile) | `coordinator.go:437` via `Discovery.ConfigFilter` |
| `state/<faikin host>` | 0 | permanent, per mapped device | `local.go:185` via `faikin.StateTopic` |

Note that `HASS_ENABLE` defaults to **false** (`internal/config/defaults.go:31`):
discovery is opt-in on this bridge, unlike the state plane.

---

## 2. What it publishes

### 2.1 The shape

Retained JSON at `<HASS_BASE_TOPIC>/<platform>/<unique_id>/config`, one config
per entity, built by three separate builders:

| Builder | File | Produces |
| --- | --- | --- |
| `Discovery.buildConfig` | `internal/hass/discovery.go:350` | every catalog-derived entity (6 platforms) |
| `Discovery.buildClimate` | `internal/hass/climate.go:187` | the composite `climate` entity |
| `Discovery.buildScheduleConfig` | `internal/hass/schedule.go:333` | one `switch` per weekly schedule |

State at `<MQTT_TOPIC>/<deviceID>/<embeddedID>/<topic>/state`, commands at the
same path with `/set`, a per-entity `…/attributes` sibling carrying
`{"data_source":"cloud"|"local"}`, and the bridge LWT at
`<MQTT_TOPIC>/bridge/status`.

### 2.2 The entity census

Measured by running `Coordinator.pollOnce` against the four shipped ONECTA
fixtures plus a synthesised two-indoor multi-split, with a recorder in place of
the broker — the real builders, the real `characteristics.yaml`. Numbers held as
Go literals in `TestSurfaceCensus`.

| Scenario | Entities | Distinct messages | Platform mix |
| --- | ---: | ---: | --- |
| `airpurifier` (en = de) | 8 | 26 | binary_sensor 2, select 1, sensor 4, switch 1 |
| `d2cnd-gas-boiler` (en = de) | 13 | 46 | binary_sensor 5, button 1, climate 1, sensor 5, switch 1 |
| `air-to-air-dx4` (en = de) | 16 | 58 | binary_sensor 2, button 1, climate 1, sensor 11, switch 1 |
| `altherma-air-to-water-wlan` (en = de) | 20 | 68 | binary_sensor 2, button 1, climate 1, number 2, sensor 11, switch 3 |
| **multi-split, 2 indoor units** | **30** | 113 | binary_sensor 4, button 1, climate 2, sensor 21, switch 2 |
| multi-split + scheduler (2 schedules) | 38 | 142 | binary_sensor 4, button 1, climate 2, sensor 27, switch 4 |
| **multi-split + local-first** | **52** | 219 | binary_sensor 4, button 1, climate 2, number 1, sensor 38, switch 6 |

The catalogue behind it: **55 entries** in `characteristics.yaml` (30 sensor, 10
binary_sensor, 7 switch, 5 number, 2 select, 1 button), matching 4 management
point types (43 climateControl, 6 domesticHotWaterTank, 4 outdoorUnit, 3
gateway), 15 settable, 12 with `scope: outdoor`, zero disabled, zero duplicate
`topic:` values.

The entity count **does not move with LANGUAGE** in any scenario — only the
display `name`, a `select`'s `options` and (see F2) two device names do.

Three of those entities per installation are *synthetic*: they have no cloud
characteristic at all and reach Home Assistant only through a coordinator-side
point synthesiser — the refresh button (`refresh.go:54`), the two schedule
status sensors (`schedule.go:307`) and, in local mode, up to eleven local-only
entities (`local.go:130`). This is the shape mtec's F7 and homeconnect's F6 both
found a defect in; here it is measured and pinned rather than assumed benign.

### 2.3 The topic form — **four segments, no node id**

```
homeassistant/sensor/daikin_809d41d9-4d42-45fa-af6a-84b512143672_room_temperature/config
homeassistant/climate/daikin_809d41d9-4d42-45fa-af6a-84b512143672_climate/config
homeassistant/switch/daikin_schedule_werktag/config
homeassistant/button/daikin_outdoor_ODU0000000001_refresh/config
```

Verified against **all 264 config topics** across the twelve pinned scenarios by
`TestConfigTopicForm`: 264 of 264 are exactly four segments,
`<prefix>/<platform>/<unique_id>/config`, with the third segment byte-equal to
the payload's own `unique_id`. `fmt.Sprintf` at `discovery.go:401`,
`climate.go:249` and `schedule.go:359`; the reconcile filter is
`homeassistant/+/+/config` (`discovery.go:346`).

This matches zendure and mtec and **inverts homeconnect**. It is the single most
consequential fact for step 6: `publisher.SupersededTopics` defaults to
`LegacyTopicWithNodeID`, the five-segment form, which would retract **none** of
these. This bridge needs `publisher.LegacyTopicByUniqueID` stated explicitly —
and `Config.LegacyEntityTopics` *replaces* the default rather than extending it,
so stating it is the whole fix. See [F5](#f5).

### 2.4 Two measured payloads, verbatim

A catalog-derived sensor (`multisplit.en`, decoded from the golden):

```json
{
  "name": "Room temperature",
  "default_entity_id": "sensor.wohnzimmer_room_temperature",
  "unique_id": "daikin_809d41d9-4d42-45fa-af6a-84b512143672_room_temperature",
  "state_topic": "daikin/809d41d9-4d42-45fa-af6a-84b512143672/climateControl/room_temperature/state",
  "unit_of_measurement": "°C",
  "device_class": "temperature",
  "state_class": "measurement",
  "availability_topic": "daikin/bridge/status",
  "payload_available": "online",
  "payload_not_available": "offline",
  "json_attributes_topic": "daikin/809d41d9-4d42-45fa-af6a-84b512143672/climateControl/room_temperature/attributes",
  "device": {
    "identifiers": ["daikin_809d41d9-4d42-45fa-af6a-84b512143672"],
    "name": "Wohnzimmer",
    "manufacturer": "Daikin",
    "configuration_url": "https://onecta.daikineurope.com"
  }
}
```

The composite climate entity — the thing phase 8 is meant to prove — consumes
seven slots and suppresses the three individual controls it replaces
(`climate.go:179-181`):

```json
{
  "name": "Thermostat",
  "default_entity_id": "climate.wohnzimmer_thermostat",
  "unique_id": "daikin_809d41d9-4d42-45fa-af6a-84b512143672_climate",
  "modes": ["off", "heat", "cool", "heat_cool", "dry", "fan_only"],
  "mode_state_topic":   "daikin/<dev>/climateControl/hvac_mode/state",
  "mode_command_topic": "daikin/<dev>/climateControl/hvac_mode/set",
  "temperature_state_topic":   "daikin/<dev>/climateControl/temperature_setpoint/state",
  "temperature_command_topic": "daikin/<dev>/climateControl/temperature_setpoint/set",
  "current_temperature_topic": "daikin/<dev>/climateControl/room_temperature/state",
  "fan_mode_state_topic": "daikin/<dev>/climateControl/fan_mode/state",
  "swing_mode_state_topic": "daikin/<dev>/climateControl/swing_mode/state",
  "preset_mode_state_topic": "daikin/<dev>/climateControl/preset_mode/state",
  "availability_topic": "daikin/bridge/status",
  "device": { "identifiers": ["daikin_<dev>"], … }
}
```

Five of its seven slots (`hvac_mode`, `fan_mode`, `swing_mode`, `swing_h_mode`,
`preset_mode`) are *synthetic*: backed by no catalog entry and by no register,
composed inline in `coordinator/climate.go:305` and `coordinator.go:324`. This
is exactly the class ADR 0070 §3.5 says must need no runtime special case, and
exactly the class that made the builder count diverge in both prior phases.

### 2.5 The whole topic tree

For the two-indoor multi-split in local mode — the richest installation measured
— 219 distinct retained messages on 219 topics:

| Plane | Topics |
| --- | ---: |
| discovery configs (`homeassistant/…/config`) | 52 |
| entity state (`…/state`) | 98 |
| entity attributes (`…/attributes`) | 68 |
| bridge status (`daikin/bridge/status`) | 1 |

Of those, 13 state topics and 16 attributes topics are read by no published
entity (see [F11](#f11)), and 2 advertised state topics are written by nobody
(see [F6](#f6)).

### 2.6 Retain and QoS

**No retain defect.** Every single publish this daemon makes is
`mqtt.QoS0, retain=true`, at all fifteen call sites — verified against all 1 083
recorded publishes by `TestPublishQoSAndRetain`:

```sh
grep -rn '\.Publish(' --include='*.go' internal cmd | grep -v _test.go   # 15 sites
```

Thirteen pass `mqtt.QoS0, true`. The two exceptions are the *outbound Faikin
command* topics (`climate.go:91`, `backend.go:37`), correctly `retain=false` — a
retained command would re-fire on every reconnect. The LWT
(`cmd/daikin2mqtt/main.go:113`) sets `Retain: true` and no QoS field, so QoS 0.

QoS is where the migration has a silent trap: `publisher.QoS`'s zero value is
`QoSUnset` and resolves to **QoS 1**, not 0. Every one of these fifteen sites
must be spelled `publisher.QoSAtMostOnce` (`0x80`) or the whole surface silently
moves to QoS 1. See [F9](#f9). This is homeconnect's F9 in the same shape; the
pin records what the transport call passes, not what a constant is named.

---

## 3. Identity — what the library can reproduce byte for byte

Home Assistant keys the entity registry on `(domain, platform, unique_id)` and
the device registry on `identifiers`. Neither has a migration path. These are
the strings phase 8 may not change by accident.

### 3.1 The five strings

| String | Formula | Source |
| --- | --- | --- |
| `unique_id`, main device | `sanitize("daikin_" + deviceID + "_" + topic)` | `discovery.go:218` |
| `unique_id`, shared gateway | `sanitize("daikin_gateway_" + gwSerial + "_" + topic)` | `discovery.go:201` |
| `unique_id`, shared outdoor | `sanitize("daikin_outdoor_" + odSerial + "_" + topic)` | `discovery.go:192`, `:213` |
| `unique_id`, climate | `sanitize("daikin_" + deviceID + "_climate")` | `climate.go:188` |
| `unique_id`, schedule | `sanitize("daikin_schedule_" + scheduleSlug)` | `schedule.go:337` |
| `device.identifiers[0]` | `"daikin_" + deviceID` / `"daikin_gateway_" + s` / `"daikin_outdoor_" + s` / `"daikin_" + deviceID + "_outdoor"` / `"daikin_scheduler"` | `discovery.go:90,122,146`, `schedule.go:271` |
| `default_entity_id` | `<platform> + "." + collapseTokens(slugify(device.Name + "_" + topic))` | `discovery.go:353`, `:457` |
| state topic | `<root>/<deviceID>/<embeddedID>/<topic>/state` | `discovery.go:277` |
| command topic | the same with `/set` | `discovery.go:282` |

Three properties worth naming, because two of them are *better* than the
precedents:

1. **The `daikin_` namespace is a compile-time literal**
   (`discovery.go:90`), not a configurable MQTT root. Changing `MQTT_TOPIC`
   orphans nothing. This is mtec's strength and zendure's documented defect (ADR
   0070 §2.2); daikin has it right.
2. **Identity carries no display name** for a main-device entity: the device id
   is an ONECTA UUID and the topic is English. `unique_id` is fully reproducible
   by a `discovery.Context` that supplies these formulas.
3. **`default_entity_id` does carry the device name**, and for the two shared
   sub-device kinds that name is *localized*. See [F2](#f2) — this is a
   confirmed violation of the invariant this repo's own `CLAUDE.md` states, and
   it is a live one, not latent.

`sanitize` (`discovery.go:462`) is a **third** normaliser beside `hass.slugify`
and `schedule.Slug`: it preserves case and maps anything outside `[A-Za-z0-9_-]`
to `_`. It is what keeps the ONECTA UUID's hyphens in `unique_id` while
`slugify` would fold them. Any `discovery.Context` for this bridge must
reproduce all three.

### 3.2 Duplicate `unique_id`s — **zero**

`TestNoDuplicateEntityRegistryKeys`, across all twelve scenarios and all 264
configs: zero duplicates on `unique_id` alone, zero on `(platform, unique_id)`,
zero on `default_entity_id`. mtec had nine; homeconnect had zero; daikin has
zero. The phase inherits no duplicate question at all, and the go-hamqtt v0.32.0
validator's `(platform, unique_id)` rule costs this bridge nothing.

The near-miss worth recording: the `scope: outdoor` catalogue entries would
produce one entity *per indoor unit* for a knob that physically exists once.
`Discovery.Publish`'s `seen[uid]` gate (`discovery.go:317`) collapses them
because `entityIdentity` keys them on the outdoor serial. That gate is what
keeps the count at zero — and it is also what makes [F7](#f7) possible.

### 3.3 Slug agreement — measured over the real catalogue

`TestSlugAgreementOverTheRealCatalogue`, with go-hamqtt v0.32.0's `topic.Slug`
transcribed verbatim beside this bridge's `slugify`.

**Catalog topics: 0 of 55 diverge.** Every `topic:` in `characteristics.yaml` is
lower-case ASCII snake_case, so both functions are the identity on them. The
entity half of every id is safe.

**Device names: 8 of 9 probes diverge.**

| Device name | `hass.slugify` | `topic.Slug` |
| --- | --- | --- |
| `Wohnzimmer` | `wohnzimmer` | `wohnzimmer` |
| `Küche` | `kuche` | `kueche` |
| `Büro` | `buro` | `buero` |
| `Außengerät` | `aussengerat` | `aussengeraet` |
| `Gäste-WC` | `gaste_wc` | `gaeste-wc` |
| `Café` | `caf` | `cafe` |
| `EG-Wohnzimmer` | `eg_wohnzimmer` | `eg-wohnzimmer` |
| `ÜÄÖ` | `uao` | `ueaeoe` |
| `` (empty) | `` | `x` |

Two independent divergence classes: the umlaut expansion (`ä→a` vs `ä→ae` — this
bridge deliberately matches Home Assistant's own slugify, and says so at
`discovery.go:410`) and the hyphen (folded vs preserved).

Unlike homeconnect, this divergence does **not** reach `unique_id` or
`device.identifiers` — those are built from the ONECTA device id and component
serials, which are ASCII. It reaches **`default_entity_id` only**. That is still
the seed Home Assistant uses for the entity id it registers, and HA does not
rename a registered entity, so a swap would strand every entity on a
German-named device. See [F4](#f4). This is a materially cheaper version of
homeconnect's F10, because the device registry is not at risk — only the entity
*ids*, and only on installations with non-ASCII device names.

### 3.4 What I could not determine

- **Whether real Faikin firmware sends `fan: "auto"|"1".."5"|"quiet"` or
  `"auto"|"low"|"medium"|…`.** `internal/faikin/faikin.go:34` documents the
  former; `coordinator/climate.go:49`'s `faikinFanToCloud` is keyed on the
  latter. The only captured real state in the repository
  (`internal/faikin/faikin_test.go:12`, `internal/coordinator/local_test.go:23`)
  has `"fan":"auto"`, which both readings accept. [F6](#f6) is therefore stated
  as a defect *candidate*. ***Settled by:*** one `mosquitto_sub -t 'state/#'`
  against a live Faikin module with the fan set to a numbered speed.
- **Whether ONECTA's `GET /devices` array order is stable across calls.**
  [F7](#f7)'s consequence depends on it. Nothing in this repository asserts it
  either way, and the ONECTA OpenAPI document (`docs/api/`) does not specify an
  ordering. ***Settled by:*** logging the device-id sequence over a day of real
  polls.
- **Whether a Home Assistant device bundle may carry a `climate` component
  alongside the state topics of entities it suppresses.** The suppression is
  this bridge's own (`climate.go:179`), so the bundle would simply not contain
  those components — but the composite's `current_temperature_topic` points at a
  *published* entity's state topic, and whether HA objects to a component
  reading another component's topic inside one bundle is unmeasured.
  ***Settled by:*** one throwaway bundle against HA 2026.9 at step 3b, the same
  half-day mtec spent on its duplicate-`unique_id` question.

---

## 4. The availability model

**One level, the bridge LWT, on every entity, with no `availability_mode`.**

Every one of the 264 configs carries exactly:

```json
"availability_topic": "daikin/bridge/status",
"payload_available": "online",
"payload_not_available": "offline"
```

and none of them carries `availability`, `availability_mode` or
`availability_template`. Asserted by `TestAvailabilityModelIsBridgeOnly`, which
also checks that the named topic is one the daemon actually publishes — it is,
by `Coordinator.PublishOnline` (`coordinator.go:171`) wired to the MQTT
lifecycle's `OnConnect`, with the matching `offline` as the CONNECT will
(`cmd/daikin2mqtt/main.go:113`, retained).

So, unlike mtec (whose status topic sat inside Home Assistant's own birth tree
and was referenced by no entity) and unlike homeconnect (whose daemon LWT was
inert), **this bridge's availability plane is correct and complete as it
stands.** There is no availability defect here. That is the first time in the
programme.

It is, however, the **trap** shape, not the fix shape. go-hamqtt's zero
`model.Availability` resolves to `{LevelBridge, LevelDevice}` with mode `all`,
which would add a second required source at
`daikin[/<scope>]/<uid>/availability` — a topic this bridge does not publish and
has no per-device reachability signal to publish. Under mode `all` that leaves
**every entity permanently unavailable**. `model.BridgeOnly()` is the setting.
See [F8](#f8). This is mtec's situation exactly, and the inverse of
homeconnect's.

A second, smaller mismatch: the library's default path emits the **list** form
(`availability: [{topic, payload_available, payload_not_available}]`) and never
the singular `availability_topic` with top-level payloads. `Component` has the
singular keys as an opt-in escape hatch and nothing in go-hamqtt's non-test code
writes them. So even `BridgeOnly()` does not reproduce these payloads
byte-for-byte; it reproduces them *semantically* (Home Assistant accepts both
spellings identically). The phase has to decide whether "byte-equal" means the
bytes or the meaning — an entity's registry keys are unaffected either way.

There is a real per-device availability signal available and unused: ONECTA
reports `isCloudConnectionUp` per device, and the Faikin path reports `online`.
Wiring `LevelDevice` to those would be an *improvement*, and it is exactly the
kind of thing that must not happen in the same step as the migration.

---

## 5. The pins

Two test files in `internal/coordinator`, one fixture, one golden per scenario,
and a `.gitattributes`.

### 5.1 `internal/coordinator/surface_golden_test.go` + `testdata/surface/`

Twelve scenarios × one golden each, 12 108 lines / ~430 KB of testdata:

```
testdata/surface/{airpurifier,air-to-air-dx4,altherma-air-to-water-wlan,
                  d2cnd-gas-boiler,multisplit}.{en,de}.json
testdata/surface/multisplit.local.en.json
testdata/surface/multisplit.scheduler.en.json
testdata/multisplit-two-indoor.json          # the synthesised two-indoor fixture
```

Each golden is one document:

```go
type recordedMsg struct {
	Topic  string         `json:"topic"`
	QoS    int            `json:"qos"`
	Retain bool           `json:"retain"`
	JSON   map[string]any `json:"json,omitempty"`   // decoded discovery config / attributes
	Text   *string        `json:"text,omitempty"`   // scalar state payload
	Empty  bool           `json:"empty,omitempty"`  // a retained-config clear
}
type surfaceDoc struct {
	Scenario string        `json:"scenario"`
	Messages []recordedMsg `json:"messages"`
}
```

**Topic and payload are stored together, in the same row, with the delivery
guarantee** — so a topic move, a payload change and a QoS/retain change are all
one diff. The payload is stored **decoded**, so a reviewer reads JSON rather
than an escaped blob. Comparison is on **canonical re-encoding**: both sides go
through `encoding/json`, which sorts object keys, so formatting, key order and
line endings cannot make a run pass or fail.

The recorder implements `mqtt.Client` with the real `Publish` signature. The
messages are produced by `Coordinator.pollOnce` — twice, because the first cycle
fills the embedded-id and mode caches the synthetic points hang off — plus
`PublishOnline`, plus, where the scenario calls for it,
`publishLocalState` (the Faikin read path), `PublishScheduleSwitches` and
`PublishScheduleState`. **Nothing is stubbed except the cloud transport and the
broker.** The catalogue is the shipped `characteristics.yaml`; the devices are
the shipped ONECTA fixtures.

One flag, `-update-surface-golden`, regenerates all twelve:

```sh
go test ./internal/coordinator -run TestPublishedSurfaceGolden -update-surface-golden
```

and **fails**, printing the new digests, because of the next paragraph.

### 5.2 The digests, held outside the goldens

```go
var goldenDigests = map[string]string{
	"air-to-air-dx4.en": "d883f6e9ab20085ee3aca056f917cc05b16ca0fe86a0006df2e25f9187f3b2f4",
	…
}
```

A golden produced by the code it guards is invisible to the run that produces
it. The only thing that makes a regeneration visible is a value held outside the
file. `-update-surface-golden` deliberately never writes this map: it prints the
twelve new literals and fails the test, so a human has to paste them in, in the
same commit, whose message must name the finding that moves the bytes. This is
homeconnect's mechanism, adapted: the digest is taken over the **canonical
encoding of the rebuilt scenario**, not over the file's bytes, so it is
immune to checkout differences as well as to reformatting.

The mechanism was verified accidentally and then deliberately: after the local
scenario gained the Faikin read path, the digest test failed alone, naming the
scenario, with the goldens already rewritten.

### 5.3 `internal/coordinator/surface_invariants_test.go`

Eleven tests that rebuild the surface from the builders and **never read
testdata**, so every one of them still fails immediately after a regeneration:

| Test | Pins |
| --- | --- |
| `TestStateTopicBuildersAgree` | every advertised state/attributes/availability topic is one the publish path writes to — **builder against builder** ([F3](#f3)) |
| `TestCommandTopicsAreSubscribed` | every advertised command topic matches `daikin/+/+/+/set` |
| `TestConfigTopicForm` | 264 of 264 config topics are four segments, and segment 3 is the `unique_id` ([F5](#f5)) |
| `TestNoDuplicateEntityRegistryKeys` | zero duplicates on `unique_id`, `(platform, unique_id)` and `default_entity_id` |
| `TestAvailabilityModelIsBridgeOnly` | the bridge-only model, the exact payload keys, the absence of `availability_mode`, and that the named topic is published ([F8](#f8)) |
| `TestPublishQoSAndRetain` | all 983 publishes are QoS 0 retained ([F9](#f9)) |
| `TestDeviceBlocksArePinned` | `device.identifiers` as Go literals — the device-registry keys |
| `TestIdentityIsLanguageIndependent` | `unique_id`, state/command topics and `device.identifiers` compared **en against de directly**, plus the exact two configs whose `default_entity_id` does move ([F2](#f2)) |
| `TestSlugAgreementOverTheRealCatalogue` | 0 of 55 catalog topics, 8 of 9 device-name probes ([F4](#f4)) |
| `TestTwoDefaultInstancesCollideOnEveryString` | two default instances share every config topic and every `unique_id` ([F14](#f14)) |
| `TestSurfaceCensus` | the entity, message and platform counts of §2.2, as Go literals |

The expected values that matter are Go literals, so a change to them is a
declaration in the review rather than a regenerated blob.

### 5.4 `.gitattributes`

The repository had none, ships JSON fixtures, and its CI test matrix includes
`windows-latest` (`.github/workflows/ci.yml`). Git rewrites text files to CRLF
on a Windows checkout, which changes their bytes. The comparison here is on
canonical re-encoding and the digest is over the rebuilt document rather than
the file, so neither would actually break — but the repository should not depend
on that, and homeconnect's digests did break this way. Added:

```
* text=auto eol=lf
```

### 5.5 Mutation proof

Every mutation below was applied to a **committed** tree, the test run observed,
and the tree restored with `git checkout --`. M6–M9 were run **after
regenerating the goldens**, which is the only honest test of a builder pin.

| # | Mutation | Caught by |
| ---: | --- | --- |
| M1 | `buildConfig` swaps platform and unique_id in the config topic | golden (all 12) + `TestConfigTopicForm` + `TestSurfaceCensus` + `TestIdentityIsLanguageIndependent` |
| M2 | `mainIdentifier` namespace `daikin_` → `dk_` | golden (all 12) + `TestDeviceBlocksArePinned` |
| M3 | `hass.slugify` transliterates `ü→ue` | golden (the 4 scenarios with German device names) |
| M4 | discovery publish `retain` true → false | golden (all 12) + `TestPublishQoSAndRetain` |
| M5 | state publish `mqtt.QoS0` → `mqtt.QoS1` | golden (all 12) + `TestPublishQoSAndRetain` + `TestSurfaceCensus` |
| **M6** | `Discovery.StateTopic` suffix `/state` → `/value` — **the config side only, goldens regenerated** | **`TestStateTopicBuildersAgree` (all 12)** |
| **M7** | `coordinator.go:261`'s inline builder `/state` → `/value` — **the publish side only, goldens regenerated** | **`TestStateTopicBuildersAgree` (all 12)** |
| M8 | `BridgeStatusTopic` moved to `homeassistant/status/lwt` (mtec's defect), goldens regenerated | `TestAvailabilityModelIsBridgeOnly` + `TestStateTopicBuildersAgree` |
| M9 | `Discovery.CommandTopic` suffix `/set` → `/cmd`, goldens regenerated | `TestCommandTopicsAreSubscribed` |
| M10 | regenerate one golden and nothing else | the scenario's digest, alone, by name |

M6 and M7 are the point of the exercise: each changes **one** of the bridge's
nine state-topic builders, which is invisible to a golden regeneration and is
precisely the failure mode [F3](#f3) describes. Neither is caught by the golden
after regeneration; both are caught by the builder-against-builder test.

---

## Findings

Ranked by severity. **None was fixed in this PR.** Every one is reproducible
from the pins.

<a name="f1"></a>
### F1 — two instances cannot both stay connected: the MQTT client id is a compile-time constant · **high**

`internal/config/config.go:70-71`:

```go
// MQTTClientID is the MQTT client identifier the bridge connects with.
MQTTClientID = "daikin2mqtt"
```

`grep -rn MQTTClientID --include='*.go' .` returns three hits: the constant, its
one use at `cmd/daikin2mqtt/main.go:111`, and `+"-faikin"` at `:161`. There is no
config key, no env override, and `config-template.yaml` does not mention it.

MQTT requires a broker to disconnect an existing session when a second client
presents the same identifier (3.1.1 §3.1.3.2 / 5.0 §3.1.4). Two go-daikin2mqtt
daemons on one broker — two ONECTA accounts, a staging instance, a migration
overlap — therefore kick each other in a loop, each one's `Lifecycle`
reconnecting into the other's session. Neither publishes reliably; both LWTs
fire repeatedly; nothing in either log says why.

This is independent of ADR 0070 and of discovery, and it is the only finding
here that can take a working installation down. Fix: a `MQTT_CLIENT_ID` config
key defaulting to the current constant, so no existing installation changes.

<a name="f2"></a>
### F2 — `default_entity_id` moves with LANGUAGE for every entity on a shared gateway or outdoor sub-device · **high**

`CLAUDE.md` states the invariant in bold: *"`unique_id` and `default_entity_id`
are English and language-independent."* Measured, it is true for `unique_id` and
false for `default_entity_id`.

`discovery.go:353` seeds the entity id from the **device block's** name:

```go
DefaultEntityID: p.Entry.Platform + "." + entityObjectID(dev.Name, p.Topic),
```

and `sharedSubDevice` (`discovery.go:146-157`) composes that name from a
*translated* label:

```go
label := labelEN
if d.lang == "de" && labelDE != "" {
	label = labelDE
}
…
name := "Daikin " + label          // "Daikin Outdoor unit" / "Daikin Außengerät"
```

Measured on the multi-split scenario, en against de:

| config | `en` | `de` |
| --- | --- | --- |
| `…/daikin_outdoor_ODU0000000001_outdoor_temperature/config` | `sensor.daikin_outdoor_unit_outdoor_temperature` | `sensor.daikin_aussengerat_outdoor_temperature` |
| `…/daikin_outdoor_ODU0000000001_refresh/config` | `button.daikin_outdoor_unit_refresh` | `button.daikin_aussengerat_refresh` |

Home Assistant never renames a registered entity, so an operator who switches
`LANGUAGE` gets a *second* set of entities for everything on a shared outdoor
unit or gateway, and the first set stays behind as orphans. On a real
multi-split with the twelve `scope: outdoor` catalogue entries plus the refresh
button, that is up to thirteen entities per outdoor unit.

`unique_id` and `device.identifiers` are unaffected — they come from the serial
— so the device registry survives and only the entity ids are stranded. Pinned
by `TestIdentityIsLanguageIndependent`, which asserts the divergence set is
exactly these two configs and no others.

Fix: seed the shared sub-devices' entity ids from a language-independent label
(`"outdoor_unit"`, `"gateway"`), which re-keys existing German installations
once — so it belongs in the defect step, with a release note, not in the
migration.

<a name="f3"></a>
### F3 — nine state-topic builders, and nothing compared them · **high**

The `<root>/<device>/<embedded>/<topic>/state` string is composed in nine
places:

```go
internal/hass/discovery.go:277   // Discovery.StateTopic  -> goes into the retained config
internal/coordinator/coordinator.go:261   // the per-point cloud publish
internal/coordinator/coordinator.go:324   // the synthetic hvac_mode
internal/coordinator/climate.go:305       // the synthetic fan/swing/preset
internal/coordinator/local.go:272         // the Faikin read path, per unit
internal/coordinator/local.go:324         // the Faikin read path, outdoor aggregate
internal/coordinator/local.go:342         // the optimistic write-through
internal/coordinator/schedule.go:241      // schedule_state / schedule_next_change
internal/coordinator/schedule.go:257      // the outdoor schedule pair
```

plus `Discovery.AttributesTopic` (`:272`) and `ClimateAttributesTopic`
(`climate.go:111`) for the sibling plane, and `auxBase` inline inside
`buildClimate` (`climate.go:189`) for the climate entity's five synthetic slots.

The phase plan expected two; mtec measured two and found five when it converged
them; homeconnect measured two and found six. **Nine is the largest count in the
programme**, and the reason is structural: this bridge has three independent
*writers* (the cloud poll, the Faikin read path, the scheduler) onto one topic
tree, and the synthetic climate slots that back no register are composed inline
in two of them.

They agree today — measured, across all twelve scenarios, every advertised topic
against every published one. Nothing enforced it. A change to any one alone
leaves entities pointing at topics nobody writes: permanently `unknown`, nothing
in the log, nothing in the registry to notice.

Now pinned builder-against-builder by `TestStateTopicBuildersAgree`, proven by
mutations M6 and M7. Fix (a later step): one `layout` package, the way
homeconnect's F3 fix did it — a topic composed once and delegated to.

<a name="f4"></a>
### F4 — the entity-id slug is not reproducible by `topic.Slug` · **high (gates the phase)**

§3.3. Zero of 55 catalog topics diverge; eight of nine device-name probes do,
on two independent classes (`ä→a` vs `ä→ae`, and the hyphen).

The divergence reaches `default_entity_id` only — not `unique_id`, not
`device.identifiers`, which are built from ONECTA UUIDs and component serials.
So a swap would strand the **entity ids** of every entity on a non-ASCII-named
device, while the device registry and the entity registry keys survive. That is
cheaper than homeconnect's F10 (where the device slug was in the node id and the
device identifier) but it is still a one-time re-registration for every German
room name, which in this bridge's actual user base is most of them.

This is not a defect in this bridge — `ä→a` is what Home Assistant's own
`slugify` does, and `discovery.go:410` says so — but it is the constraint the
phase has to be designed around. Either the migration supplies a
`discovery.Context` whose `ObjectID` keeps `slugify`, or it accepts the
re-registration. **The decision belongs before step 3, not inside it**; phase 6
hit its equivalent at step 3 and paid for it.

<a name="f5"></a>
### F5 — the config topic is four-segment and node-id-less; the library's default retracts nothing · **high (gates step 6)**

§2.3, measured over all 264 config topics.
`publisher.SupersededTopics(prefix, bundle)` with no `forms` argument defaults to
`[]LegacyTopicFunc{LegacyTopicWithNodeID}` — the five-segment form — which
derives `homeassistant/<platform>/<node_id>/<object_id>/config`, a topic this
bridge has never published. It would retract nothing.

The consequence is silent and total: the device bundle is published while all
264 per-entity configs are still retained, Home Assistant refuses it with one
`WARNING [mqtt.entity] Received a conflicting MQTT discovery message`, and the
result is no entities and no error on the wire. `migrate_discovery: true` was
tried on a sibling repo and did not lift it.

Fix: `Config.LegacyEntityTopics = []publisher.LegacyTopicFunc{publisher.LegacyTopicByUniqueID}`,
stated explicitly. Note that the field **replaces** the default rather than
extending it, so stating it is the whole fix — and `Runtime.LegacyForms()` plus
the `publisher.legacy_forms` boot log line exist to make the choice visible.

Two further consequences of the same fact, both for step 6:

- `Discovery.ConfigFilter()` is `homeassistant/+/+/config`, which *does* match
  `homeassistant/device/<node>/config`, so the reconcile would see a bundle —
  but `IsOwnConfig` (`discovery.go:332`) requires a top-level `unique_id`
  starting `daikin_`, which a bundle payload does not have. A bundle published
  by this daemon would be invisible to its own orphan cleanup, in either
  direction. (mtec's F11, same shape.)
- The `object_id`-less four-segment form was only made parseable by
  `publisher.ParseConfigTopic` in go-hamqtt v0.27.0; v0.32.0 is fine.

<a name="f6"></a>
### F6 — in local mode the climate fan dropdown may never be published · **medium (candidate — see §3.4)**

`coordinator/local.go:579`:

```go
if cloud, ok := faikinFanToCloud[st.Fan]; ok {
	out[hass.FanModeTopic] = localizeAux(cloud, lang, fanModeDE)
}
```

`faikinFanToCloud` (`climate.go:49`) is keyed on
`auto|low|lowMedium|medium|mediumHigh|high|night|quiet`. `faikin.State.Fan`
(`faikin.go:34`) documents the firmware as sending `auto|1..5|quiet|…`. If the
latter is right, every numbered fan speed misses the map, nothing is published,
and the climate entity's fan dropdown sits at `unknown` for the whole time the
unit is not on `auto` — while the cloud path that would have published it is
deliberately suppressed for a mapped device (`climate.go:311`, `localFanSwing`).

The sibling values in the same function are not guarded this way: `swing`,
`swing_h` and `preset` are published unconditionally. `operation_mode` is
guarded but by a map (`faikinToDaikinMode`) whose keys *are* the documented
Faikin vocabulary.

The pinned local scenario uses `Fan: "3"` precisely to expose the gap; the two
resulting dead topics are the only entries in
`knownAdvertisedButUnpublished`, named to this finding so that a fix deletes
them. Cannot be settled from this repository — see §3.4.

<a name="f7"></a>
### F7 — a `scope: outdoor` entity's `state_topic` depends on the order ONECTA returns devices in · **medium**

A `scope: outdoor` catalogue entry resolves to one point per indoor unit, all
sharing a `unique_id` derived from the outdoor serial. `Discovery.Publish`'s
`seen[uid]` gate (`discovery.go:317`) keeps the **first** one, and its
`state_topic` therefore names whichever member came first in `points` — which is
the order `process.ResolveAt` walked `devices`, which is the order the ONECTA
`GET /devices` array arrived in.

The coordinator already knows this: `publishOutdoorScheduleState`
(`schedule.go:246-249`) carries the comment *"which member's topic that entity
reads from depends on the discovery order, so every member has to carry the
value"*, and `publishOutdoorShared` does the same for the telemetry. So the
*state* is safe — every member publishes it.

What is not safe is the retained **config**. If the array order flips, the point
order flips, `discoverySignature` (`coordinator.go:757`) changes, discovery is
republished, and the retained config's `state_topic` now names a different
device. The entity keeps its `unique_id` (the outdoor serial), so it is not
re-registered — it just starts reading a different topic, with a retained value
that may be a poll old.

Measured: twelve `scope: outdoor` catalogue entries plus the refresh button, so
up to thirteen entities per outdoor unit are exposed. Whether the order actually
varies is §3.4's second unknown. Fix: choose the member deterministically
(lowest device id) rather than by arrival order — a one-line change that may
move `state_topic` on an existing installation and therefore belongs in the
defect step.

<a name="f8"></a>
### F8 — the library's availability default would make every entity permanently unavailable · **medium**

§4. This bridge's availability plane is correct: one level, the bridge LWT,
published by `PublishOnline` and by the will, named by all 264 configs.

go-hamqtt's zero `model.Availability` resolves to `{LevelBridge, LevelDevice}`
with mode `all`. `LevelDevice` names
`Layout.Availability(DeviceSlot(dev, e))` — for `topic.Default{Root: "daikin"}`
that is `daikin/<uid>/availability`, which this bridge never writes. Under mode
`all` Home Assistant requires *every* listed source to say `online`, so all 264
entities would sit unavailable forever, with nothing in any log.

`model.BridgeOnly()` is the setting. This is mtec's trap exactly, and the
inverse of homeconnect's (where the default *was* the fix). It is recorded as a
finding rather than a defect because nothing is wrong today; it is the thing
that goes wrong if the default is accepted silently at step 4.

<a name="f9"></a>
### F9 — QoS 0 becomes QoS 1 on migration unless spelled out · **medium**

All fifteen transport calls pass `mqtt.QoS0` (§2.6); `publisher.QoS`'s zero
value is `QoSUnset`, and every runtime field's documented default resolves it to
**QoS 1**. A `publisher.Config{}` left unset therefore triples the broker
traffic of a bridge that publishes 219 retained topics per poll on a 10-minute
day / 30-minute night cycle, and changes the delivery semantics of a plane whose
whole design assumes retained-last-value.

`publisher.QoSAtMostOnce` (`0x80`) is the sentinel that must be written
explicitly, in `Config.QoS`, `StateConfig.QoS`, `AvailabilityConfig.QoS` and
`CommandConfig.QoS`. `resolveQoS` panics at construction on an unrecognised
value, so a mistake is loud — but omission is silent, which is the case here.

Pinned by `TestPublishQoSAndRetain`, which reads the value off the transport
call rather than off a constant.

<a name="f10"></a>
### F10 — `hass.slugify` and `schedule.Slug` disagree, and a comment says they do not · **low**

`internal/schedule/model.go:694-698`:

> *"It deliberately matches `hass.slugify` … Keep the two in sync."*

They differ on the hyphen: `schedule.Slug` preserves `-` (`model.go:722-729`,
with its own justifying comment), `hass.slugify` folds it to `_`
(`discovery.go:424`). A schedule named `EG-Wohnzimmer` gets the id
`eg-wohnzimmer` and the entity `switch.daikin_schedule_eg-wohnzimmer`; a device
named `EG-Wohnzimmer` gets `sensor.eg_wohnzimmer_…`. Both are internally
consistent and both survive `sanitize`, which keeps `-`. Nothing is broken; the
comment is wrong, and a future "sync them" change would re-key every schedule
whose name contains a hyphen. `schedule.Slug` is frozen at creation
(`model.go:703-707`), so the risk is confined to newly created schedules.

<a name="f11"></a>
### F11 — up to 13 state topics and 16 attributes topics that no entity reads · **low**

Measured per scenario, as topics published that no config names:

| Scenario | unread `/state` | unread `/attributes` |
| --- | ---: | ---: |
| `airpurifier` | 0 | 1 |
| `air-to-air-dx4` | 2 | 3 |
| `altherma-air-to-water-wlan` | 2 | 4 |
| `d2cnd-gas-boiler` | 2 | 4 |
| `multisplit` | 5 | 8 |
| `multisplit` + scheduler | 7 | 10 |
| `multisplit` + local | 13 | 16 |

Three causes, all structural rather than accidental:

1. **The climate composite suppresses `power`, `operation_mode` and
   `temperature_setpoint`** as entities (`climate.go:179-181`) but the poll loop
   still publishes their state and attributes. Two `/state` and three
   `/attributes` per climate management point. The climate entity reads the
   setpoint topic, so only `power` and `operation_mode` are truly unread.
2. **`scope: outdoor` entities are deduplicated but their state is published per
   member** — deliberately, per F7's comment, so the surviving entity can read
   whichever it ends up naming. Every non-surviving member's copy is unread.
3. **`publishDataSources` writes an attributes document for every point,
   including the stateless refresh button** (`coordinator.go:480`) and including
   points whose entity was suppressed or deduplicated away.

Cause 1 and 3 are cheap to remove. Cause 2 is load-bearing and must not be.
mtec and homeconnect both declined to remove their equivalents on the grounds
that retiring a topic an installed base may already consume is a decision about
the operator contract, not a defect fix; the same call applies.

<a name="f12"></a>
### F12 — no `origin` block · **low**

`grep -rn '"origin"' --include='*.go' internal` → no hits. Shared with all six
bridges (ADR 0070 §2.2, *"Not one of the six sets the `origin` block"*).
go-hamqtt makes it mandatory (`origin.name` missing is a blocking validation
issue), so the migration supplies it for free at step 4 — and it is a payload
addition on all 264 configs, which means it must not land in the same step as
anything else.

<a name="f13"></a>
### F13 — `publishDataSources` runs only when the discovery signature changes · **low**

`coordinator.go:413` calls it from inside `maybePublishDiscovery`, after the
`changed` gate. The attributes are retained and their value only moves when a
device switches between the cloud and the Faikin path — which does not change
the point set, so it does not change the signature, so the attributes are not
republished. The `data_source` attribute can therefore be stale for as long as
the point set is stable, which is normally forever.

Latent rather than live in practice: `localActiveFor` is a function of the
static `LOCAL_DEVICE_MAP`, so a device's source does not actually change at
runtime today. It would the moment a fallback-on-Faikin-timeout behaviour is
added.

<a name="f14"></a>
### F14 — two default instances publish byte-identical configs to byte-identical topics · **medium**

Every identity string is derived from the ONECTA device id or a component
serial plus the catalog topic. Neither `MQTT_TOPIC` nor `HASS_BASE_TOPIC` nor
any instance identifier enters `unique_id`, `device.identifiers` or the config
topic — verified by `TestTwoDefaultInstancesCollideOnEveryString`, which shows
all 30 config topics and all 30 `unique_id`s of the multi-split scenario shared
between two differently-configured instances.

Today the consequence is mild: two daemons seeing the same ONECTA account write
the same bytes to the same topics, last writer wins, and `IsOwnConfig` accepts
both so neither sweeps the other away. (They cannot actually be connected at the
same time — see [F1](#f1) — so in practice they alternate.)

**At step 6 the stakes change.** A device bundle is *one* retained topic per
device carrying that device's *entire* component set. Two instances with any
divergence — different `LANGUAGE`, different catalogue version, one in local
mode — then replace each other's whole entity set on every publish, rather than
overwriting entity-by-entity. This is homeconnect's F8 with a sharper edge,
because daikin's per-entity surface is more configuration-dependent.

Nothing is decided here. The three candidates are the same as homeconnect's: put
`MQTT_TOPIC` in the namespace (breaks every existing `unique_id`), add an
`INSTANCE_ID` key empty by default (additive, and empty is the current
behaviour), or leave it and document it.

---

## Sequencing — the rest of phase 8

Ordered so each step de-risks the next, following the shape phases 5, 6 and 7
converged on.

| Step | Work | Why here |
| ---: | --- | --- |
| **0** | **This PR.** Bump `go-mqtt` v1.3.0 → v1.5.1; add the twelve-scenario surface pin, the digests, the eleven builder invariants and `.gitattributes`; measure and record F1–F14. | Nothing can be proved byte-equal against a surface that was never captured. |
| 1 | Housekeeping the bump enables, payloads untouched — the goldens must not move. | Small, mechanical, and a free check that the pins do not fire on a non-payload change. |
| 2 | **Fix the defects, one commit per finding, goldens regenerated with the diff reviewed.** F1 (`MQTT_CLIENT_ID`) and F3 (one topic builder) first — neither changes a byte on the wire. Then F2 (the localized entity-id seed — a one-time re-key, with a release note), F7 (deterministic outdoor member), F13, F10's comment. F6 only once §3.4's first unknown is settled. | The byte-equality proof in step 5 must compare against *corrected* bytes, not against bugs. F1 is first because it is the only finding that takes a live installation down. |
| 3 | **Decide the three questions that are not implementation.** (a) F4: a consumer `discovery.Context` keeping `slugify`, or accept entity-id re-registration for non-ASCII device names. (b) F8: `model.BridgeOnly()` and stay byte-equal, or take `{LevelBridge, LevelDevice}` and wire `LevelDevice` to `isCloudConnectionUp`/Faikin `online` — changing all 264 payloads. (c) F14: whether an `INSTANCE_ID` lands before the bundle. Written down, not discovered. | All three are irreversible for an installed base. Phase 6 hit (a) at step 3 and paid for it. |
| 3b | **Settle §3.4's third unknown against a live Home Assistant.** One throwaway bundle carrying a `climate` component whose `current_temperature_topic` is another component's state topic, HA 2026.9, watch the log. | Half a day, and it gates step 6 for the one bridge whose composite entity is the point of the phase. |
| 4 | **Model the catalogue as `model.Entity` and render one bundle, publishing nothing.** The composite climate as `Bindings` + `Suppressor` + `Builder`; the shared outdoor unit as `Identity.Equal` merging two indoor units' outdoor identifiers; the cloud/Faikin fusion as `Origin` + `Precedence("local", "cloud")`. Compare the rendered per-component output **against the golden files, not against the builder it replaces**. Neither pin regenerated. | This is where a `Layout`, `Context`, `Slot` or composite mismatch surfaces, at zero risk — and it is the part ADR 0070 §3.5 nominates daikin to prove. |
| 5 | Adopt the library on the **state and command** planes, discovery still per-entity from the old path. Spell `publisher.QoSAtMostOnce` everywhere (F9). | The state plane has no registry keys to orphan; it is the cheap half. |
| 6 | **Switch discovery to the device bundle.** `PublishBundle` + `SupersededTopics(prefix, bundle, publisher.LegacyTopicByUniqueID)` (F5), retracting all per-entity configs before the bundle lands, and teach `IsOwnConfig` to recognise a bundle in both directions. Verify against a live HA that no `Received a conflicting MQTT discovery message` warning appears. | The one step no unit test can prove. Everything above exists to make it a small diff. |
| 7 | Apply the step-3 decisions, add the `origin` block (F12), changelog + `addon/CHANGELOG.md`, version bump across the five spots `CLAUDE.md` names. | Operator-visible last. |

### What I would not do

- **Do not swap `slugify` for `topic.Slug` as a convenience.** Zero catalog
  topics change and every German room name does; the win is cosmetic and the
  cost is every entity id on the device.
- **Do not take the library's availability default without reading F8.** It
  costs one line to get right and greys out 264 entities to get wrong, with
  nothing in any log.
- **Do not fix F2 and F7 in the migration step.** Both move retained strings on
  an installed base. A migration step whose golden diff is empty is provable;
  one whose diff is a hundred rows is not.
- **Do not remove the unread topics of F11 cause 2.** They are what makes the
  `scope: outdoor` dedup work at all.
- **Do not shrink the pins.** The 38 sensors of a local-mode multi-split that
  nobody looks at are exactly where an identity regression hides.
- **Do not regenerate a golden to make a step pass.** Every regeneration in
  steps 2 and 7 must be accompanied by a reviewed diff and a hand-updated
  digest naming the finding; steps 1, 4, 5 and 6 must regenerate nothing.

---

## Appendix — how the measurement was taken

Everything in this document was produced by the two pin test files it describes,
against this repository's own working tree. There was no throwaway scratch copy
and no transcription of production code into a test, with two exceptions, both
in `surface_invariants_test.go` and both unreachable from any production path:

- `librarySlug` — a verbatim transcription of go-hamqtt v0.32.0's
  `topic.Slug` (`topic/topic.go`), kept there so §3.3 can be counted without
  taking the dependency one step early. It deletes itself at step 4.
- `bridgeSlug` — a transcription of `internal/hass.slugify`, which is
  unexported, kept beside `librarySlug` so the two are read together.

The one synthesised input is `internal/coordinator/testdata/multisplit-two-indoor.json`:
two copies of the shipped `air-to-air-dx4` ONECTA fixture with distinct device
ids, distinct room names (`Wohnzimmer`, `Küche`), distinct gateway and indoor
serials and **one shared outdoor serial**. No shipped fixture has more than one
device or any component serial, so without it the `scope: outdoor` dedup, the
shared sub-device blocks, the multi-split identity plane and F2 are all
unreachable. Its values are otherwise the real fixture's.

Commands that reproduce the numbers:

```sh
# the whole pinned surface, and every count in §2.2
go test ./internal/coordinator -run TestSurfaceCensus -v

# the topic form (§2.3), the identity plane (§3.1-3.2), the slug (§3.3),
# the availability model (§4), the delivery guarantee (§2.6)
go test ./internal/coordinator -run 'TestConfigTopicForm|TestNoDuplicate|TestSlugAgreement|TestAvailabilityModelIsBridgeOnly|TestPublishQoSAndRetain|TestIdentityIsLanguageIndependent' -v

# the nine state-topic builders against each other (F3)
go test ./internal/coordinator -run TestStateTopicBuildersAgree -v

# regenerate the goldens; this FAILS and prints the new digests to paste in
go test ./internal/coordinator -run TestPublishedSurfaceGolden -update-surface-golden
```

The LOC figures use `git ls-files` and brace-matched function extents, not
`awk`-to-next-`func` spans.
