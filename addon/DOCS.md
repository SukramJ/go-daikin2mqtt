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

Fixed by the add-on (not user-configurable): the token store lives at
`/data/token-store.json` and the web UI binds to `0.0.0.0:8080` for Ingress.
The OAuth callback is served on that same port; the externally registered
address is the `redirect_uri` option above.
