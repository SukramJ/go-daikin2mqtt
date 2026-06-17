# Options reference

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `client_id` | str | `""` | Daikin ONECTA OAuth2 client ID (Daikin Developer Portal). |
| `client_secret` | password | `""` | Daikin ONECTA OAuth2 client secret. |
| `redirect_uri` | str | `""` | OAuth2 redirect URI registered for your client in the Daikin portal. Empty → `http://localhost:8080/callback`, which only works for a browser on the same host. Daikin requires **HTTPS**, so behind Ingress set an HTTPS URL that forwards to the add-on's `:8080` (reverse-proxy or tunnel). |
| `mqtt_server` | str | `""` | MQTT broker hostname. **Leave empty** to auto-use the Home Assistant MQTT broker (the configured MQTT integration / `core-mosquitto`). Set a value only to point at a different broker. |
| `mqtt_port` | int | `1883` | MQTT broker port. Only used when `mqtt_server` is set; the auto-detected broker brings its own port. |
| `mqtt_login` | str | `""` | MQTT username. Only used when `mqtt_server` is set (auto-detect supplies credentials). |
| `mqtt_password` | password | `""` | MQTT password. Only used when `mqtt_server` is set. |
| `mqtt_topic` | str | `daikin` | Base MQTT topic for published device state. |
| `hass_enable` | bool | `true` | Publish Home Assistant MQTT discovery messages. |
| `language` | list(en\|de) | `en` | UI / entity naming language. |
| `web_enable` | bool | `true` | Enable the diagnostic web UI / OAuth flow (required for Ingress login). |
| `refresh_day_interval` | int | `600` | Seconds between cloud polls during day hours. |
| `refresh_night_interval` | int | `1800` | Seconds between cloud polls during night hours. |

Fixed by the add-on (not user-configurable): the token store lives at
`/data/token-store.json` and the web UI binds to `0.0.0.0:8080` for Ingress.
The OAuth callback is served on that same port; the externally registered
address is the `redirect_uri` option above.
