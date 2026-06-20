# go-daikin2mqtt add-on

## Quickstart

For a standard Home Assistant install with the Mosquitto broker, only two
options are required:

1. Create an application in the
   [Daikin Developer Portal](https://developer.cloud.daikineurope.com) and copy
   its **Client ID** and **Client secret** into `client_id` / `client_secret`.
2. Leave **`mqtt_server` empty** — the add-on auto-connects to the Home
   Assistant MQTT broker (like zigbee2mqtt), and `hass_enable` is on by
   default, so devices appear automatically via MQTT discovery.
3. **Start** the add-on, open its **Web UI**, and click **Connect to Daikin**
   for the one-time OAuth login.

> **Redirect URI:** Daikin requires an **HTTPS** redirect URI registered in the
> portal. Behind Ingress the default `http://localhost:8080/callback` is not
> reachable, so set the `redirect_uri` option to an HTTPS URL that forwards to
> the add-on's `:8080` (reverse proxy or tunnel) — see the add-on README.

Everything else has sensible defaults; use the reference below to fine-tune.

## Options reference

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `client_id` | str | `""` | Daikin ONECTA OAuth2 client ID (Daikin Developer Portal). Required for cloud access, but the add-on still starts without it (UI shows "not configured") so you can configure it after install. |
| `client_secret` | password | `""` | Daikin ONECTA OAuth2 client secret. Same as above — required to connect, optional to start. |
| `redirect_uri` | str | `""` | OAuth2 redirect URI registered for your client in the Daikin portal. **Leave empty** to auto-derive it from the request — behind Ingress this becomes the add-on's external HTTPS ingress URL, logged at login time (`web.oauth_redirect_uri`) so you can register that exact value. Set a value only to override, e.g. a dedicated HTTPS reverse-proxy or tunnel URL. |
| `mqtt_server` | str | `""` | MQTT broker hostname. **Leave empty** to auto-use the Home Assistant MQTT broker (the configured MQTT integration / `core-mosquitto`). Set a value only to point at a different broker. |
| `mqtt_port` | int | `1883` | MQTT broker port. Only used when `mqtt_server` is set; the auto-detected broker brings its own port. |
| `mqtt_login` | str | `""` | MQTT username. Only used when `mqtt_server` is set (auto-detect supplies credentials). |
| `mqtt_password` | password | `""` | MQTT password. Only used when `mqtt_server` is set. |
| `mqtt_topic` | str | `daikin` | Base MQTT topic for published device state. |
| `hass_enable` | bool | `true` | Publish Home Assistant MQTT discovery so devices and entities appear automatically. On by default — leave enabled for the normal HA experience; disable only to manage entities manually. |
| `language` | list(en\|de) | `en` | UI / entity naming language. |
| `web_enable` | bool | `true` | Enable the diagnostic web UI / OAuth flow (required for Ingress login). |
| `refresh_day_interval` | int | `600` | Seconds between cloud polls during day hours. |
| `refresh_night_interval` | int | `1800` | Seconds between cloud polls during night hours. |
| `local_mode` | bool | `false` | Read and control the indoor units over their local **Faikin / Faikout** MQTT interface instead of the ONECTA cloud. Requires `local_device_map`. See "Local-first mode" below. |
| `local_faikin_server` | str | `""` | MQTT broker the Faikin modules publish to. **Leave empty** to use the same broker as the daemon (the common case — the existing connection is reused). |
| `local_faikin_port` | int | `1883` | Faikin broker port. Only used when `local_faikin_server` is set. |
| `local_faikin_login` | str | `""` | Faikin broker username. Empty → falls back to the main MQTT username. |
| `local_faikin_password` | password | `""` | Faikin broker password. Empty → falls back to the main MQTT password. |
| `local_faikin_prefix` | str | `Faikout` | **Deprecated and ignored.** The Faikin command topic is always `command/<host>/<setting>` (the firmware convention). |
| `local_device_map` | list(str) | `[]` | Maps each ONECTA device to its Faikin host as `deviceID=Faikin host` entries (e.g. `cfcbab3e-…=Klima GA`). Only mapped devices are driven locally. See below. |
| `multisplit_mode_sync` | bool | `true` | Propagate a heat/cool change to the other indoor units of the same outdoor unit (a standard multi-split cannot cool and heat at once). |
| `multisplit_outdoor_aggregate` | bool | `true` | Surface outdoor-shared settings (outdoor silent, demand) as one entity per outdoor unit, fanning writes out to every member unit. |
| `enforce_mutual_exclusive` | bool | `true` | Turning powerful on clears econo, and vice versa. |

Fixed by the add-on (not user-configurable): the token store lives at
`/data/token-store.json` and the web UI binds to `0.0.0.0:8080` for Ingress.
The OAuth callback is served on that same port; the externally registered
address is the `redirect_uri` option above.

## Local-first mode (optional)

If your indoor units have local **Faikin / Faikout** (revk/ESP32) modules, the
add-on can read and control them over their local MQTT interface instead of the
rate-limited ONECTA cloud. This also surfaces settings the cloud does not expose
for some units (econo, streamer, outdoor silent, demand).

Enable it with:

- `local_mode: true`
- `local_faikin_server`: leave **empty** if the Faikin modules publish to the
  same broker as the add-on (the common case); set it only for a separate broker
  (then also set `local_faikin_login` / `local_faikin_password` if it differs).
- `local_device_map`: one `deviceID=Faikin host` entry per unit, e.g.

  ```yaml
  local_device_map:
    - "cfcbab3e-7e55-42b4-9af7-21bb1feb2bbd=Klima GA"
    - "1921496f-5316-4555-b79a-2d3c96da202f=Klima SZ"
  ```

  Find each **device ID** in the diagnostic web UI's device browser; the
  **Faikin host** is the module's hostname (its `state/<host>` topic).

The cloud connection is still used to bootstrap the device list and Home
Assistant discovery and for anything Faikin does not expose. The `multisplit_*`
options govern settings physically shared across one outdoor unit (heat/cool
mode consistency, outdoor silent / demand as one entity with write fan-out);
leave them on for a multi-split.

> **Note:** on a multi-split, an outdoor-unit setting (outdoor silent, demand)
> is applied by the **active** indoor unit; the add-on fans the command out to
> all units and shows the aggregated state, so the single entity reflects the
> whole outdoor unit.

### Avoiding duplicate entities (Faikin's own HA discovery)

The Faikin firmware can publish its **own** Home Assistant discovery, which then
duplicates the add-on's entities (an English set under separate *Klima …*
devices next to the add-on's localized set). Do **not** turn Faikin's HA off to
stop this — the same firmware flag also stops the AC data the add-on reads, so
local mode would go blank.

Instead, on each Faikin module keep HA **enabled** but change its discovery
topic prefix (`topic.ha`) from `homeassistant` to a prefix Home Assistant does
not scan, e.g. **`homeassistant_disabled`**. The state feed keeps working and the
duplicate entities disappear. Then delete the leftover Faikin devices in Home
Assistant (their old retained configs persist until removed). Full walkthrough:
[docs/faikin-home-assistant.md](https://github.com/SukramJ/go-daikin2mqtt/blob/main/docs/faikin-home-assistant.md).
