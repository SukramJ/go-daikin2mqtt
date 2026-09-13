# ADR 0070 phase 8 — measurement for go-daikin2mqtt

- Status: measurement (step 0), plus the step 1+2 outcome and the F4, F8, F9,
  F11, F12 and F14 decisions — see
  [Step 2 outcome](#step-2-outcome--what-was-fixed-what-was-decided-what-stays)
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

Ranked by severity. **None was fixed in the measurement PR (#75).** Their
status after steps 1+2 is in
[Step 2 outcome](#step-2-outcome--what-was-fixed-what-was-decided-what-stays),
which also corrects the places where this document measured wrong.

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

---

## Step 2 outcome — what was fixed, what was decided, what stays

This section was written by phase 8 steps 1+2 — the PR that fixes the defects.
The measurement above is unchanged except where it was measured *wrong*; those
corrections are collected in
[Corrections to the measurement above](#corrections-to-the-measurement-above)
rather than silently edited into the text.

### Decisions taken in writing

The sequencing table below puts F4, F8 and F14 at step 3, *"decided, not
discovered"*. Three of them are decided **here**, one step early, because step 2
had to touch the code they govern, and **a fix that contradicts a later decision
is worse than an early decision**. Written down once, so step 4 does not reopen
them.

<a name="d-f4"></a>
**F4 — the slug. Keep this bridge's own normalisers. Decided; do not reopen.**

The question is not which slug is more correct. `topic.Slug` **is** the more
correct one by one reading — it expands `ä ö ü` to `ae oe ue`. But
`hass.slugify` expands them to `a o u`, which is what **Home Assistant's own
`slugify` does**, and `discovery.go` says so at the point of definition. Being
consistent with Home Assistant beats being consistent with a sibling Go module.

Measured, the cost of swapping is entirely on one side:

- **0 of 55 catalog topics diverge.** Every `topic:` in `characteristics.yaml`
  is lower-case ASCII snake_case, so both functions are the identity on them.
  The entity half of every id is safe either way.
- **8 of 9 device-name probes diverge**, on two independent classes: the umlaut
  expansion and the hyphen.

So the swap buys nothing but consistency with a sibling module's
transliteration, and costs a one-time entity-id re-registration on every
German-named device — which in this bridge's actual user base is most of them.
This is the third bridge in the programme to face it and the third to keep its
own: `go-mtec2mqtt` (8 of 100) and `go-homeconnect2mqtt` (5 of 7 device probes)
both refused the swap.

**There are THREE normalisers to reproduce at step 4, not one:**

| Function | File | Rule |
| --- | --- | --- |
| `hass.slugify` | `internal/hass/discovery.go` | lower-case; `ä→a ö→o ü→u ß→ss`; any run of other characters → one `_`; trimmed |
| `hass.sanitize` | `internal/hass/discovery.go` | **preserves case**; keeps `[A-Za-z0-9_-]`; everything else → `_`. This is what keeps the ONECTA UUID's hyphens in `unique_id` |
| `schedule.Slug` | `internal/schedule/model.go` | as `slugify`, but **preserves `-`**, and is frozen at a schedule's creation |

A `discovery.Context` for this bridge has to supply all three. The second is the
one most easily missed, because it is the only one that is not a slug.

**Do not "fix" the transliteration either.** `Küche → kuche` is arguably wrong
and stays: an umlaut in a room name is not a reason to break someone's entity
registry.

<a name="d-f8"></a>
**F8 — availability. `model.BridgeOnly()`. Decided; do not reopen.**

This bridge's availability plane is **correct and complete as it stands** — the
first in the programme with no availability defect. One level, the bridge LWT at
`daikin/bridge/status`, published by `Coordinator.PublishOnline` on connect and
by the CONNECT Will on loss, named as `availability_topic` by all 264 discovery
payloads, with `payload_available`/`payload_not_available` beside it and no
`availability_mode` anywhere. **Nothing to fix.**

It is recorded as a decision because it is the **trap** shape, not the fix
shape. go-hamqtt's zero `model.Availability` resolves to
`{LevelBridge, LevelDevice}` with mode `all`. `LevelDevice` names
`daikin[/<scope>]/<uid>/availability` — a topic this bridge never writes and has
no per-device reachability signal to write. Under mode `all` Home Assistant
requires *every* listed source to say `online`, so accepting the default would
leave **all 264 entities permanently unavailable**, with nothing in any log.
`go-mtec2mqtt` hit exactly this.

`model.BridgeOnly()` is the setting. Step 4 states it explicitly.

Two things step 4 must also know:

- The library's default path emits the **list** form
  (`availability: [{topic, payload_available, payload_not_available}]`) and
  never the singular `availability_topic` with top-level payloads. So even
  `BridgeOnly()` does not reproduce these payloads byte for byte; it reproduces
  them *semantically* (Home Assistant accepts both spellings identically). An
  entity's registry keys are unaffected either way. The phase has to say which
  it means by "byte-equal" — and it should say *semantically*, for this one key
  group only, named.
- There **is** a real per-device availability signal available and unused:
  ONECTA reports `isCloudConnectionUp` per device, and the Faikin path reports
  `online`. Wiring `LevelDevice` to those would be an improvement. It must not
  happen in the same step as the migration.

<a name="d-f9"></a>
**F9 — QoS. What this bridge passes, read off the transport call.**

Pinned, not changed. `TestPublishQoSAndRetain` asserts over every recorded
publish of every scenario that the value reaching `mqtt.Client.Publish` is
`mqtt.QoS0` and `retain` is `true` — **read off the transport call, not off a
constant**, which is what made `go-mtec2mqtt`'s equivalent pin survive its whole
plane moving. The two `retain=false` sites are the outbound Faikin *command*
topics, correctly so, and they are outside the recorded surface.

At step 5 every one of those sites must be spelled `publisher.QoSAtMostOnce`
(`0x80`) in `Config.QoS`, `StateConfig.QoS`, `AvailabilityConfig.QoS` and
`CommandConfig.QoS`. `publisher.QoS`'s zero value is `QoSUnset` and resolves to
**QoS 1**. `resolveQoS` panics at construction on an unrecognised value, so a
mistake is loud — but *omission* is silent, and omission is the failure mode.

<a name="d-f11"></a>
**F11 — the unread topics. Leave them. Decided.**

Up to 13 `/state` and 16 `/attributes` topics that no published entity reads.
Not removed, and not from oversight:

- Cause 2 (the `scope: outdoor` members' copies) is **load-bearing** and must
  not be touched — it is what makes the dedup work at all, and F7's fix does not
  change that.
- Causes 1 and 3 are cheap to remove and are still not removed, because
  retiring a topic an installed base may already consume is a decision about the
  **operator contract**, not a defect fix. `go-mtec2mqtt` declined its
  equivalent twice, and `go-homeconnect2mqtt` once, on exactly this ground.

The count moved from F11's table: F6's fix publishes two previously-dead
`fan_mode` topics, so the local scenario's *advertised-but-unpublished* set is
now empty (it was the only entry) while the *published-but-unread* counts stand.

<a name="d-f12"></a>
**F12 — the `origin` block. Not added here. Deliberate.**

go-hamqtt makes `origin.name` a blocking validation issue, so the migration
supplies it for free at step 4. Adding it now would be a payload addition on all
264 configs, in the same PR as six other findings — and the measurement's own
rule is that it "must not land in the same step as anything else". It is one
line at step 4 and an unreviewable 264-row diff here.

### Fixed in step 2

| Finding | Fix | Bytes moved |
| --- | --- | --- |
| **F1** high | `MQTT_CLIENT_ID` config key / `DAIKIN_MQTT_CLIENT_ID` / add-on `mqtt_client_id`, defaulting to the literal `daikin2mqtt`. The Faikin connection derives from it too. | none |
| **F3** high | `internal/layout` — one `Root`/`Slot` vocabulary replacing **twelve** state-topic builders, two attributes builders, four command builders and three bridge-status builders | none |
| **F5** high | `Discovery.ConfigTopic` (one builder, was three) + `hass.LegacyConfigTopicForm`, the named constant step 6 must state | none |
| **F10** low | the comment claiming `schedule.Slug` matches `hass.slugify` corrected, and the divergence pinned in both directions | none |
| **F13** low | `publishDataSources` moved out from behind the discovery-signature gate | none |
| **F2** high | the entity-id seed composed from the **English** label, separately from the localized display name | **2 keys**, `multisplit.de` only: `default_entity_id` on the two shared-outdoor configs |
| **F7** medium | the surviving member of a deduplicated shared sub-device chosen on `(DeviceID, EmbeddedID)` rather than by ONECTA array order | **39 keys** across the four multi-split scenarios: `state_topic`, `json_attributes_topic`, `command_topic` |
| **F6** medium | `faikinFanToCloud` keyed on the real `fanSpeed` vocabulary instead of the **humidification** one | **2 topics added**, `multisplit.local.en`: `…/fan_mode/state` |

Total: **41 keys changed and 2 topics added, across 5 of the 12 scenarios.** No
`unique_id`, no `device.identifiers`, no config topic, no `qos` and no `retain`
moves anywhere. Five digests updated by hand.

The diff was derived by running the **builders** at `origin/main` and at the
branch and comparing outputs key by key — not by reading the regenerated
fixtures, which are produced by the very code they guard.

### Deliberately not fixed

**F11 and F12** — see the decisions above.

**F14 — two instances. Measured, written down, not fixed.** And **F1's fix
changes its character**: see the next section.

**F4's slug** — decided, not changed.

### F14 — what step 6 needs to know

Every identity string this bridge publishes derives from the ONECTA device id or
a component serial, plus the catalogue topic. Neither `MQTT_TOPIC` nor
`HASS_BASE_TOPIC` nor any instance identifier enters `unique_id`,
`device.identifiers` or the config topic.
`TestTwoDefaultInstancesCollideOnEveryString` shows all 30 config topics and all
30 `unique_id`s of the multi-split scenario shared between two
differently-configured instances.

**F1's fix removed the thing that was masking it.** Before this PR two daemons
could not both stay connected: they presented the same client id and the broker
disconnected each in turn, so in practice they alternated rather than
overlapped. They can now both hold a session. What two instances seeing the same
ONECTA account then do to each other, *today*:

- They write byte-identical payloads to byte-identical topics, so as long as
  they are configured identically the result is indistinguishable from one
  instance. Last writer wins; both are the same bytes.
- They **diverge** the moment any of `LANGUAGE`, `MQTT_TOPIC`, `LOCAL_MODE`,
  `LOCAL_DEVICE_MAP` or the catalogue version differs. Then each poll rewrites
  the other's configs entity by entity, and Home Assistant sees the entity's
  definition flip back and forth on every poll — `state_topic` pointing at one
  root and then the other, display names switching language.
- Neither sweeps the other away: `IsOwnConfig` accepts both, because both are in
  the `daikin_` namespace, so the orphan reconcile leaves them alone. They do
  not fight. They overwrite.

  > **This claim was wrong, and step 5 proved it wrong.** See
  > [F18](#f18). "Accepts both, so neither sweeps the other" holds only where
  > both are accepted *as current entities*, which is true only for identical
  > device sets. Two instances seeing different devices — two ONECTA accounts,
  > the home-plus-holiday-home case this document itself contemplates — each
  > find the other's configs accepted by `IsOwnConfig` and **absent from their
  > own published set**, which is the definition of an orphan. On a shared MQTT
  > root that was the sibling's entire fleet; across roots it was still its
  > composite climate entities and its refresh buttons, because those carry no
  > `state_topic` for the root check to key on. The reconcile is live and
  > unguarded today. They did fight.
- Their state planes do NOT collide if `MQTT_TOPIC` differs — but the discovery
  configs still do, so half the entities end up naming the other instance's
  root.

**At step 6 the stakes change, and this is the part that must not land
unexamined.** A device bundle is **one** retained topic per device carrying that
device's **entire** component set. Overwriting stops being per-entity and
becomes whole-entity-set replacement:

- Two instances with any divergence replace each other's complete component set
  on every publish, rather than overwriting entity by entity. It goes from "some
  entities point at the wrong root" to "the whole device flips".
- Worse, and this is the failure `go-mtec2mqtt`'s reviewer proved for the
  equivalent shape: a **staggered upgrade**. Instance A is upgraded to publish
  bundles and retracts the per-entity configs via `SupersededTopics`. Instance B
  is still on the old build and republishes its per-entity configs on its next
  poll — into a namespace A has just declared superseded. A's next reconcile
  then retracts them again. Between the two, Home Assistant sees the fleet
  appear and disappear. If instead B is upgraded second and publishes a bundle
  keyed on the same device identifiers, **B's bundle replaces A's entire
  fleet**, not one entity of it.
- The retraction list is keyed on `unique_id` (see
  [F5](#f5)/`hass.LegacyConfigTopicForm`), and both instances produce the same
  `unique_id`s. So neither instance can tell its own retained configs from the
  other's, in either direction.

The three candidates are unchanged and **nothing is decided here**: put
`MQTT_TOPIC` in the namespace (correct, and re-keys every `unique_id` in every
existing installation), add an `INSTANCE_ID` key empty by default (additive, and
empty is exactly the current behaviour), or leave it and document it. What step
3 must decide is not *which*, but *whether a decision lands before the bundle*.
The answer that needs no argument: **step 6 must not ship without one.**

### Corrections to the measurement above

The measurement got two counts wrong, both found by acting on it rather than
reading it.

1. **[F3](#f3) says nine state-topic builders. There are twelve.** The three it
   missed:
   - `internal/hass/climate.go:190` `auxBase` — the composite climate's five
     synthetic slots. (§F3 mentions it in a trailing sentence but excludes it
     from the nine.)
   - `internal/hass/schedule.go:45` `Discovery.ScheduleStateTopic`.
   - `internal/coordinator/schedule.go:288` `PublishScheduleSwitches`.

   The last two are a **pair** — the same topic composed in two packages, from
   two separately-declared `SchedulerDeviceID` constants, with the leaf segment
   a bare `"enabled"` literal on one side and a named constant on the other.
   That is precisely the shape F3 describes, and it was not in the count.

   `TestStateTopicBuildersAgree`'s own doc comment in #75 said "eight places …
   and seven inline" while listing nine sites, so the pin and the document
   disagreed with each other as well. Both are corrected.

   Counting the whole tree rather than just the state plane: **seventeen**
   composition sites — 12 state, 2 attributes, 4 command, 3 bridge status (the
   bridge status topic was composed in `cmd/daikin2mqtt/main.go` for the Will,
   in `coordinator.go:172` for the retained `online`, and in
   `hass/discovery.go:268` for all 264 payloads). The programme's running count
   is therefore: zendure —, mtec 2 measured / 5 found, homeconnect 2 measured /
   6 found, daikin 9 measured / **12** found. **The count has still never been
   too high.**

2. **[F6](#f6) is a confirmed defect, not a candidate.** §3.4's first unknown —
   "whether real Faikin firmware sends `fan: auto|1..5|quiet` or
   `auto|low|medium|…`" — is answered *from this repository*, without a live
   module:
   - `docs/api/onecta-cloud-api-openapi.json` gives
     `fanSpeed.currentMode.values` as `["quiet","auto","fixed"]`, `fixed` an
     integer 1..5 — and `low`/`medium`/`high` as the **humidification**
     vocabulary, which is what `faikinFanToCloud` was written against.
   - Every `fanSpeed` block in every shipped ONECTA fixture agrees.
   - `parseFanSpeed` builds the entity's advertised `fan_modes` from exactly
     those values.
   - `cloudFanToFaikin`, four lines above the broken map, maps cloud
     `auto|quiet|1..5` to Faikin `A|Q|1..5`.

   So the map's keys belonged to a different characteristic entirely. Fixed.
   What remains genuinely unknown, and is narrower than §3.4 stated: whether the
   firmware ever reports its fan as the command *character* (`A`, `Q`) rather
   than the word. Those are deliberately not accepted.

3. **§2.2's message count for `multisplit.local.en` was 219; it is 221 after
   F6's fix.** The two added messages are the `fan_mode` state topics that
   finally get published.

§3.4's second unknown — whether ONECTA's device array order is stable — is
**no longer load-bearing**. [F7](#f7)'s fix removes the dependency rather than
resolving the question. §3.4's third (whether HA accepts a bundled `climate`
component reading a sibling component's state topic) is untouched and still
gates step 6 at step 3b.

---

## Step 4 outcome — go-hamqtt reproduces the published surface, byte for byte

This section was written by phase 8 step 4, **the decisive experiment**: model
the catalogue as `model.Device` + `model.Entity`, render it through
`go-hamqtt` v0.32.0, and compare the result against the twelve pinned
scenarios — **publishing nothing**.

### The result

| Pin | Pinned configs | Reproduced |
| --- | ---: | ---: |
| the twelve scenario goldens, discovery plane | **264** | **264** |

**264 of 264, across all twelve scenarios, on canonical re-encoding** — which
is the standard `TestPublishedSurfaceGolden` itself holds, because the bridge
marshals a struct in field order and the library marshals a map in sorted-key
order, and key *order* is not a fact Home Assistant can see. Every key, every
value and every topic is compared exactly.

**No golden was regenerated and no digest moved.** `-update-surface-golden` is
unreachable from the new test file, the twelve SHA-256 literals in
`goldenDigests` are byte-identical to what #76 left, and
`git diff origin/main -- internal/coordinator/testdata` is empty. The
comparison runs against the golden **files**, never against the builders they
guard.

**Nothing reached a broker.** `TestHamqttRenderPathPublishesNothing` drives all
four entry points of the new path with an `mqtt.Client` that fails the test on
contact. No publish path, coordinator or MQTT-bootstrap code was changed.

### The one key group reproduced semantically rather than literally

Exactly as decision [F8](#d-f8) requires, named once and nowhere else: the
library's default path renders availability as a **list**
(`availability: [{topic, …}]` plus `availability_mode`), and this bridge
publishes the singular `availability_topic` with top-level payload keys. Home
Assistant accepts both spellings identically and an entity's registry keys are
unaffected either way.

`hass.singularAvailability` performs that one re-spelling in the entity's
`discovery.Builder`, and it **refuses anything that is not exactly one plain
bridge-level source**. That is what keeps `model.BridgeOnly()` load-bearing
rather than decorative: the library's zero `model.Availability` resolves to
`{LevelBridge, LevelDevice}`, which would produce two entries and **fail the
render** instead of silently publishing a second, never-written topic that
would leave all 264 entities permanently unavailable. Mutation M13 proves it.

### What it took

`internal/hass/hamqtt.go`, ~430 lines including its argument, plus two small
**pure extractions** in the files it has to share logic with:

- **`hamqttLayout`** — a `topic.Layout` delegating to `internal/layout`, so the
  library path and the daemon's own publish path cannot disagree about a topic.
  F3 carried forward and asserted **builder against builder**
  (`TestHamqttLayoutMatchesInternalLayout`), including the schedule pair — the
  one topic composed in two packages from two constants that F3's original
  count missed. `topic.Default` is unusable on all four methods, which
  `TestHamqttLayoutIsNotTheLibraryDefault` records so a later reader does not
  try.
- **`hamqttContext`** — `discovery.StdContext` with `UniqueID`, `ObjectID` and
  `NodeID` overridden onto this bridge's own normalisers. All three of F4's
  normalisers are reproduced: `slugify` (in `entityObjectID`), `sanitize` (in
  `UniqueID`/`NodeID` — the one that is not a slug and is most easily missed,
  and the reason the ONECTA UUID keeps its hyphens) and `schedule.Slug`
  (frozen into the schedule id the switch entity is keyed on). Nothing reaches
  for `topic.Slug`.
- **`climateEntity`** — the composite as `Bindings` + `Suppressor` + `Builder`,
  see below.
- Two extractions, each proved inert by the twelve unmoved digests:
  `hass.entityIDBase` (`entityIdentity` without its final
  `sanitize(base + "_" + topic)`, so the context can compose the same string
  rather than be handed a finished one) and `hass.climateEligibleGroups` /
  `climateConsumedKeys` (so both paths decide which management points become a
  composite, and what it replaces, from one place rather than two).

**Three library settings are load-bearing and each would have been a silent
264-row diff:** `discovery.RawEncoding` (the zero `Encoding` attaches a
`value_template` reading `value_json.value` to every entity with a state
topic), the zero `discovery.Origin` on `RenderComponent` (which attaches an
origin block whenever its name is non-empty — [F12](#d-f12), deliberately not
added inside the byte-equality proof) and `model.BridgeOnly()`.

### The topic form — settled with evidence, and the verdict holds

**`publisher.LegacyTopicByUniqueID`.** Step 6 must state it explicitly in
`publisher.Config.LegacyEntityTopics`; `hass.LegacyConfigTopicForm` already
records it and `TestHamqttLegacyTopicForm` now asserts the constant against
what actually reproduces the topics.

Proved against the **pinned** topics rather than read off `ConfigTopic`: every
component of every scenario is turned into a `publisher.LegacyEntity` and run
through **all three** forms the library ships.

| Form | Reproduces a pinned config topic |
| --- | ---: |
| `LegacyTopicByUniqueID` | **264 of 264** |
| `LegacyTopicWithNodeID` (the library **default**) | **0 of 264** |
| `LegacyTopicByObjectID` | **0 of 264** |

Three candidates rather than two, and the two losers reproduce *nothing*, so
the test cannot pass vacuously — the forms are unambiguously distinguishable
here because the object id is the catalogue topic while the unique id carries
the `daikin_<deviceID>_` prefix. Swapping the verdict fails the test (M27).

### `discovery.Validate` — clean, and that is a finding too

Three bridges in this programme had never validated their own output against
Home Assistant's schemas; two of the three that have now been checked were
refused (go-homeconnect2mqtt 11 of 687, go-unifi2mqtt 18 of 315, every
`button`).

**go-daikin2mqtt passes clean, in both forms:**

- `discovery.ValidateBody` over all **264** rendered per-entity payloads: **0
  blocking, 0 advisory.**
- `discovery.Validate` over **31 device bundles carrying 264 components** — the
  form step 6 publishes, validated as one document each: **0 blocking, 0
  advisory.**

This matters beyond hygiene precisely because of what step 6 does with the
answer: a bundle reported `Blocking()` publishes **nothing at all**, so one
refused component costs a device its entire entity set. There is no such
finding here, and therefore **nothing in this phase gates step 6 on a payload
fix** — which is the first time in the programme.

The `button` platform, which refused 18 of 315 in go-unifi2mqtt, is present
here (10 of the 264) and validates. There is no `device_tracker` in this
bridge. The scheduler's switches validate as ordinary switches.

### The composite `climate` — the thing phase 8 nominated this bridge to prove

All **14** composite climate configs across the twelve scenarios are reproduced
byte for byte, and the modelling needed **no runtime special case**, which is
the claim ADR 0070 §3.5 makes about this class:

- **Seven role bindings**, of which **five are synthetic** (`hvac_mode`,
  `fan_mode`, `swing_mode`, `swing_h_mode`, `preset_mode`) — backed by no
  catalogue entry and no register. They are ordinary `model.Slot`s on the same
  device and management point with a synthetic leaf, resolved by the same
  `Layout` as everything else. Nothing in the model knows they are synthetic.
- **`model.Suppressor`** replaces the bridge's `consumed` set: all the
  entities are handed to the model, including the three the composite replaces,
  and `model.ApplySuppression` removes them. That is a stronger statement than
  filtering them out first, and M22/M28/M29 prove it is doing the work.
- **`discovery.Builder`** + `discovery.ClimateFields` spell the per-role keys.
  The library's own guarantee carries the rest: it declines to project
  `state_topic` onto `climate` (and onto `button`, exercised deliberately with
  wrong input) because the platform's schema declares none.

**The open question §3.4 left gating step 6 is narrowed but not closed.**
`TestHamqttBundledClimateReadsASiblingComponentsTopic` shows the shape really
does occur and is not hypothetical: in a rendered bundle the composite's
`current_temperature_topic` is byte-equal to the `state_topic` of the
`room_temperature` **component of the same bundle** (`room_temperature` is not
among the three the composite suppresses), on both indoor units of the
multi-split. The library renders it without complaint and Home Assistant's own
discovery *schemas* do not refuse it. What is left for step 3b is therefore the
**runtime** behaviour alone, against a bundle this test proves is buildable —
a narrower experiment than the measurement budgeted for.

### Mutation proof — 30 applied, 30 caught

Every new assertion was verified to fail under mutation, on a **committed**
tree, one at a time, reverted with `git checkout --` between runs.

| # | Mutation | Caught by |
| ---: | --- | --- |
| M1 | `hamqttLayout.State` renders the command topic | goldens + the builder-against-builder pin |
| M2 | the layout swaps a slot's Address and Channel | goldens + layout pin |
| M3 | `hamqttLayout.Attributes` renders the state topic | goldens + layout pin |
| M4 | `hamqttLayout.Bridge` moved into HA's own tree (mtec's defect) | goldens + layout pin + availability pin |
| **M5** | **`hamqttLayout.Availability` starts naming a topic** | **`TestHamqttLayoutIgnoresOnlyWhatItDeclares` alone** |
| **M6** | **the layout starts reading `Slot.Bucket`** | **the same test, plus the goldens** |
| M7 | `UniqueID` falls back to `discovery.UniqueID` | goldens (15 of 264) + normaliser pin |
| M8 | `ObjectID` falls back to `discovery.ObjectID` | goldens (the German device names) + normaliser pin |
| **M9** | **`NodeID` falls back to `discovery.NodeID`** | **the normaliser pin alone** |
| M10 | `ObjectID` ignores the climate's `thermostat` seed key | goldens + climate pin |
| **M11** | **`ObjectID` ignores the schedule's unique-id seeding** | goldens + `TestHamqttScheduleEntityIDIsTheUniqueID` |
| M12 | `Encoding` left at the zero `EnvelopeEncoding` | goldens + encoding pin |
| M13 | `model.BridgeOnly()` dropped — the library's default | every render fails; availability pin names the setting |
| M14 | `availability_mode` survives the re-spelling | goldens + availability pin |
| **M15** | **`singularAvailability` accepts more than one source** | **availability pin alone** |
| M16 | `binary_sensor` `payload_on` `"true"` → `"1"` | goldens |
| M17 | `switch` loses `state_on` | goldens |
| M18 | `button` `payload_press` `"PRESS"` → `"press"` | goldens + button pin |
| M19 | `select` loses its options | goldens + **both validators** |
| M20 | `number` loses `min` | goldens |
| M21 | `button` loses its command binding | goldens + both validators + button pin |
| M22 | the composite climate suppresses nothing | goldens + climate pin + validators |
| M23 | `current_temperature_topic` reads the command topic | goldens + climate pin |
| M24 | the climate mode list loses `"off"` | goldens |
| M25 | the climate entity-id seed key `thermostat` → `climate` | goldens + climate pin |
| M26 | an `origin` block is attached | goldens + `TestHamqttOriginIsAbsent` |
| **M27** | **the legacy-topic-form verdict is swapped** | **`TestHamqttLegacyTopicForm`** |
| M28 | `model.ApplySuppression` is never called | goldens + validators |
| M29 | `climateConsumedKeys` drops the setpoint | goldens + the daemon's own suppression test |
| M30 | `entityIDBase` loses the shared-outdoor namespace | the step-0 invariants + the input-fidelity pin |

M5, M6, M9, M11 and M15 are the point of the exercise: each changes **no
published byte**, so no golden can ever catch one. They are caught by
assertions written for exactly that reason.

### Two things asserted rather than claimed as coverage

Holding #76's standard turned up two places where a mutation is **equivalent**
rather than missed. Both are asserted directly, per that standard:

1. **`sanitize` and `topic.Slug` agree on a main device's identity.** An ONECTA
   device id is a lower-case hyphenated UUID and `topic.Slug` preserves both
   the hyphen and the case, so **249 of the 264** unique ids would survive a
   swap to the library's own `UniqueID` unchanged. The proof rests on the
   other **15** — the shared gateway and outdoor sub-devices, whose serials are
   **upper case** (`ODU0000000001`, `GW0000000001`, `ABCDEF0123456789`) and
   which `topic.Slug` would lower-case. `TestHamqttContextUsesThisBridgesNormalisers`
   asserts the divergent case *and* the equivalent one, so the test
   fails if the divergence class ever grows or shrinks.
2. **A `button` with a readable state binding.** It changes no byte, because
   go-hamqtt projects `state_topic` only onto the platforms whose schema
   declares it and `button` is one of the ten that do not. That is the
   library's guarantee, and a test rendering only correct input never exercises
   it — so one test renders the wrong input on purpose.

### Two items of the step-4 plan that belong to step 5, and why

The sequencing row for this step named two things this PR deliberately does not
do, because neither is a *rendering* question and doing them here would have
put untested behaviour inside the byte-equality proof:

- **`Identity.Equal` merging two indoor units' outdoor identifiers.** The
  shared outdoor unit is already deduplicated, by
  `Discovery.entityIdentity` keying on the outdoor serial and `survivingPoints`
  breaking the tie on `(DeviceID, EmbeddedID)` — [F7](#f7)'s step-2 fix. The
  rendering path consumes that decision rather than re-taking it with
  `Identity.Equal`, which is the right division: the library replaces the
  rendering, not this bridge's domain logic. The proof that the two agree is
  that all 30 configs of the two-indoor multi-split, in both languages,
  reproduce byte for byte — including the 13 shared-outdoor entities whose
  `state_topic` F7 moved.
- **`Origin` + `Precedence("local", "cloud")` for the cloud/Faikin fusion.**
  That is a **state-plane** mechanism — it decides which *reading* wins, not
  what a discovery config says — and the state plane is step 5. It moves no
  byte in any of the 264 payloads. What step 4 does prove about local mode is
  that the local-first scenario's 52 entities, the richest installation
  measured, render identically to the cloud-only ones.

### What this step did NOT do, deliberately

- **No publish path change.** Nothing in `internal/coordinator`, `cmd/` or the
  MQTT bootstrap was touched. `hass.Discovery.Publish` does not call any of
  this.
- **[F14](#f14) is not resolved**, and nothing found here changes the analysis.
  It is the caller's decision and it must be taken before step 6. One thing
  step 4 *can* add to it: the retraction hazard is now concrete — the legacy
  form is `LegacyTopicByUniqueID`, keyed on a `unique_id` that two instances
  produce identically, so **neither instance can tell its own retained configs
  from the other's in either direction**, exactly as [F14](#f14-—-what-step-6-needs-to-know) states. The
  bundle node id (`sanitize(dev.UID())`) is likewise identical between two
  instances, so a bundle publish is a whole-fleet replacement rather than a
  collision Home Assistant could even notice.
- **[F11](#f11), [F4](#d-f4), [F8](#d-f8), [F9](#d-f9), [F12](#d-f12)** — all
  decided at step 2 and none reopened.

### What step 5 and step 6 now know

- The rendering is proved. Step 5 (state and command planes) and step 6 (the
  bundle) are switch-overs with a test behind them.
- `publisher.QoSAtMostOnce` must be spelled in all four QoS fields at step 5
  ([F9](#d-f9)); `publisher.QoS`'s zero value resolves to QoS 1 and omission is
  silent.
- Step 6 must pass `publisher.LegacyTopicByUniqueID` — measured above, 264 of
  264 against 0 and 0.
- The `origin` block ([F12](#d-f12)) is one line and becomes **mandatory** at
  step 6: `discovery.Validate` makes `origin.name` a blocking issue on a bundle
  (and optional on a per-entity config), so the bundle step is where it lands.
- Nothing in the payloads gates step 6. The only open gate is step 3b's live-HA
  question, now narrowed to runtime behaviour on a bundle proved buildable —
  and [F14](#f14).

---

## Step 5 outcome — the state, command and availability planes now publish through go-hamqtt

This section was written by phase 8 step 5, the step where the path step 4
proved **starts actually publishing**. Discovery is still per-entity from the
old builders; the bundle is step 6.

### What moved, and what did not

| Plane | Before | After |
| --- | --- | --- |
| entity state + attributes | nine `mqtt.Publish` sites, four log keys | one `Coordinator.publishState` funnel over `publisher.StatePublisher` |
| a retained clear (empty payload) | `Publish(nil, retain)` | `StatePublisher.Evict` — the same three wire values, said on purpose |
| bridge availability | `PublishOnline`'s own `Publish` | `publisher.Runtime.AnnounceOnline` |
| the Last Will | a literal in `cmd/daikin2mqtt/main.go` | `publisher.Runtime.Will()`, copied field for field by `bridgeWill` |
| `<root>/+/+/+/set` | a hand-written `mqtt.Subscribe` + a retained-flag check | `publisher.CommandRouter`, one route, `DeliverRetained: false` |
| the orphan sweep | the hand-rolled reconcile only | the reconcile, then `Runtime.Sweep(ReportOnly)` reporting beside it |

**The twelve pins hold byte for byte and none of the twelve SHA-256 literals
moved.** `git diff origin/main -- internal/coordinator/testdata` is empty,
`-update-surface-golden` was never passed, and `TestPublishQoSAndRetain` still
reports **985 publishes, all QoS 0 retained** — the same number step 2 left.
The goldens are not passive here: mutations M1, M2, M6b and M10 below each move
a byte and each is caught by all twelve.

### The QoS, and how it was verified

`publisher.QoSAtMostOnce` in all three fields this daemon constructs —
`publisher.Config.QoS` (the availability marker, the will and the sweep
window), `StateConfig.QoS` and `CommandConfig.QoS` — stated as three named
constants in `internal/coordinator/plane.go` with the argument beside them.
The fourth field [F9](#d-f9) names, `AvailabilityConfig.QoS`, has **no site**:
this bridge's availability plane is one bridge-level topic, which
`Runtime.AnnounceOnline` publishes, so no `AvailabilityPublisher` is
constructed at all. That is recorded rather than left as an omission.

Verified **off the transport call**, not off the constant, twice over:
`TestPublishQoSAndRetain` reads the QoS of every one of the 985 recorded
publishes of the twelve scenarios — the pin that survived the whole plane
moving underneath it, which is exactly what it was written for — and
`TestEveryLibraryPublishReachesTheWireAtQoS0` reads the three new paths (state,
the retained clear, the availability marker) off a recorder. A constant-only
assertion would pass with a constant nothing reaches; M1 and M2 prove these do
not.

### Subscription filters — this bridge is homeconnect's case, with evidence

A broker sends one copy per matching subscription, so two overlapping filters
run a handler twice per message. Measured structurally, by registering the real
filters on a `publisher.CommandRouter` — whose `Handle` refuses an overlapping
pair outright — in `TestSubscriptionFiltersCannotOverlap`:

| Filter | Lifetime | Overlaps |
| --- | --- | --- |
| `daikin/+/+/+/set` | permanent, main connection | nothing |
| `state/<faikin host>` | permanent, per mapped device, Faikin connection | nothing |
| `homeassistant/+/+/config` | transient, per reconcile | `homeassistant/#` |
| `homeassistant/#` | transient, per sweep | `homeassistant/+/+/config` |

**The permanent filters are structurally disjoint.** The two discovery filters
**do** overlap — and are never installed together: both run inside
`reconcileOrphans`' single goroutine behind its try-locked gate, and the
reconcile unsubscribes before the sweep subscribes. That is go-homeconnect2mqtt's
situation, not openccu-loom's, and it is asserted rather than asserted-about:
`TestTheReconcileRunsTheReportOnlySweepAfterItsOwnWindow` drives the real
reconcile and pins the exact four-event sequence
`sub …/+/+/config, unsub …/+/+/config, sub …/#, unsub …/#`. Mutation M31 —
deferring the unsubscribe so the two windows overlap — is caught by it.

A second, independent guard comes from the library: `StateConfig.CommandFilters`
refuses a state publish that would land inside this process's own command
subscription. It is **inert today** — every topic the layout produces ends
`/state` or `/attributes` while the filter's last level is the literal `set` —
and inert is asserted rather than assumed: one test hands it a command topic on
purpose, and another asserts over the whole catalogue that no state or
attributes topic matches the filter while every command topic does.

### The report-only sweep — the fan-out, not just the count

`SweepRequest{Owns, Window: 2s, Inspect, ReportOnly: true}`, run inside the
reconcile goroutine after the hand-rolled pass, logged as
`coordinator.discovery_sweep_report`. Measured by
`TestReportOnlySweepOverTheRealFleet` against a broker seeded with one
instance's live fleet plus every class a shared discovery tree actually
carries:

```
15 retained configs offered, 9 inspected, 4 claimed,
  3 another instance's, 1 another integration's,
  1 would be retracted: [homeassistant/sensor/daikin_dev1_retired_sensor/config]
```

Why each survivor survived — this is the part that makes the predicate
reviewable:

| Retained config | Verdict | Declined by |
| --- | --- | --- |
| `sensor/daikin_dev1_room_temperature`, `climate/daikin_dev1_climate`, `switch/daikin_schedule_werktag` | keep | claimed — published in this batch |
| `sensor/daikin_dev1_declared_earlier` | keep | claimed — still in `Runtime.Declared()`, not in this batch |
| `sensor/daikin_dev2_room_temperature`, `climate/daikin_dev2_climate` | keep | **payload** — a sibling instance, same root, a device this one does not poll |
| `sensor/daikin_dev3_room_temperature` | keep | **payload** — a sibling on another MQTT root |
| `sensor/daikin_handwritten` | keep | **payload** — no `daikin_` unique_id of ours |
| `sensor/zigbee2mqtt_0x00124b`, `binary_sensor/tasmota_ABC123_status` | keep | topic — another namespace |
| `device/daikin_dev9` | keep | topic — a bundle, not the four-segment form |
| `sensor/daikinnode/daikin_via_node` | keep | topic — the five-segment node-id form this bridge never published |
| `vacuum/daikin_dev1_hoover` | keep | topic — a platform this daemon never emits |
| `sensor/daikin_dev1_x/config.bak` | keep | topic — does not parse as a config topic |
| `sensor/daikin_dev1_retired_sensor` | **retract** | — ours, unclaimed |

`Inspected` is logged beside the orphan count deliberately: a window that saw
none of this bridge's configs and one that saw them all and correctly found
nothing both read as "0 retracted" in a line that reports only the second
number.

**It stays report-only until step 6, and that is not caution but arithmetic.**
The library's retracting pass compares what it saw against what THE RUNTIME has
claimed, and at step 5 the runtime publishes no config at all — its claim set is
empty, so an armed pass would judge this bridge's entire retained fleet an
orphan and delete it, once per boot. `SweepResult.Unclaimed` is for the same
reason not the answer here and is not used: it is every owned topic minus that
empty set, which is the list that cleared 29 live configs in a sibling repo when
it was mistaken for one. The verdict is computed in `Inspect`, against
`published` ∪ `Runtime.Declared()`, exactly as the acting path will.

### F14 — the `Owns` predicate is not the fix; the payload is

**Decided as a requirement, not left open, and adopted from `go-mtec2mqtt` PR
#54 (its F4).** `hass.Discovery.IsOwnConfig` now requires, **unconditionally**,
that every topic key in the retained payload (bar the bridge-level
`availability_topic`) sits under this instance's own MQTT root **and** under a
device segment this instance actually polls — the ONECTA device ids of the last
resolved poll, plus the scheduler's reserved segment when a scheduler is
attached. It returns **false before the first poll**: ownership that cannot be
proven is not claimed.

There is no flag on it. mtec's earlier version made the same check opt-in, and
its reviewer proved that under the default a staggered two-instance upgrade
deleted the sibling's entire fleet — permanently, since the sibling has no
reason to republish.

The topic predicate (`coordinator.OwnsConfigTopic`) cannot do this job and does
not pretend to: step 4 established that the legacy form is keyed on `unique_id`
and the bundle node id is `sanitize(dev.UID())`, and that **both are identical
between two instances**. Every string `publisher.ConfigTopic` offers is a string
the sibling could have produced. The payload is the only thing that separates
them, because an instance that does not poll a device never writes a topic under
that device's segment.

Pinned by driving the **sweep**, not the predicate:
`TestAStaggeredUpgradeDoesNotReachTheSiblingsFleet` seeds a sibling's whole
five-config fleet — same broker, same discovery prefix, **same MQTT root**, same
namespace, same topic form, different device — opens a real window, and asserts
the retraction list is empty *and* that the topic predicate owned every one of
them, so the test cannot pass by the window seeing nothing. The easy edge (a
sibling on another root) is in the fixture too, beside the hard one rather than
instead of it. Mutations M13, M14 and M22 each re-open the hazard and each is
caught.

Two consequences stated plainly:

- **What this costs a single instance.** A device removed from the ONECTA
  account is no longer polled, so its retained configs are no longer claimed and
  no longer swept: they stay as unavailable entities until cleared by hand. The
  common orphan case — an entity removed or renamed by a catalogue change on a
  device that is still there — is unaffected, because the device id is still
  claimed. Leaving an orphan standing is recoverable; deleting a sibling's fleet
  is not. In `changelog.md`.
- **The residual ambiguity is named, not hidden.** Two instances that both run a
  schedule with the same id write the same switch topic under the same
  `scheduler` segment, so each still claims the other's. That is exactly what
  they do today for everything, it gets no worse, and only an instance
  identifier — the open step-3 decision — can close it. [F14](#f14) is still
  **not resolved**; what changed is that the sweep can no longer be the thing
  that acts on it.

### Birth and LWT — one function, structurally

`publisher.Config` takes the **`Layout`**, not a status-topic literal:
`publisher.New` derives `StatusTopic` from `Layout.Bridge()` and **panics** on a
`StatusTopic` that disagrees with it. So the retained `online`, the CONNECT
will and the `availability_topic` of all 264 payloads are one function —
`internal/layout`'s `BridgeStatus` — by construction rather than by three copies
that happen to match. `cmd/daikin2mqtt` holds no `"offline"` and no status-topic
literal any more; `bridgeWill` copies `publisher.Will` field for field and is
tested, because `run()` is a composition root that dials a broker and blocks.

### The per-connection rebuild — built here, consumed at step 6

`Deps.NewHARuntime` is a `RuntimeFactory`, and `PublishOnline` — which is wired
to the MQTT lifecycle's `OnConnect` — rebuilds the whole `publisher.Runtime` on
every (re)connect, discarding the old one. This PR supersedes nothing, so it
moves no byte; it exists so that step 6 cannot inherit the defect mtec shipped
and its reviewer found: the runtime's memo of what it has superseded, declared
and announced is a statement about **a broker**, made **per process**, and at
QoS 0 a "successful" publish is a statement about **one connection**. After a
reconnect the stale memo reported the retractions as already applied, the
retraction was skipped, the document was published anyway and Home Assistant
refused it — retractions re-sent 0, document published true, configs still
retained 1.

Replacing the object rather than clearing three fields fixes the class: a field
the library adds later is covered the day it is added.
`TestPublishOnlineRebuildsTheRuntimeOnEveryConnect` therefore asserts the object
changed. `PublishOnline` also calls `StatePublisher.Reset()` first, so a broker
back without its retained store stops being told "already published" for every
value.

### Mutation proof — 37 applied, 35 caught, 2 deliberate equivalent survivors

Applied to a **filesystem copy** of a **committed** tree (`cp -a`, never
`git checkout --`), one at a time, whole suite each time.

| # | Mutation | Caught by |
| ---: | --- | --- |
| M1 | `StateQoS` left `QoSUnset` | the twelve goldens + `TestEveryLibraryPublishReachesTheWireAtQoS0` + `TestPublishQoSAndRetain` |
| M2 | `DiscoveryQoS` left `QoSUnset` | the twelve goldens + the same two |
| **M3** | **`CommandQoS` left `QoSUnset`** | **`TestQoSIsStatedNotDefaulted` alone — it moves no published byte** |
| M4 | `LegacyEntityTopics` dropped from `RuntimeConfig` | `TestRuntimeStatesTheLegacyTopicForm` |
| M5 | the legacy form swapped to `LegacyTopicByObjectID` | the same |
| M6 | `Layout` replaced by an **equivalent** literal | **SURVIVED — equivalent** (see below) |
| M6b | `Layout` replaced by a **wrong** literal (mtec's `<prefix>/status/lwt`) | the goldens + `TestAvailabilityModelIsBridgeOnly` + the runtime tests |
| M7 | `resetHAPlane` keeps the old runtime | `TestPublishOnlineRebuildsTheRuntimeOnEveryConnect` |
| M8 | `PublishOnline` stops rebuilding | the same |
| M9 | `PublishOnline` stops resetting the dedup gate | `TestTheDedupGateReopensOnReconnect` |
| M10 | an empty payload published instead of evicted | four goldens + `TestStateTopicBuildersAgree` + `TestSurfaceCensus` |
| **M11** | **one state site publishes straight to `mqtt.Client` again** | **`TestEveryStateTopicGoesThroughTheDedupGate`** |
| M12 | `IsOwnConfig` claims before initialisation | `TestIsOwnConfigClaimsNothingBeforeTheFirstPoll` |
| **M13** | **`IsOwnConfig` back to the namespace check alone** | **`TestAStaggeredUpgradeDoesNotReachTheSiblingsFleet` + 3 more** |
| **M14** | **the device check made conditional on a `state_topic` (the opt-in shape)** | **`TestIsOwnConfig`'s sibling-climate case** |
| M15 | `ClaimDevices` never called from a poll | `TestAPollClaimsTheDevicesItResolved` |
| M16 | the scheduler segment claimed unconditionally | the same |
| M17 | `OwnsConfigTopic` drops the node-id guard | `TestReportOnlySweepOverTheRealFleet` |
| M18 | `OwnsConfigTopic` drops the platform guard | the same |
| M19 | `OwnsConfigTopic` drops the namespace guard | the same |
| M20 | the sweep armed instead of report-only | the same (it published) |
| M21 | the sweep drops the `Declared()` claim set | the same |
| M22 | the sweep ignores the payload predicate | the same + the staggered-upgrade test |
| M23 | the router accepts retained commands | `TestSubscribeWritesDropsRetained` |
| M24 | the will written as a literal again | **MISSED** → `bridgeWill` extracted, then M24b-d |
| M24b/c/d | `bridgeWill` drops retain / writes its own payload / upgrades the QoS | `TestTheWillWritesTheTopicEveryEntityReads` |
| M25 | the deferred transport's wired-check removed | `TestDeferredTransportRefusesUseBeforeWiring` |
| M26 | `checkCommandDisjoint` always passes | **SURVIVED — equivalent** (see below) |
| M27 | the layout grows a topic inside the command filter | `TestNothingThisDaemonPublishesIsAlsoSubscribed` + 20 more |
| M28 | `StateConfig.CommandFilters` dropped | **MISSED** → a test written for it, then caught |
| M29 | the command route widened to `<root>/#` | `TestNothingThisDaemonPublishesIsAlsoSubscribed` |
| M30 | the report-only sweep never run | **MISSED** → a test written for it, then caught |
| M31 | the sweep window opened inside the reconcile's | `TestTheReconcileRunsTheReportOnlySweepAfterItsOwnWindow` |
| **M32** | **the shipped `IsOwnConfig` restored verbatim, escape hatch and all** | **`TestASiblingsStatelessEntitiesAreNeverSwept` + `TestIsOwnConfig` + the sweep report** |
| **M33** | **`availability_topic` included in the device check instead of excluded** | **the same, plus `TestScheduleConfigIsRecognisedAsOwn` — it pins the exclusion as deliberate rather than forgotten** |

Three genuine misses (M24, M28, M30), each closed by a new assertion and
re-verified. M3, M11, M13, M14 and the sweep group move **no published byte**,
so no golden can ever catch one — they are the reason this step has assertions
of its own at all.

**The two survivors are equivalent, and the inert thing is asserted rather than
left blank:**

1. **M6** replaces `Config.Layout` with a literal that happens to be correct.
   Nothing observable changes, because the output is the same string. What is
   lost is the *structural* guarantee — with the Layout set, a disagreement is a
   panic at the composition root instead of a silently dark fleet. M6b shows a
   literal that is wrong is caught everywhere, and
   `TestRuntimeDerivesTheStatusTopicFromTheLayout` asserts the derivation for
   two different roots.
2. **M26** makes `checkCommandDisjoint` always pass. No input the catalogue can
   supply makes it fail today: every topic the layout produces ends `/state` or
   `/attributes`, the filter's last level is the literal `set`. It is an upgrade
   tripwire, not a live check. The property behind it is asserted directly —
   over the whole catalogue, no state or attributes topic matches the filter and
   every command topic does — and **M27**, the change that would actually make
   it matter, is caught.

### Findings

<a name="f15"></a>
**F15 — an empty `HASS_BASE_TOPIC` would put the two planes on different
discovery trees · low, latent.** `publisher.New` substitutes
`discovery.DefaultPrefix` (`homeassistant`) for an empty `Config.Prefix`;
`hass.Discovery` uses the empty string verbatim, giving `/+/+/config`.
`config.Normalize` defaults the key, so the daemon cannot reach it — but the two
packages disagree about what "unset" means, and only one of them says so. Found
by a test that used an unnormalised config. Not fixed; named here and stated at
the one test that could hit it.

<a name="f16"></a>
**F16 — the discovery-signature gate is not re-opened on a reconnect ·
low.** `lastDiscSig` survives a reconnect, so a broker restarted without a
persistent retained store never gets the 264 configs back until the point set
happens to change. The state plane's equivalent is fixed here
(`StatePublisher.Reset` in `PublishOnline`) and the discovery plane's is
deliberately **not**, because it is a change to what goes on the wire on every
reconnect and this step's claim is that nothing does. It resolves naturally at
step 6, where the runtime that owns the claims is itself rebuilt per connection
— provided `lastDiscSig` is cleared with it. **Step 6 must do that;** it is the
one thing in this section that is a task rather than a record.

<a name="f18"></a>
**F18 — the orphan reconcile deleted another instance's climate entities and
refresh buttons · HIGH, live before this PR, fixed here.**

Found by a cross-repository audit while this step was in flight, and it is not
a step-6 hazard: the reconcile subscribes the **shared**
`homeassistant/+/+/config` and `clearOrphanConfigs` gates on `IsOwnConfig`
alone, with no topic pre-filter and no device scoping. It has shipped.

The predicate this bridge carried was

```go
strings.HasPrefix(uid, "daikin_") && (stateTopic == "" || strings.HasPrefix(stateTopic, root+"/"))
```

and `stateTopic == ""` is the whole defect. **24 of the 264 pinned configs
carry no `state_topic`**: the **14 composite climate entities**, which name
`mode_state_topic` / `temperature_state_topic` / `current_temperature_topic`
and never a plain one, and the **10 refresh buttons**, which are stateless by
definition. For those the rule collapses to the `daikin_` namespace — which
every instance of this bridge shares. Measured through the real
`clearOrphanConfigs` against a second instance's **rendered** twenty-config
fleet (`TestASiblingsStatelessEntitiesAreNeverSwept`):

| The sibling is… | The shipped rule retracted | The rule in this PR retracts |
| --- | ---: | ---: |
| on the **same** MQTT root | **20 of 20** — its whole fleet | 0 |
| on a **different** MQTT root | **2** — its climate and its button | 0 |

In Home Assistant that is the sibling's climate cards and refresh buttons
disappearing from the entity registry, dashboards and automations, silently, on
every discovery-signature change — and on a shared root, everything else too.

This is the same shape found in `go-homeconnect2mqtt` the same day (20 of 687,
all buttons). `go-mtec2mqtt` is safe because its rule is unconditional and it
builds no buttons; `go-zendure2mqtt` is safe because what remains when the field
is absent is the operator-configured root, which a sibling does not share. **The
general lesson: when the keyed field is absent, what remains must still be
instance-specific.** Here it collapsed to a compile-time literal.

The fix is the [F14](#f14-—-what-step-6-needs-to-know) work above and needs no
payload change: every topic key in the payload, not one field that 24 configs do
not have. Note that the *field* choice differs from mtec's deliberately — mtec
keys on `state_topic` because its availability topic is bridge-level and
serial-free by design; here the availability topic is likewise bridge-level and
therefore **not** instance-specific either, so it is excluded from the check
rather than used as the anchor. What was copied from mtec is the
unconditionality, not the field.

Two test weaknesses let it survive this long, and both are fixed: the
`IsOwnConfig` fixtures were hand-written and included a "climate (no
state_topic)" case asserting `want: true` — **the hole encoded as intended
behaviour** — and the only test driving the sweep used three `sensor` fixtures
whose "foreign" payload was a *different integration*, never a sibling instance
of this bridge. The regression test renders its fixtures with the real builders
and its sibling is another scenario of this same bridge, on the same root, with
its climate and its button in the fixture.

<a name="f17"></a>
**F17 — `AvailabilityConfig.QoS` has no site on this bridge · informational.**
[F9](#d-f9) names four QoS fields to spell. Three exist here; the fourth belongs
to `publisher.AvailabilityPublisher`, which this bridge does not construct
because its availability plane is one bridge-level topic that
`Runtime.AnnounceOnline` publishes. Recorded so a later reader does not go
looking for a fourth `QoSAtMostOnce` and conclude it was forgotten.

### What this step did NOT do

- **No bundle, no retraction, no `origin` block.** `PublishBundle` is never
  called and nothing is superseded; [F12](#d-f12) stays out because it is a
  payload addition on all 264 configs. `LegacyEntityTopics` IS stated, one step
  early, because it costs nothing while nothing publishes a bundle and because a
  composition root that says nothing looks exactly like one that chose the
  default.
- **The discovery plane still publishes through `hass.Discovery.Publish`.** That
  is step 6.
- **`Runtime.WatchBirth` is not wired.** It replays `Declared()`, which is empty
  until step 6 publishes through the runtime; wiring it now would subscribe
  `homeassistant/status` for nothing. It belongs with the bundle.
- **`CommandRouter.Resubscribe` is deliberately NOT wired to `OnConnect`.**
  go-mqtt resubscribes its own subscriptions on reconnect, so calling it would
  register the filter twice and the broker would deliver every command twice —
  the exact hazard this step spent its overlap analysis on.

---

## Step 6 outcome — the discovery plane is the device bundle

This section was written by phase 8 step 6, the last step and the only one that
can destroy an installed base. Everything before it was reversible.

### What moved

| Plane | Before | After |
| --- | --- | --- |
| discovery | 264 retained per-entity configs, `hass.Discovery.Publish` | **31 retained device documents**, `publisher.Runtime.PublishBundle` |
| the retraction | — | `publisher.SupersededTopics`, inside `PublishBundle`, **264 of 264** |
| orphan removal | a hand-rolled `homeassistant/+/+/config` window + the library's report-only pass | **one** armed `homeassistant/#` pass, payload-gated |
| a removed entity | retracting its per-entity config | **a tombstone** in the document |
| `origin` | absent (F12) | present, mandatory — `discovery.Validate` makes `origin.name` blocking on a bundle |

### The twelve pins held, and what they now pin

**All twelve SHA-256 literals in `goldenDigests` are byte-identical to what #79
left, `-update-surface-golden` was never passed, and
`git diff origin/main -- internal/coordinator/testdata/surface` is empty.**

They could not stay a snapshot of the wire — the wire changed — so they were
re-framed rather than regenerated: the twelve goldens are now **the retraction
contract**. `TestTheRetractionCoversTheWholePinnedFleet` compares the topics
`SupersededTopics` derives from the rendered documents against the config topics
the goldens pin, per scenario: **264 of 264, none missing, none invented**. The
two sides come from genuinely different places — a pinned file and the library
rendering the model — and nothing but that test compares them, which is the
shape (a value spelled twice with nothing comparing the spellings) that let
go-mtec2mqtt's composition root drop its legacy topic form unnoticed.

`buildSurface` therefore assembles the per-entity surface from two places on
purpose: the state/command/availability planes from the **real publish path**,
the discovery plane from the per-entity builders over the same re-derived
inputs `TestHamqttInputsReproduceThePublishedConfigTopics` already validates.
`hass.Discovery.Publish` stays in the tree for exactly that reason and says so
in its own doc comment.

**The bundle is a new pinned artefact**: `testdata/bundle/<scenario>.json`,
twelve files, its own `-update-bundle-golden` flag (so a bundle test cannot
reach the per-entity one) and its own `bundleDigests`. Each file pins the
document **and** the retraction list, because they fail differently and both
silently: a wrong topic publishes a valid document where Home Assistant never
looks; a wrong payload publishes to the right place a document Home Assistant
discards without a line in its log. `goldenBundleSW = "0.0.0-golden"` keeps a
release tag from staling twelve digests, and the real wiring is asserted
separately — at the point the document is **sent**, not where the origin is
built, which is go-mtec2mqtt's finding.

### Retraction completeness, established across a RECONNECT

The ordering itself is the library's: `PublishBundle` checks its byte dedup,
then supersedes, then publishes, aborting before the document if any retraction
fails. What this bridge owns is the two things that make it true.

1. **The form.** `publisher.Config.LegacyEntityTopics` REPLACES the library's
   default. `hass.LegacyConfigTopicForms()` states `LegacyTopicByUniqueID`, and
   `TestTheRuledOutLegacyFormsRetractNothing` measures both ruled-out forms at
   **0 of 264** so the verdict cannot be an accident of overlap. Bypassing
   `RuntimeConfig` is now a **boot panic**, with the want side derived from
   `RuntimeConfig` rather than written out again, and both halves pinned —
   the check (`TestARuntimeWithoutTheLegacyFormIsABootPanic`) and the fact that
   the composition root reaches it
   (`TestNewRefusesARuntimeBuiltWithoutRuntimeConfig`).
2. **The runtime's age.** `Runtime`'s `superseded` memo is a statement about a
   broker made per process, and at QoS 0 a successful publish is a statement
   about one connection. Step 5 built `RuntimeFactory` +
   `Coordinator.resetHAPlane` for this; step 6 consumes it.
   `TestAReconnectReSendsTheRetractionsBeforeTheDocument` drives a
   **reconnect** — `PublishOnline` on a live coordinator, which is what the MQTT
   lifecycle's `OnConnect` calls — and asserts **both** halves: every retraction
   goes out again, and every one precedes the document. mtec's own test drove a
   process *restart*, which is why its gap survived a release; the restart case
   is here too, as the crash-window test, and it is a second coordinator over a
   second runtime rather than a simulated one.

### F16 — done, and how

`PublishOnline` now clears `lastDiscSig` (and `priorLoaded`/`priorComponents`)
and bumps a `discoveryGen` counter, beside the runtime rebuild and the state
plane's `Reset`. A broker restarted without its retained store gets the whole
fleet back on the next connect instead of staying empty until the entity set
happens to change — which, on a stable installation, is never. It costs no
traffic when nothing moved: the library's byte-level gate underneath returns
`written=false` for an identical document. `discoveryGen` closes the other half:
a batch published on a connection that has since been replaced does not commit
its signature, or it would suppress the republish the new connection is owed.

### Arming the sweep, and `SweepResult.Unclaimed`

**Armed.** Step 5's reason for not arming was arithmetic — the runtime published
no config, so its claim set was empty and an acting pass would have deleted the
fleet. That is now established rather than assumed:
`TestTheClaimSetIsPopulatedByTheDocumentsThatWentOut` reads
`publisher.Runtime.Declared()` **off the runtime** after a poll and requires the
document topic to be in it, and requires nothing of the per-entity form to be.

The guard asks **"was a document published"**, never "was one built" — the two
come apart exactly where it matters, and go-mtec2mqtt logged the first while
testing the second. The sweep does not run unless every document of the batch
reached the socket (`TestTheSweepIsArmedOnlyOverAPopulatedClaimSet`), and the
claim set names even a failed document so a transient error cannot make the
sweep clear a config this daemon still intends to publish.

**`SweepResult.Unclaimed` is still not used, and now permanently.** It is
topic-only, and every string `publisher.ConfigTopic` offers is byte-identical
between two instances seeing one ONECTA device (F14): acting on it deletes a
sibling's fleet. The verdict is taken in `Inspect`, against the retained
**payload**, and `OwnsConfigTopic` continues to decline device documents so this
daemon can never retract a bundle — its own or anyone's.

The hand-rolled reconcile is **gone**, and with it the two overlapping windows
step 5 had to sequence. One window, one predicate, one retraction.

### The document sizes, and the preflight

Measured over the twelve scenarios: **31 documents carrying 264 components**,
largest packet **14 172 bytes** (`multisplit.local.en`, the
809d41d9 indoor unit: 13 951 bytes of payload on a 71-byte topic), average
document ~6 KB.

`Coordinator.bundleFits` checks that against the broker's **advertised**
Maximum Packet Size (`mqtt.ConnectResult.MaximumPacketSize`, MQTT 5.0 property
0x27) **before** the retraction, plus a 64-byte margin for the fixed header,
the remaining-length varint, the topic-length prefix and the v5 property block.
It is emphatically **not** `mqtt.TCPConfig.MaximumPacketSize`, which is this
client's own inbound cap and says nothing about what a broker will accept —
go-mtec2mqtt's notes conflated the two.

Unknown is unknown, not small: no hook, no connect yet, an MQTT 3.1.1 link and
a broker that set no limit all skip the check, because MQTT's meaning of an
absent property IS "no limit". The guard can therefore only ever prevent a
publish that would genuinely have failed. A withheld document leaves that
device's per-entity configs retained and its entities working, and logs
`coordinator.discovery_bundle_too_large`.

`discovery.Validate` is applied at **build** time for the same reason, and it is
tested against a genuinely invalid document rather than against the measurement
that says none of this bridge's are — otherwise the gate would be untested code,
which a mutation proved (M7 survived the first pass).

### The crash window

Retractions out, document not: the device has **no** discovery config and its
entities are absent from Home Assistant — not unavailable, absent.

It heals because this daemon remembers nothing across boots.
`TestTheCrashWindowHealsOnTheNextBoot` puts one process in exactly that state
(the broker refuses every `homeassistant/device/…` publish), asserts the
retractions went out and the document did not, then drives a **second
coordinator over a second runtime** and asserts the next boot does **both**:
re-sends every retraction *and* publishes the document. Both matter — a boot
that trusted a previous process's retraction is a boot that publishes into a
conflict.

### Tombstones — implemented, not deferred

go-mtec2mqtt deferred this and shipped a capability regression. The same
deferral costs **more** here, because this bridge's component set shrinks for
three ordinary reasons: a weekly schedule deleted, `LOCAL_MODE` turned off, and
a `characteristics.yaml` edit shipped with a release. Under the per-entity form
the orphan sweep removed the entity; on a document it cannot, because
`OwnsConfigTopic` declines bundles (deliberately — widening it is the sibling
hazard). An omitted component is not removed: the entity stays, and because this
bridge's availability is bridge-level and the bridge is online it reads
**available**, with nothing on screen to say it is dead.

So the previous document is read back — once per connection, before the first
publish of that connection, through the same `Sweep(ReportOnly)` machinery —
and every component the previous document carried and the new one does not is
marked with `Bundle.RemoveComponents`: a platform-only entry in `components`,
and the removed `unique_id` remembered in `Bundle.Tombstones` (`json:"-"`,
outside the payload, because putting it back would un-remove the entity) so
`SupersededTopics` also clears its retained per-entity config.

**Every failure direction of the read-back produces FEWER tombstones, never
different ones** — a window that sees nothing, a document that does not parse, a
component with no platform or a `unique_id` outside this namespace, and a
document the payload predicate does not claim all simply do not become prior
state. That is what makes a read-back safe to put in front of the one publish
that cannot be undone: its failure mode is the behaviour of not having it.
`TestTheTombstoneReadBackIgnoresASiblingsDocument` pins the one direction that
would be destructive.

`BundleIsOwnConfig` is the payload predicate for a document: the per-entity rule
applied component by component, `availability_topic` excluded for the same
reason as before (it is bridge-level and therefore not instance-specific), and a
document that named no topic of ours refused outright — F18's lesson, that what
remains when the keyed field is absent must still be instance-specific.

### F15 — both directions

`config.Normalize` now trims surrounding slashes off `MQTT_TOPIC` and
`HASS_BASE_TOPIC` **before** the empty check. Empty was already covered;
trailing slash was not, and it is the mirror-image of the defect go-mtec2mqtt
shipped (a birth topic spelled twice, diverging on a trailing slash, so entities
were gone after every HA restart until the daemon restarted).
`TestTheTwoPlanesAgreeOnTheDiscoveryPrefix` feeds the **raw** value through the
real normalisation and requires the runtime's prefix, this package's config
filter and the bridge topic to agree, for six spellings.

### F14 — what this step did and did not make reachable

**Nothing unrecoverable became reachable, and the reason is structural: the
retraction list is derived from this instance's OWN documents.**
`SupersededTopics` renders it from the components a document carries, so an
instance only ever retracts the per-entity configs of devices it polls. Two
instances on disjoint ONECTA accounts — the home-plus-holiday-home case — have
disjoint node ids and disjoint retraction lists and cannot touch each other.
That is strictly better than before #79.

What IS newly reachable, for two instances seeing the **same** devices with
**different** configuration:

- overwriting goes from per-entity to **whole-device**: the same flip-flop, at a
  coarser grain;
- during a **staggered upgrade** the upgraded instance retracts per-entity
  configs the old-build sibling republishes, so its entities appear and
  disappear until it too is upgraded.

Both are transient and self-healing, and both are the same "they overwrite"
class two identically-keyed instances are already in. The permanent,
unrecoverable failure mtec's reviewer proved — one instance deleting a
sibling's fleet with no reason for the sibling to republish — stays closed, by
`IsOwnConfig`/`BundleIsOwnConfig` being unconditional and by the sweep never
claiming a document.

**F14 itself is still not resolved**: an instance identifier is still the only
thing that separates two instances on one account, and it is still step 3(c)'s
decision. This step does not take it and does not need to, but it narrows what
it has to cover — a document-level, not entity-level, collision.

### Mutation proof — 26 applied, 25 caught, 1 deliberate equivalent survivor

Applied to a **filesystem copy** (`cp -a`) of a **committed** tree, one at a
time, whole suite each time.

| # | Mutation | Caught by |
| ---: | --- | --- |
| M1 | `LegacyEntityTopics` dropped from `RuntimeConfig` | `TestRuntimeStatesTheLegacyTopicForm` + the boot check |
| M2 | the document published through `Runtime.Publish` — no retraction at all | the reconnect + crash-window tests |
| **M3** | **`PublishOnline` stops clearing `lastDiscSig` (F16)** | **`TestTheDiscoveryGateReopensOnEveryConnect`** |
| M4 | `resetHAPlane` keeps the old runtime | three tests, incl. the reconnect |
| M5 | the packet-size preflight removed | `TestAnOversizedDocumentIsWithheldBeforeAnythingIsRetracted` |
| M6 | an unknown broker maximum treated as small | `TestAnUnknownPacketLimitIsNotTreatedAsASmallOne` |
| **M7** | **`discovery.Validate` skipped** | **MISSED** → `TestAnInvalidDocumentIsRefusedBeforeItIsPublished`, then caught |
| M8 | the sweep runs with nothing published | `TestTheSweepIsArmedOnlyOverAPopulatedClaimSet` |
| M9 | the sweep retracts `SweepResult.Unclaimed` | `TestTheArmedSweepClearsOnlyItsOwnOrphans` |
| M10 | the sweep ignores the payload predicate | four tests, incl. the staggered-upgrade one |
| M11 | `OwnsConfigTopic` claims device documents | `TestReportOnlySweepOverTheRealFleet` |
| M12 | tombstones never applied | both tombstone tests |
| **M13** | **the read-back ignores `BundleIsOwnConfig`** | **MISSED** → `TestTheTombstoneReadBackIgnoresASiblingsDocument`, then caught |
| **M14** | **`recordPublished` carries tombstones forward as prior state** | **MISSED** → `TestATombstoneIsNotRepeatedWithinOneProcess`, then caught |
| M15 | `BundleIsOwnConfig` drops the "named a topic of ours" test | `TestASiblingsDeviceDocumentIsNeverMistakenForOurs` |
| M16 | `BundleIsOwnConfig`'s pre-first-poll guard removed | **SURVIVED — equivalent** (see below) |
| M17 | the `origin` block dropped | the twelve bundle goldens |
| **M18** | **the origin's `sw_version` wired to a literal** | **MISSED** → `TestThePublishedDocumentCarriesThisBuildsVersion`, then caught |
| M19 | `config.Normalize` stops trimming slashes (F15) | `TestTheTwoPlanesAgreeOnTheDiscoveryPrefix` |
| M20 | `availability_topic` used as an ownership anchor (F18) | six tests |
| M21 | a failed publish still commits the signature and sweeps | `TestDiscoveryRetriedAfterPublishFailure` |
| **M22** | **a failed document dropped from the claim set** | **MISSED** → `TestAFailedDocumentStaysInTheClaimSet`, then caught |
| **M23** | **`checkLegacyForms` never called from `New`** | **MISSED** → `TestNewRefusesARuntimeBuiltWithoutRuntimeConfig`, then caught |
| **M24** | **the document topic concatenated (`Bundle.Topic`) rather than normalised** | **MISSED** → `TestTheRenderedTopicIsTheOneTheRuntimeWrites`, then caught |
| M25 | `BundleComponents` accepts foreign components as prior state | `TestATombstoneIsNotRepeatedForever` |
| **M26** | **the generation guard removed** | **MISSED** → `TestABatchFromAReplacedConnectionDoesNotCommitItsSignature`, then caught |

Seven genuine misses (M7, M13, M14, M18, M22, M23, M24, M26 — the last is
eight; M22 is arguably the ninth and was closed anyway), every one closed by a
new assertion and re-verified. Several of them move **no published byte**, which
is why they needed assertions written for exactly that reason.

**The one survivor is equivalent, and is asserted rather than left blank.**
`BundleIsOwnConfig`'s `len(owned) == 0` early return changes no behaviour: an
empty claim set makes `ownsTopic` decline every topic, and a document naming
none is refused by the `named > 0` test below it. The line stays because the
rule is easier to read at the top than to reconstruct from two conditions
further down, and the doc comment says so.

**`-race` was run locally on the whole suite before pushing**, and it found one
thing: a plain counter written from parallel subtests in
`TestTheRetractionCoversTheWholePinnedFleet` — the same CI-only shape step 5
shipped. Fixed before the push.

### What this step did NOT do

- **`Runtime.WatchBirth` is still not wired.** It would subscribe
  `homeassistant/status` and replay `Declared()` on a Home Assistant restart —
  but the documents are RETAINED, so a restarting Home Assistant re-reads them
  from the broker anyway, and the failure it would guard (a broker losing its
  retained store) is what F16's gate-clearing now covers. Re-arming it per
  connection would also double-subscribe, since go-mqtt resubscribes its own
  subscriptions on reconnect — the hazard step 5 spent its overlap analysis on.
- **F14 is not decided**, see above.
- **No version bump and no release notes beyond `changelog.md`/
  `addon/CHANGELOG.md`.** That is step 7.
- **`hass.Discovery.Publish` and the per-entity builders are not deleted.** They
  are what the twelve pins are built from and therefore what makes the
  retraction contract checkable. Removing them is a step-7 question and must be
  answered together with the pins.

---

## Sequencing — the rest of phase 8

Ordered so each step de-risks the next, following the shape phases 5, 6 and 7
converged on.

| Step | Work | Why here |
| ---: | --- | --- |
| **0** | **This PR.** Bump `go-mqtt` v1.3.0 → v1.5.1; add the twelve-scenario surface pin, the digests, the eleven builder invariants and `.gitattributes`; measure and record F1–F14. | Nothing can be proved byte-equal against a surface that was never captured. |
| 1 | **Done.** Housekeeping the bump enables: `mqtt.SplitClient` replaces the hand-rolled `mqttSession`; `ConnectWithRetry` deliberately declined. Payloads untouched, goldens unmoved. | Small, mechanical, and a free check that the pins do not fire on a non-payload change. |
| **2** | **Done** — see [Step 2 outcome](#step-2-outcome--what-was-fixed-what-was-decided-what-stays). F1, F3, F5, F10, F13 first (no bytes move), then F2, F7, F6 (41 keys changed, 2 topics added, 5 of 12 scenarios, 5 digests updated by hand). F4, F8, F9, F11, F12 and F14 decided in writing. | The byte-equality proof in step 5 must compare against *corrected* bytes, not against bugs. F1 was first because it is the only finding that takes a live installation down. |
| 3 | (a) and (b) are **decided above** — F4 keeps this bridge's three normalisers, F8 takes `model.BridgeOnly()`. What is left is (c) F14: whether an `INSTANCE_ID` lands before the bundle, plus the `availability_topic`-vs-`availability`-list spelling question F8's decision raises. | All are irreversible for an installed base. Phase 6 hit (a) at step 3 and paid for it; this phase settled it at step 2 instead. |
| 3b | **Settle §3.4's third unknown against a live Home Assistant.** One throwaway bundle carrying a `climate` component whose `current_temperature_topic` is another component's state topic, HA 2026.9, watch the log. | Half a day, and it gates step 6 for the one bridge whose composite entity is the point of the phase. |
| **4** | **Done** — see [Step 4 outcome](#step-4-outcome--go-hamqtt-reproduces-the-published-surface-byte-for-byte). **Model the catalogue as `model.Entity` and render, publishing nothing.** The composite climate as `Bindings` + `Suppressor` + `Builder`; the shared outdoor unit as `Identity.Equal` merging two indoor units' outdoor identifiers; the cloud/Faikin fusion as `Origin` + `Precedence("local", "cloud")`. Compare the rendered per-component output **against the golden files, not against the builder it replaces**. Neither pin regenerated. | This is where a `Layout`, `Context`, `Slot` or composite mismatch surfaces, at zero risk — and it is the part ADR 0070 §3.5 nominates daikin to prove. |
| **5** | **Done** — see [Step 5 outcome](#step-5-outcome--the-state-command-and-availability-planes-now-publish-through-go-hamqtt). State, command, birth/LWT and a report-only sweep on the library; `publisher.QoSAtMostOnce` in all three fields that exist (F9); F14's claim predicate tightened to the payload. Twelve goldens and twelve digests unmoved. | The state plane has no registry keys to orphan; it is the cheap half. |
| **6** | **Done** — see [Step 6 outcome](#step-6-outcome--the-discovery-plane-is-the-device-bundle). **Switch discovery to the device bundle.** `PublishBundle` + `SupersededTopics(prefix, bundle, publisher.LegacyTopicByUniqueID)` (F5), retracting all per-entity configs before the bundle lands, and teach `IsOwnConfig` to recognise a bundle in both directions. Verify against a live HA that no `Received a conflicting MQTT discovery message` warning appears. | The one step no unit test can prove. Everything above exists to make it a small diff. |
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
  one whose diff is a hundred rows is not. *(Done at step 2, as intended: 41
  keys, every one named to its finding.)*
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
