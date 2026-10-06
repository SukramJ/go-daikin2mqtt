# go-daikin2mqtt

[![Open your Home Assistant instance and add this add-on repository.](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2FSukramJ%2Fgo-daikin2mqtt)

A pure-Go bridge between the **Daikin ONECTA cloud API** and an **MQTT
broker**, with optional **Home Assistant** auto-discovery. It polls
your Daikin devices (heat pumps, air-to-air units, …) through the
official ONECTA cloud — or, in **local-first mode**, through the indoor
units' local Faikin modules — publishes their state to MQTT, and accepts
write-back commands from Home Assistant.

> **Status: beta.** The daemon works end-to-end (read + control) and has been
> validated against live ONECTA devices, including local-first control and a
> multi-split (3MXM + FTXA) setup. The characteristic catalog may still evolve.
>
> **0.14 changed every MQTT topic** to the
> [mqtt-smarthome 2.0](https://github.com/mqtt-smarthome/mqtt-smarthome/blob/master/SPEC.md)
> convention. Home Assistant users need to do nothing; scripts, flows and
> dashboards that read the raw topics need updating — see
> [MQTT topics](#mqtt-topics).

## Features

- OAuth2 (Authorization Code + PKCE) against the Daikin Developer Portal
  (ONECTA), with rotated refresh-token persistence and 401 auto-refresh.
- Rate-limit-aware, time-of-day adaptive polling (day / night intervals)
  plus a post-write "scan ignore" window to avoid stale reads.
- Bidirectional: reads device state and applies Home Assistant commands as
  ONECTA PATCHes (power, mode, setpoints, …).
- Curated characteristic catalog ([`characteristics.yaml`](./characteristics.yaml))
  mapping ONECTA data points (incl. nested `sensoryData` /
  `temperatureControl` and `consumptionData` energy) to MQTT and HA.
- Home Assistant MQTT auto-discovery for climate / sensor / binary_sensor /
  number / select / switch. **English `entity_id`s with localized (en/de)
  display names**; localized select options that map back to API codes. A
  combined **climate** entity (mode / setpoint / fan / swing / preset). Static
  device identity (model, serial, sw/firmware version, MAC) is carried in the HA
  **device** object, not as separate sensors.
- **Self-cleaning discovery**: when an entity is removed, moved to another
  device, or renamed across versions, the daemon clears its own now-stale
  retained discovery configs automatically — no manual broker cleanup. Other
  integrations' configs are never touched.
- **mqtt-smarthome 2.0 topics**: `daikin/status/…` / `daikin/set/…`, every
  value a `{"val","ts","lc"}` object with real booleans, numbers and stable
  tokens, `daikin/connected` (0/1/2), `daikin/info` and per-device `online`
  items — one grammar shared with the other `go-*2mqtt` bridges and
  openccu-loom. See [MQTT topics](#mqtt-topics).
- **Maintenance over MQTT** (on by default): change the log level, restart a
  supervised daemon, and read process stats. See [Maintenance](#maintenance).
- **`data_source` attribute** on every entity (`cloud` / `local`) so you can see
  at a glance whether a value comes from the ONECTA cloud or the local Faikin
  path.
- **Local-first mode** (optional): read **and** control the indoor units over
  their local **Faikin / Faikout** (revk/ESP32) MQTT interface instead of the
  rate-limited cloud, keeping the same HA entities — including fan speed, swing
  and the boost preset. Surfaces settings the cloud does not expose for a unit
  (econo, streamer, outdoor silent, demand) plus local-only telemetry (energy,
  power, compressor / fan frequency, refrigerant / outdoor temperature). Each
  mapped device's HA link points at its Faikin web UI. See
  [`docs/design.md`](./docs/design.md) and
  [`docs/faikin-home-assistant.md`](./docs/faikin-home-assistant.md).
- **Multi-split aware**: settings shared across one outdoor unit (operation
  mode, outdoor silent, eco, demand) are surfaced once per outdoor unit and fanned
  out to all indoor units; heat/cool mode is kept consistent across the group.
  powerful ⇄ eco are mutually exclusive (both act on the shared compressor):
  turning powerful on for any unit suspends eco group-wide and restores it when
  the boost ends (manually or after the 20-minute hardware timeout). Telemetry
  follows the physics: energy and power are **per indoor unit** and also
  **summed** once per outdoor unit; values identical across the units (compressor
  / fan frequency, refrigerant / outdoor temperature) are shown **once** at the
  outdoor unit.
- **Weekly schedules** (optional): a seven-day / 24-hour programme run by the
  daemon itself, edited in a calendar view in the web UI. Several schedules can
  target the same device and are layered by priority, so a base programme can be
  overridden by "home office" or "holiday" without editing it. Blocks set power,
  mode and setpoint at their start and the state holds until the next block, so a
  manual change in between survives. Each schedule becomes a Home Assistant
  switch, and every device gets "active block" / "next change" sensors. On a
  Faikin-mapped device the switching is entirely local — no cloud quota.
  The **outdoor unit** can be scheduled too, as its own schedule type: silent
  mode, eco and the power limit are one knob on the shared compressor, so they
  are set once per outdoor unit and fan out to every indoor unit on it.
  See [`docs/schedule-design.md`](./docs/schedule-design.md).
- Optional diagnostic **web UI** with integrated OAuth (HA-ingress ready).
- `daikin2mqtt-util` helper CLI (auth, devices, points, set, ratelimit,
  catalog-check) and a `--mock` mode using the ONECTA mock endpoint.
- Pure Go, no CGo — single static binary, distroless Docker image,
  multi-arch GHCR images, and a Home Assistant add-on.

## Quickstart

### Linux (curl | bash)

One-liner that downloads the latest release, verifies its checksum,
installs the binaries under `/opt/go-daikin2mqtt`, creates a dedicated
`daikin` service user, runs an interactive wizard for the fields with
no usable default (`CLIENT_ID`, `CLIENT_SECRET`, `MQTT_SERVER`,
`HASS_ENABLE`), and registers a hardened systemd unit:

```bash
curl -sSfL https://raw.githubusercontent.com/SukramJ/go-daikin2mqtt/main/script/install.sh | sudo bash
```

Pin a specific version:

```bash
curl -sSfL https://raw.githubusercontent.com/SukramJ/go-daikin2mqtt/main/script/install.sh | sudo bash -s -- 0.2.2
```

### Docker

```bash
docker run --rm -d \
  --name daikin2mqtt \
  -v /path/to/your/config:/config:ro \
  ghcr.io/sukramj/go-daikin2mqtt:latest
```

Start from [`config-template.yaml`](./config-template.yaml).

### Binary

```bash
make build
./bin/daikin2mqtt --config ./config.yaml
```

### Home Assistant add-on

A Home Assistant add-on is provided under [`addon/`](./addon/): it runs the
daemon inside Supervisor, exposes the diagnostic UI (incl. the OAuth
"Connect to Daikin" button) via **ingress**, reads options from the add-on
config, persists the token store under `/data`, and can use the Supervisor
MQTT service. See [`addon/README.md`](./addon/README.md).

## Diagnostic web UI

Set `WEB_ENABLE: true` (default bind `127.0.0.1:8080`) to serve a small
diagnostic UI that shows the OAuth status, offers a "Connect to Daikin"
button, browses devices / data points, sends test PATCHes, and shows the
rate-limit budget. The same server hosts the OAuth `/callback`, so no
inbound port forwarding is required.

## Helper CLI (`daikin2mqtt-util`)

```bash
daikin2mqtt-util auth                  # interactive OAuth2 flow → token store
daikin2mqtt-util devices               # list gateway devices
daikin2mqtt-util points <deviceId>     # dump a device's characteristics
daikin2mqtt-util set <dev> <emb> <characteristic> <value> [--path p]
daikin2mqtt-util ratelimit             # show the rate-limit budget
daikin2mqtt-util catalog-check         # report characteristics not in the catalog
```

Append `--mock <example-id>` to `devices` / `points` to hit the ONECTA mock
endpoint (e.g. `altherma-air-to-water-wlan`, `airpurifier`) without owning
the hardware.

## Configuration

Every field is documented in
[`config-template.yaml`](./config-template.yaml). Copy it to
`config.yaml` and fill in at least your ONECTA `CLIENT_ID` /
`CLIENT_SECRET` and the MQTT broker address.

Every config key can be overridden at runtime via a `DAIKIN_<KEY>` env
var — useful in Docker / systemd setups:

```bash
DAIKIN_MQTT_PASSWORD='change-me' ./bin/daikin2mqtt
```

Bool / int / float values are coerced; everything else stays a string.

**`MQTT_TOPIC`** (default `daikin`) is the instance name every topic starts
with. **It is the only thing that keeps two instances apart on one broker** —
nothing checks it. Two instances of this bridge (two ONECTA accounts, a
staging copy) need **different** `MQTT_TOPIC` values, or they write the same
topics and overwrite each other's `daikin/connected` and `daikin/info`. Give
each one its own `MQTT_CLIENT_ID` too. A name must not contain `+`, `#` or
start with `$`; a `/` still works but puts the instance outside the
convention (tools scanning `+/info` will not find it).

## MQTT topics

Since 0.14 every topic follows
[mqtt-smarthome 2.0](https://github.com/mqtt-smarthome/mqtt-smarthome/blob/master/SPEC.md):
`<name>/<function>/<item…>`, where `<name>` is `MQTT_TOPIC`. The function
moved from the end of the topic (`…/state`, `…/set`) to the second level.

| What | 0.13 and earlier | 0.14 |
| --- | --- | --- |
| A value | `daikin/<uuid>/<emb>/<key>/state` | `daikin/status/<uuid>/<emb>/<key>` |
| A command | `daikin/<uuid>/<emb>/<key>/set` | `daikin/set/<uuid>/<emb>/<key>` |
| Its data source | `daikin/<uuid>/<emb>/<key>/attributes` | `daikin/status/<uuid>/<emb>/<key>/attributes` |
| Climate attributes | `daikin/<uuid>/<emb>/climate/attributes` | `daikin/status/<uuid>/<emb>/climate/attributes` |
| Cloud refresh button | `daikin/<uuid>/<emb>/refresh/set` | `daikin/set/<uuid>/<emb>/refresh` |
| Schedule switch | `daikin/scheduler/<id>/enabled/state` · `…/set` | `daikin/status/scheduler/<id>/enabled` · `daikin/set/scheduler/<id>/enabled` |
| Daemon availability | `daikin/bridge/status` (`online`/`offline`) | `daikin/connected` (`0`/`1`/`2`) |
| Device reachability | — | `daikin/status/<uuid>/online` |
| Instance description | — | `daikin/info` |
| Maintenance | — | `daikin/maintenance/…` ([below](#maintenance)) |
| HA discovery | `homeassistant/device/<node id>/config` | unchanged |

`<uuid>` is the ONECTA device id and `<emb>` its management point
(`climateControl`, `gateway`, …), both verbatim. `<key>` is the catalog
`topic:` from [`characteristics.yaml`](./characteristics.yaml), or one of the
climate entity's own items (`hvac_mode`, `fan_mode`, `swing_mode`,
`swing_h_mode`, `preset_mode`). `scheduler` is a reserved first-level item
beside the device ids. The Faikin firmware's own topics (`state/<host>`,
`command/<host>/<setting>`) are dictated by the firmware and did not change.

**Values.** Every status item is a JSON object, retained, QoS 0:

```json
{"val": 21.5, "ts": 1730385720123, "lc": 1730385720000}
```

`val` is the value — a JSON boolean for switches and binary sensors (power
is `true`/`false`, no longer `on`/`off`), a JSON number for numbers, the
stable API token for enums (`cooling`, `quiet`, `windnice`, `boost`, `idle`)
rather than the localized label, and an object for the attributes items.
`ts` is when the value was observed, `lc` when it last changed, both in
milliseconds. A value is published when it changes and again after every
broker reconnect, not on every poll. Labels and units live in the Home
Assistant discovery payload only.

**Commands.** Publish to the `set` item, **not retained**, either the plain
value or `{"val": …}`. Booleans accept `true`/`false`, `1`/`0`, `on`/`off`,
`yes`/`no` in any case; enums accept the token in any case (and, as before,
the label); numbers are rounded to the device's step and clamped to its
range. Empty and retained payloads are ignored; a rejected or failed command
is logged at `warn` with its topic and payload. The daemon subscribes at
QoS 1 — publish at QoS 0 if a duplicate must never happen. `refresh` is an
action: any non-empty payload runs a cloud poll now. A command is never
echoed as a status; the status follows from the device.

```bash
mosquitto_pub -t 'daikin/set/<uuid>/climateControl/power' -m 'true'
mosquitto_pub -t 'daikin/set/<uuid>/climateControl/temperature_setpoint' -m '{"val": 21.5}'
```

**`daikin/connected`** is retained: `0` when the daemon is gone (Last Will,
and on a graceful stop), `1` when it is connected to the broker but its
upstream is unusable (two cloud polls in a row failed, the authorization
is gone or no poll has succeeded yet; in local mode, the Faikin broker
down), `2` when it is fully operational. A rate-limited poll does not lower
it: the last values stay available until the quota resets. Every Home Assistant entity is available while it is `2` and its
device's `daikin/status/<uuid>/online` is `true` (the cloud's
`isCloudConnectionUp`, or in local mode the Faikin module's report).

**`daikin/info`** is retained JSON written on every connect: `name`
(`go-daikin2mqtt`), `version`, `spec` (`2.0`), `go`, `host`, `pid`,
`started`, `maintenance`, and `mode` (`cloud` or `local`).

### Upgrading to 0.14

This is a clean break with no compatibility switch:

- **Home Assistant:** nothing to do. Every entity keeps its `unique_id`,
  entity id and device, so history, names and areas are kept; Home
  Assistant re-points them to the new topics when the daemon republishes
  its discovery documents.
- **Anything reading raw topics** — Node-RED flows, dashboards, Telegraf,
  `mosquitto_sub` scripts — must move to the new topics and read `val` out of
  the JSON object. Enum values are now tokens, power is a boolean.
- **Old retained topics** are cleared by the daemon itself: on every start of
  0.14 it reads back, for a short window, the retained 0.13 topics of the
  devices it polls, its own schedules and `daikin/bridge/status`, and clears
  exactly those. Topics of another instance on the same broker — other
  devices, another `MQTT_TOPIC` — are never touched. The same pass clears
  status items of its own devices that this release no longer publishes.

## Maintenance

On by default (mqtt-smarthome 2.0 §7):

| Topic | Effect |
| --- | --- |
| `daikin/maintenance/set/loglevel` | `error`, `warn`, `info` or `debug`; changes the daemon's log level until the next start |
| `daikin/maintenance/set/restart` | any payload: graceful shutdown (`daikin/connected` → `0`) and exit 0 — **only** where something restarts the process; otherwise refused with a `warn` line |
| `daikin/maintenance/stats` | retained process stats every `MQTT_STATS_INTERVAL` seconds: `rss`, `heapUsed`, `heapTotal`, `cpu`, `uptime`, `ts` |

| Key | Default | |
| --- | --- | --- |
| `MQTT_MAINTENANCE` | `true` | `false` disables all three topics |
| `MQTT_STATS_INTERVAL` | `60` | seconds; `0` switches the stats topic off |

A restart is accepted when `DAIKIN_SUPERVISED=1` is set, or — when the
variable is unset — under systemd, Kubernetes or in a container. A container
without a restart policy (`--restart unless-stopped`) would then simply stop:
set `DAIKIN_SUPERVISED=0` there. The Home Assistant add-on sets it to `0`,
because the Supervisor does not restart an add-on that exited cleanly.

> **Security:** anyone who may publish on the broker can restart the daemon
> or raise its log level. Secure the broker with authentication and per-client
> ACLs (the daemon needs `daikin/#` — its `MQTT_TOPIC` — and the discovery
> prefix, nothing else), or set `MQTT_MAINTENANCE: false` on a broker that
> cannot be secured.

## Home Assistant discovery

Entities are created through MQTT discovery. Since 0.12 the daemon
publishes **one retained document per Home Assistant device** —
device-based discovery — instead of one retained config per entity:

```
homeassistant/device/<node id>/config
```

`<node id>` is derived from the device's identifier and is **not** the
raw ONECTA device id. Do not compose it by hand. The daemon logs each
document's exact topic at startup, one line per device:

```
INFO coordinator.discovery_bundle_published topic=homeassistant/device/daikin_809d41d9-.../config components=16
```

Upgrading from an earlier release needs no action: the old per-entity
configs are retracted automatically before the documents are published,
and every entity keeps its `unique_id`, so its entity id, name, icon,
area and history are unaffected.

### Running more than one instance

Run **one daemon per ONECTA account per `MQTT_TOPIC`**, and give each one
its own `MQTT_TOPIC` and its own `MQTT_CLIENT_ID`. Two instances with the
same `MQTT_TOPIC` share one `daikin/connected` and one `daikin/info` and
overwrite each other's.

Two instances on **different** accounts coexist: their device ids differ,
so neither claims the other's entities. Their weekly-schedule switches
used to be the exception — those live on a topic segment and a Home
Assistant device whose names are the same in every installation — and
each instance deleted the other's from the entity registry. Since 0.12
no instance claims schedule switches, so they coexist too; the cost is
that a schedule deleted while the daemon is **stopped** leaves its
switch behind to remove by hand (deleting a schedule in the web UI, the
normal way, still removes it).

Two instances on the **same** ONECTA account see the same devices and
cannot be told apart by anything on the wire. If they are configured
differently — different `LOCAL_MODE`, a different `characteristics.yaml`
— the one with fewer entities will keep removing the other's from Home
Assistant. Use one instance, or separate `MQTT_TOPIC` values.

### Rolling back

Rolling back from 0.14 to 0.13 needs nothing beyond starting the older
release: it republishes its own topics and its discovery documents point at
them again. Clear the 0.14 tree by hand if you like (`daikin/status/#`,
`daikin/connected`, `daikin/info`, `daikin/maintenance/#`); upgrading again
clears what 0.13 left.

Home Assistant refuses a per-entity config while a device document
carrying the same `unique_id` is retained, and vice versa. So rolling
back to **0.11.x or earlier** needs one manual step first: clear the
retained device documents, once per device, using the topics from the
log lines above.

```bash
mosquitto_pub -h <broker host> -p 1883 -u <user> -P <password> \
  -t 'homeassistant/device/<node id>/config' -r -n
```

`-r -n` publishes an empty **retained** payload, which is how MQTT
clears a retained topic; without `-r` it clears nothing. Omit `-u`/`-P`
only if your broker is genuinely open — the Home Assistant add-on runs
against the Supervisor's **authenticated** broker, so add-on users need
them. Then start the older release; it republishes the per-entity
configs and the entities return, with their history, because nothing
was re-keyed.

## Documentation

- [`docs/design.md`](./docs/design.md) — local-first (Faikin) control and
  multi-split outdoor-unit handling.
- [`docs/faikin-home-assistant.md`](./docs/faikin-home-assistant.md) — running
  local mode alongside the Faikin firmware without duplicate Home Assistant
  entities (and why `ha.enable` must stay on).

## Acknowledgments

This is an independent, ground-up Go re-implementation. It was inspired by the
prior work of the Home Assistant
[**Daikin Onecta**](https://github.com/jwillemsen/daikin_onecta) integration by
**Johnny Willemsen** and contributors (licensed GPL-3.0). go-daikin2mqtt was
built directly against the official Daikin ONECTA cloud API, using the Daikin
Developer Portal (API documentation and mock endpoints) as the source of truth.
It does not incorporate that project's source code, translation files, or other
copyrightable content — only factual, non-copyrightable knowledge about the
official ONECTA cloud API informed this work. Our thanks to the daikin_onecta
authors. See [NOTICE](./NOTICE).

## Development

Parts of go-daikin2mqtt are developed with agentic AI assistance, primarily
[Claude Code](https://www.anthropic.com/claude-code). Incoming issues are
also triaged and analyzed with agentic help. Every change is still reviewed
by a human maintainer and has to pass the project's tests before it lands —
the AI accelerates the work, it does not replace the review gate.

Contributions made with AI assistance are welcome under the same terms — see
[`AI_POLICY.md`](./AI_POLICY.md) for the rules that apply.

## License

MIT — see [LICENSE](./LICENSE).
