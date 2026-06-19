# Design: local-first control & multi-split outdoor-unit handling

This document describes the architecture added in 0.2.0: a second control
backend that drives the indoor units through their **local Faikin interface**,
and the handling of settings that ONECTA exposes per indoor unit but that are
physically shared across one **multi-split outdoor unit**.

It complements the package-level doc comments; read those for exact signatures.

## Goals

1. **Local-first control** — when enabled, read and write the indoor units over
   their local Faikin / Faikout MQTT interface instead of the rate-limited
   ONECTA cloud, while keeping the same Home Assistant entities and MQTT topics.
2. **Correct multi-split behaviour** — settings that are one physical knob on
   the shared outdoor unit (operation mode, outdoor silent, demand limit) must
   stay consistent across all indoor units; mutually-exclusive settings
   (powerful ⇄ econo) must not be set together.
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
  fan/noise; **demand control** limits its power draw. Both are exposed per
  indoor unit in ONECTA but act on the one outdoor unit.

Genuinely per-indoor-unit settings (independent): setpoint, fan, swing,
powerful, econo, streamer, on/off.

## Control backend seam

`internal/coordinator/backend.go` routes every write through one entry point:

```
setCharacteristic(deviceID, embeddedID, characteristic, value, path)
  ├─ device mapped to a Faikin host AND characteristic locally controllable?
  │     → publish Faikout/<host>/command/control  {translated JSON}   (local)
  └─ otherwise                                     → client.Patch(...)  (cloud)
```

`faikinControlFor` translates an ONECTA characteristic write into a partial
Faikin `Control` (e.g. `operationMode "cooling"` → `{"mode":"cool"}`). Anything
Faikin does not model returns `ok=false` and **falls back to the cloud**, so
local mode degrades gracefully rather than dropping a command. Unmapped devices
always use the cloud, even in local mode.

## Local-first path

`internal/faikin` is a pure translation layer (no I/O) over the revk
Faikin/Faikout firmware:

- **State** — a retained JSON document on `state/<host>` (see the reference
  below). `ParseState` decodes it; `State.HAMode()` maps `power`+`mode` to an HA
  `hvac_mode`.
- **Command** — a partial JSON `Control` published to
  `<prefix>/<host>/command/control` (`prefix` = the firmware "app" name, e.g.
  `Faikout`). Only the set fields are emitted, so a command never disturbs other
  settings.

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
| **Mode sync** | heat/cool change → propagate `operationMode` to the other group members | `MULTISPLIT_MODE_SYNC` |
| **Outdoor fan-out** | write to a `scope: outdoor` setting → apply to every group member | `MULTISPLIT_OUTDOOR_AGGREGATE` |
| **Mutual exclusion** | powerful/econo on → clear the partner | `ENFORCE_MUTUAL_EXCLUSIVE` |

These run **above** `setCharacteristic`, so they fan out through whichever
backend each member uses.

### `scope: outdoor` in the catalog & discovery

A catalog entry can carry `scope: outdoor` (`internal/catalog`). On the write
side it triggers fan-out; on the discovery side
(`internal/hass/discovery.go`), such points are keyed by the **outdoor serial**
and attached to the outdoor device, so all indoor units' copies deduplicate to a
**single entity per outdoor unit** (`outdoor_silent`, `demand_control`).

## Catalog additions (0.2.0)

Per-unit switches `econo_mode`, `streamer`; outdoor-shared `outdoor_silent`
(switch) and `demand_control` (number). Faikin exposes all of them locally;
`demand_control`'s cloud-side nested PATCH is best-effort and should be verified
against a live `daikin2mqtt-util points <id>` dump.

## Configuration reference

```yaml
LOCAL_MODE: false                  # master switch for local-first
LOCAL_FAIKIN_SERVER: ""            # empty -> same broker as MQTT_SERVER
LOCAL_FAIKIN_PORT: 1883
LOCAL_FAIKIN_LOGIN: ""             # empty -> MQTT_LOGIN
LOCAL_FAIKIN_PASSWORD: ""          # empty -> MQTT_PASSWORD
LOCAL_FAIKIN_PREFIX: Faikout       # firmware app name (command-topic prefix)
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

Command topic `<prefix>/<host>/command/control` takes a partial of the same
keys, e.g. `{"power":true,"mode":"cool","temp":22.5,"quiet":true}`.

## Testing

Pure layers are table-tested against the real captured Faikin payload
(`internal/faikin`), the translation/routing (`backend_test.go`), the local read
path (`local_test.go`), the group/dependency engine (`outdoor_test.go`), the
config parser including the string/env device-map form (`config_test.go`), and
the outdoor-scoped discovery dedup (`internal/hass`).

## Limitations / future work

- `demand_control` cloud-side write needs verification against live device JSON.
- Local mode bootstraps device structure + HA discovery from one cloud poll; a
  fully cloud-free local mode (local-driven discovery) is out of scope here.
- Fan/swing writes still route to the cloud (not yet modelled in
  `faikinControlFor`).
