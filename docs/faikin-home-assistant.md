# Faikin local mode and Home Assistant discovery

This page explains how go-daikin2mqtt's **local-first mode** coexists with the
**Faikin / Faikout** firmware on the same MQTT broker, why you may see duplicate
Home Assistant entities, and how to get a single clean set of entities **without
losing the local data feed**.

It is written for two audiences:

- **End users** — the "Recommended setup" and "Cleanup" sections are all you need.
- **Developers** — the "How it works" section documents the firmware behaviour
  the daemon depends on, with source references.

---

## The situation: two discovery sources

When local-first mode is on, **two** programs can publish Home Assistant MQTT
discovery for the same air conditioners:

| Source | Publishes discovery to | Entity names | Extras |
| --- | --- | --- | --- |
| **go-daikin2mqtt** | `homeassistant/…` | localized (e.g. German), English `entity_id`s | cloud info, multi-split outdoor aggregation, curated catalog |
| **Faikin firmware** | `homeassistant/…` (by default) | English (firmware default) | none beyond the unit itself |

Because both default to the `homeassistant/` discovery prefix, Home Assistant
picks up **both** sets and you get **duplicate entities** — one German set under
the go-daikin2mqtt devices (named after the ONECTA room, e.g. *Schlafzimmer*) and
one English set under separate Faikin devices (named after the Faikin host, e.g.
*Klima SZ*). They are genuinely separate devices — Faikin keys its device on the
ESP chip id (e.g. `1020BA304320`), go-daikin2mqtt on `daikin_<deviceID>` — so
Home Assistant never merges them; they simply sit side by side.

---

## The catch: you cannot just disable Faikin's HA discovery

The obvious fix — set `ha.enable` to `false` on the Faikin module — **does not
work**, because in the firmware the same flag gates **both** the discovery
configs **and the AC state document go-daikin2mqtt reads.**

In `Faikout.c`, `revk_state_extra()` (the function that writes `power`, `mode`,
`temp`, `target`, `quiet`, `econo`, `energy`, `consumption`, … into the
`state/<host>` document) begins:

```c
void revk_state_extra (jo_t j) {
   if (!haenable)
      return;          // ha disabled → no AC fields are published at all
   ...
}
```

So with `ha:false` the firmware publishes only its base OS/heartbeat fields
(uptime, rssi, …) to `state/<host>` — **none of the AC fields**. go-daikin2mqtt
detects an AC document by the presence of `power` (`State.HasAC`); without it the
message is ignored, and **local mode has no data**.

**Therefore: `ha.enable` must stay `true` for local-first mode to work.**

---

## Recommended setup: go-daikin2mqtt owns the entities

Keep Faikin's HA discovery **enabled** (so the state feed keeps flowing) but send
its discovery configs to a prefix Home Assistant does **not** watch. Faikin has a
separate setting for the discovery prefix, independent of `ha.enable`:

| Faikin setting | Default | Set to |
| --- | --- | --- |
| `ha.enable` | `true` | **`true`** (leave on — it feeds the data) |
| `topic.ha` | `homeassistant` | **`homeassistant_disabled`** (any prefix HA doesn't scan) |

With `topic.ha = homeassistant_disabled`:

- `state/<host>` keeps carrying the full AC document (`ha.enable` still true) →
  **go-daikin2mqtt reads everything as before.**
- Faikin's discovery configs go to `homeassistant_disabled/…`, which Home
  Assistant's default discovery prefix (`homeassistant`) ignores → **no duplicate
  entities.**
- go-daikin2mqtt remains the single entity owner: localized names, stable English
  `entity_id`s, cloud-enriched device info, and multi-split outdoor aggregation.

### How to set it

On the Faikin web UI, open the **MQTT** settings and set the **HA topic**
(`topic.ha`) field to `homeassistant_disabled`, then save/restart.

Equivalently over MQTT, publish to the module's settings topic:

```bash
mosquitto_pub -h <broker> -u <user> -P <pass> \
  -t 'setting/<faikin-host>' -m '{"topicha":"homeassistant_disabled"}'
```

(`setting/<host>` is Faikin's settings topic; `state/<host>` and
`command/<host>/<suffix>` are unaffected.)

### Cleanup: remove the stale retained configs

Changing `topic.ha` does **not** delete the discovery configs the firmware
already published to the old `homeassistant/…` prefix — those are **retained**
and Home Assistant keeps showing them until they are cleared. Two options:

1. **From Home Assistant** — *Settings → Devices → the Faikin device → delete*.
   Repeat per Faikin device.
2. **From the broker** — clear each retained config by publishing an empty
   retained message to its topic. List them first:

   ```bash
   mosquitto_sub -h <broker> -u <user> -P <pass> -v -W 3 \
     -t 'homeassistant/#' | grep -i revk        # find the Faikin config topics
   ```

   then for each `homeassistant/<type>/<faikin-host><tag>/config` topic:

   ```bash
   mosquitto_pub -h <broker> -u <user> -P <pass> -r -n \
     -t 'homeassistant/<type>/<faikin-host><tag>/config'
   ```

---

## Alternative: let Faikin own the entities

If you would rather use Faikin's native entities, leave `topic.ha =
homeassistant` and instead turn **go-daikin2mqtt**'s discovery off
(`HASS_ENABLE: false`). You then lose go-daikin2mqtt's cloud-enriched device
info, localized names, and multi-split outdoor aggregation, so this is only
sensible if you do not run local mode for those features.

---

## How it works (developer reference)

The daemon's local read path subscribes to Faikin's canonical state topic and
republishes onto its own per-unit topics, so Home Assistant sees identical
entities whichever backend is active. The relevant firmware facts:

- **State topic** — `state/<host>`, retained, app form (`mode` is a word like
  `cool`, `temp` is the room temperature, `target` is the setpoint). This is the
  exact topic Faikin's own HA discovery points every entity at (`stat_t`).
- **`ha.enable` couples discovery and state.** Both `send_ha_config()`
  (`Faikout.c`, `if (!haenable) return;`) and `revk_state_extra()` (same guard)
  are gated by it. There is no firmware flag to publish state *without*
  discovery; the discovery **location** is decoupled only via `topic.ha`.
- **Heartbeat filtering.** Faikin interleaves OS/heartbeat documents (no AC
  fields) on `state/<host>`; `faikin.ParseState` sets `HasAC` from the presence
  of `power` and the read path skips the rest, so entities are never reset to
  zero. See `internal/faikin/faikin.go` and `internal/coordinator/local.go`.
- **Multi-split telemetry is outdoor-scoped.** On a multi-split only the
  **active** indoor unit reports outdoor-unit values (`consumption`, `comp`,
  `energy`/`energyheat`/`energycool`) and the outdoor-shared settings
  (`quiet`, `demand`) over the S21 bus; idle units omit those fields. Treat them
  as the outdoor unit's value (the reporting member), never as a per-unit sum.

This dependency is why the recommended setup keeps `ha.enable` on: it is not just
about Home Assistant entities, it is the daemon's local data source.
