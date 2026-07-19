# Design: local-first control & multi-split outdoor-unit handling

This document describes the architecture added in the 0.2.x series: a second
control backend that drives the indoor units through their **local Faikin
interface**, and the handling of settings that ONECTA exposes per indoor unit
but that are physically shared across one **multi-split outdoor unit**.

It complements the package-level doc comments; read those for exact signatures.

## Goals

1. **Local-first control** — when enabled, read and write the indoor units over
   their local Faikin / Faikout MQTT interface instead of the rate-limited
   ONECTA cloud, while keeping the same Home Assistant entities and MQTT topics.
2. **Correct multi-split behaviour** — settings that are one physical knob on
   the shared outdoor unit (operation mode, outdoor silent, eco, demand limit)
   must stay consistent across all indoor units; mutually-exclusive settings
   (powerful ⇄ eco) must not be set together.
3. **No rework** — the two are independent: control routing is a seam both the
   cloud and the local path plug into, and the outdoor/dependency logic sits
   above the backend so it applies to either.

## Background: multi-split constraints

A standard multi-split (e.g. a `3MXM` outdoor unit with several `FTXA` indoor
units) runs one refrigerant cycle, so:

- **Operation mode (heat/cool)** cannot differ between simultaneously-running
  indoor units — the first unit to start wins; conflicting units drop to
  standby (Daikin FAQ: *"…either cooling or heating at the same time"*).
- **Outdoor silent** (Außen Geräuscharm) caps the single outdoor unit's
  fan/noise; **demand control** limits its power draw; **eco** (econo) limits the
  shared compressor too. All are exposed per indoor unit in ONECTA but act on the
  one outdoor unit. eco is also mutually exclusive with **powerful** (boost),
  which stays per indoor unit but drives the same shared compressor — see
  "Mutual exclusion" below.

Genuinely per-indoor-unit settings (independent): setpoint, fan, swing,
powerful, streamer, on/off.

## Control backend seam

`internal/coordinator/backend.go` routes every write through one entry point:

```
setCharacteristic(deviceID, embeddedID, characteristic, value, path)
  ├─ device mapped to a Faikin host AND characteristic locally controllable?
  │     → publish command/<host>/<suffix>  (e.g. command/<host>/quiet "true")  (local)
  └─ otherwise                                     → client.Patch(...)  (cloud)
```

`faikinCommand` translates an ONECTA characteristic write into a dedicated
per-setting command — topic suffix + payload (e.g. `operationMode "cooling"` →
`command/<host>/mode "C"`; `outdoorSilentMode "on"` → `command/<host>/quiet
"1"`). Anything Faikin does not model returns `ok=false` and **falls back to the
cloud**, so local mode degrades gracefully rather than dropping a command.
Unmapped devices always use the cloud, even in local mode.

## Local-first path

`internal/faikin` is a pure translation layer (no I/O) over the revk
Faikin/Faikout firmware:

- **State** — a retained JSON document on `state/<host>` (see the reference
  below). `ParseState` decodes it; `State.HAMode()` maps `power`+`mode` to an HA
  `hvac_mode`.
- **Command** — a dedicated per-setting topic `command/<host>/<suffix>` (e.g.
  `command/<host>/quiet`, `/power`, `/mode`, `/temp`, `/demand`), with a
  `true`/`false` payload for switches. This mirrors the firmware's own HA
  discovery; the combined `command/control` JSON does not take effect for outdoor
  silent on multi-split units.

### Reads (`internal/coordinator/local.go`)

On start, the coordinator subscribes to `state/<host>` for every mapped device
(`subscribeLocal`). Each update is translated (`localStateMessages`) and
republished to the **same** per-unit topics the cloud path uses
(`<root>/<deviceID>/<embeddedID>/<topic>/state`), with identical value formats
(localized select labels, catalog precision), so HA sees the same entities
regardless of backend. The `embeddedID` is taken from a cache populated by the
cloud poll — so the cloud bootstraps device structure and HA discovery once,
then local state takes over. The cloud poll **skips** the locally-owned topics
for mapped devices (`localTopics`) to avoid redundant writes.

Two refinements are essential here:

- **OS/heartbeat filtering** — Faikin interleaves OS/heartbeat documents on
  `state/<host>` (uptime, rssi, mem, … with **no AC fields**) between full
  state documents. Parsing those would decode to the `State` zero value and
  publish `power off`, `temp 0`, `outdoor_silent off`, … resetting every
  entity. `ParseState` sets `State.HasAC` from the presence of `power`, and the
  read path skips messages where it is false.
- **Synthesized discovery for local-only settings** — HA discovery is driven by
  the cloud poll, so settings the cloud does not expose for a unit (econo,
  streamer, outdoor silent, demand on the FTXA range — Faikin reads them off the
  serial bus) would publish local state but get no discovery config, and no
  entity would appear. `localOnlyPoints` synthesizes discovery points for these
  topics per mapped device (skipping any the cloud already resolves); their live
  state still comes from the Faikin read path.

### Device mapping & broker

`LOCAL_DEVICE_MAP` maps each ONECTA device ID to a Faikin host. It accepts a
YAML map or an `id=host,…` string (the latter so it survives the scalar-only env
/ add-on path; see `config.DeviceMap`). The Faikin broker defaults to the main
MQTT broker — in that common case the **existing MQTT connection is reused**; a
distinct `LOCAL_FAIKIN_SERVER` opens a second connection. Credentials fall back
to the main MQTT login.

## Outdoor-shared settings & dependency engine

`internal/coordinator/outdoor.go` builds **outdoor groups** from each device's
outdoor-unit serial (`updateOutdoorGroups`); `groupMembers` returns the indoor
units sharing one outdoor unit. On this it implements the dependent settings:

| Mechanism | Trigger → effect | Flag (default on) |
|---|---|---|
| **Mode sync** | heat/cool change → switch the group members **running in the opposite compressor direction** to the new mode (last write wins) | `MULTISPLIT_MODE_SYNC` |
| **Outdoor fan-out** | write to a `scope: outdoor` setting → apply to every group member | `MULTISPLIT_OUTDOOR_AGGREGATE` |
| **Mutual exclusion** | eco on → clear powerful; powerful on → suspend eco group-wide and restore it (save/restore) when the boost ends | `ENFORCE_MUTUAL_EXCLUSIVE` |

These run **above** `setCharacteristic`, so they fan out through whichever
backend each member uses.

**Mode sync never wakes a sleeping unit.** Turning one indoor unit on (e.g. to
cool) must not switch on the rest of the house: the sync only writes to members
that are **known to be running** in the opposite compressor direction (`heating`
vs `cooling`/`dry`; `auto`/`fanOnly` demand no direction and neither trigger nor
receive a sync). This matters doubly on the local path, where the Faikin
`command/<host>/mode` topic force-powers the unit on for any mode value — a
blind sync would turn every unit on. Power and mode come from a per-device cache
(`powerCache`/`modeCache`) fed by the cloud poll, the Faikin state feed, and
each successful write (`noteWrite`, so back-to-back commands see the value just
written); for locally-mapped devices the lagging cloud snapshot only bootstraps
missing entries and never overwrites the fresher local value. Off units keep
their stored mode — when later switched on via HA, the climate command carries
power + mode, and the sync then runs from that unit (last write wins). Skipping
off units also saves ONECTA daily-quota requests in cloud mode.

**Powerful ⇄ eco save/restore.** eco (econo) is `scope: outdoor` because it limits
the shared compressor, but powerful (boost) stays per indoor unit and drives the
same compressor, so they cannot coexist. Turning eco on clears powerful group-wide
(`enforceMutualExclusive`). The other direction needs more than a blind clear: the
hardware suspends eco while powerful runs but does **not** restore it afterwards.
`reconcileEconoSuspend` therefore drives an edge-based, group-keyed state machine
(`Coordinator.econoSuspend`) off the observed `(anyPowerful, econoOn)` snapshot:
on the group's first powerful it remembers whether eco was on and switches it off
across the group; on the last member leaving powerful it restores eco. It is fed
from both the local read path (`publishLocalState`, before `publishOutdoorShared`)
and the cloud poll (`reconcileEconoSuspendCloud`, which skips locally-active
groups), so a manual powerful-off and the 20-minute hardware timeout are handled
by the same code (the timeout simply shows up as `powerful:false` on the next
read). Being edge-driven, the restore's own eco write is not re-triggered, so it
never loops.

### `scope: outdoor` in the catalog & discovery

A catalog entry can carry `scope: outdoor` (`internal/catalog`). On the write
side it triggers fan-out; on the discovery side
(`internal/hass/discovery.go`), such points are keyed by the **outdoor serial**
and attached to the outdoor device, so all indoor units' copies deduplicate to a
**single entity per outdoor unit** (`outdoor_silent`, `econo_mode`,
`demand_control`, plus the outdoor telemetry below).

### Outdoor-unit telemetry aggregation

Several Faikin telemetry fields are catalogued `scope: outdoor` so they surface
as one entity per outdoor unit; `localOutdoorAgg`/`publishOutdoorShared` combine
them across the group, but the rule differs by what the field physically is
(established from live multi-split data, where each active indoor unit reports
its **own** energy/power while the compressor frequency is identical everywhere):

- **Per-unit, summed** — `power_consumption`, `energy_total`,
  `heating_energy_total`, `cooling_energy_total` are each indoor unit's own
  reading (confirmed in the firmware: the energy fields decode S21 responses, and
  `energyheat`/`energycool` are command `'U'`, labelled *"Per device power"*), so
  the outdoor (system) total is the **sum across members**. Power is instantaneous
  (an idle unit reports ~0, correct). Energy is a `total_increasing` lifetime
  counter, and an idle unit stops reporting it (reads 0), which would drop the
  sum — so each unit's energy is **held** at its highest seen value
  (`Coordinator.lastEnergy`) and the held values are summed; the sum is never
  republished as 0. `aggregateEnergy` guards the one ambiguous field: Faikin's
  `energy` is the *outside power meter*, which on some hardware is a single shared
  counter every unit reports identically — so when all reporting members show the
  same value it is treated as one shared meter (returned as-is, not multiplied)
  rather than summed.
- **Shared, max** — `compressor_frequency` is the single outdoor compressor's
  speed, reported identically by every member, so the aggregate is the max.

Genuinely per-indoor-unit telemetry (`fan_frequency`, `refrigerant_temperature`)
stays per unit (not `scope: outdoor`).

## Catalog additions

Per-unit switch `streamer`; outdoor-shared `outdoor_silent` and `econo_mode`
(switches) and `demand_control` (number); local-only telemetry sensors (outdoor:
`power_consumption`, `compressor_frequency`, `energy_total`,
`heating_energy_total`, `cooling_energy_total`; per-unit: `fan_frequency`,
`refrigerant_temperature`). Faikin exposes all of them locally. On the FTXA range
the settings are **absent from the ONECTA cloud JSON** (confirmed via the device
browser — only as nested `schedule` action types) and the telemetry has no cloud
equivalent at all, so in cloud mode they never resolve; in local mode they appear
via `localOnlyPoints` (above, matched on the synthetic `faikinLocal`
characteristic). `demand_control`'s cloud-side nested PATCH is best-effort and
should be verified against a live `daikin2mqtt-util points <id>` dump.

## Configuration reference

```yaml
LOCAL_MODE: false                  # master switch for local-first
LOCAL_FAIKIN_SERVER: ""            # empty -> same broker as MQTT_SERVER
LOCAL_FAIKIN_PORT: 1883
LOCAL_FAIKIN_LOGIN: ""             # empty -> MQTT_LOGIN
LOCAL_FAIKIN_PASSWORD: ""          # empty -> MQTT_PASSWORD
LOCAL_FAIKIN_PREFIX: Faikout       # deprecated/ignored (command topic is command/<host>/<suffix>)
LOCAL_DEVICE_MAP: {}               # {deviceID: host} or "id=host,id=host"
MULTISPLIT_MODE_SYNC: true
MULTISPLIT_OUTDOOR_AGGREGATE: true
ENFORCE_MUTUAL_EXCLUSIVE: true
```

Every key is overridable via `DAIKIN_<KEY>` env. The Home Assistant add-on
exposes them as options (`local_device_map` as a list of `id=host` strings,
joined into the scalar form by `script/run.sh`).

## Faikin MQTT reference (captured)

State topic `state/<host>` (relevant fields):

```json
{"online":true,"power":true,"mode":"cool","target":22.5,"temp":21.0,
 "hum":66.0,"outside":28.0,"fan":"auto","swing":"off","quiet":false,
 "econo":true,"powerful":false,"streamer":false,"demand":100,
 "energy":772600,"energyheat":71000,"energycool":117300}
```

Commands use dedicated per-setting topics `command/<host>/<suffix>` (the app
name is **not** in the path), e.g. `command/<host>/quiet` `true`,
`command/<host>/mode` `C`, `command/<host>/temp` `22.5`,
`command/<host>/demand` `80`. (The combined `command/control` JSON exists but
does not apply outdoor silent on multi-split units.)

The firmware also publishes **OS/heartbeat** documents to the same `state/<host>`
topic — e.g. `{"ts":…,"id":…,"uptime":…,"rssi":-54,"mem":…}` with no AC fields.
These are frequent (the full AC document is published only on change/occasionally),
so they must be filtered out (see OS/heartbeat filtering above).

## Testing

Pure layers are table-tested against the real captured Faikin payload
(`internal/faikin`), the translation/routing (`backend_test.go`), the local read
path (`local_test.go`), the group/dependency engine (`outdoor_test.go`), the
config parser including the string/env device-map form (`config_test.go`), and
the outdoor-scoped discovery dedup (`internal/hass`).

## Limitations / future work

- Local mode still needs the cloud to **bootstrap** device structure (the
  `embeddedID` cache) and the device-registry metadata; it is not fully
  cloud-free. Local-only *settings* do get synthesized discovery, but the base
  device/entity scaffolding comes from one cloud poll.
- `demand_control` cloud-side write needs verification against live device JSON
  (the value is nested `value.modes.fixed.value`; local Faikin `{"demand":N}`
  works).
- Fan and swing now route locally too (`handleFanModeWrite` /
  `handleSwingWrite`): fan via single-char codes on `command/<host>/fan`, swing
  by combining the cloud's two axes into Faikin's single `command/<host>/swing`.
  `floorheatingairflow` has no Faikin equivalent and still uses the cloud.
- Faikin publishes its own HA discovery; running it alongside go-daikin2mqtt
  creates duplicate entities. **Do not** disable it with `ha.enable = false` —
  that flag also gates the AC state document the daemon reads (`revk_state_extra`
  returns early when off), so local mode would lose its data feed. Instead keep
  `ha.enable = true` and point Faikin's `topic.ha` at a prefix Home Assistant
  does not scan (e.g. `homeassistant_disabled`). See
  [faikin-home-assistant.md](./faikin-home-assistant.md).
