# go-daikin2mqtt

A pure-Go bridge between the **Daikin ONECTA cloud API** and an **MQTT
broker**, with optional **Home Assistant** auto-discovery. It polls
your Daikin devices (heat pumps, air-to-air units, …) through the
official ONECTA cloud, publishes their state to MQTT, and accepts
write-back commands from Home Assistant.

> **Status: beta.** The daemon works end-to-end (read + control) and has
> been validated against live ONECTA devices. Topic layout and the
> characteristic catalog may still evolve. See [`docs/todo.md`](./docs/todo.md)
> for the roadmap and [`docs/konzept.md`](./docs/konzept.md) for the design.

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
- Home Assistant MQTT auto-discovery for sensor / binary_sensor / number /
  select / switch. **English `entity_id`s with localized (en/de) display
  names**; localized select options that map back to API codes.
- Optional diagnostic **web UI** with integrated OAuth (HA-ingress ready).
- `daikin2mqtt-util` helper CLI (auth, devices, points, set, ratelimit,
  catalog-check) and a `--mock` mode using the ONECTA mock endpoint.
- Pure Go, no CGo — single static binary, distroless Docker image,
  multi-arch GHCR images, and a Home Assistant add-on.

## Quickstart

> Quickstart commands below are placeholders until the first release is
> cut. Track progress in [`docs/todo.md`](./docs/todo.md).

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
curl -sSfL https://raw.githubusercontent.com/SukramJ/go-daikin2mqtt/main/script/install.sh | sudo bash -s -- 0.1.0
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
inbound port forwarding is required (see [`docs/konzept.md`](./docs/konzept.md) §4.1).

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

## Documentation

- [`docs/konzept.md`](./docs/konzept.md) — architecture and design concept.
- [`docs/todo.md`](./docs/todo.md) — roadmap and milestones.

## License

MIT — see [LICENSE](./LICENSE).
