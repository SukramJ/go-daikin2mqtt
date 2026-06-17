# Version 0.1.3 (2026-06-17)

## What's Changed

OAuth login fixes for the Home Assistant ingress iframe.

### Fixed

- The "Connect to Daikin" button now opens the OAuth flow in the **top-level
  window** (`target="_top"`). Behind HA ingress the UI runs in an iframe, but
  the Daikin IdP login sets `X-Frame-Options: sameorigin` and refuses to be
  framed, so the login previously failed to render.

### Added

- The authentication panel now shows the **effective redirect URI** (derived
  from the request behind ingress) so you can register exactly that value with
  the Daikin portal — it is served via `/api/auth/status` and surfaced in the UI.

# Version 0.1.2 (2026-06-17)

## What's Changed

Makes the Home Assistant add-on actually installable from the store.

### Fixed

- Publish a **dedicated add-on image** (HA base + bashio + `run.sh`) per
  architecture to `ghcr.io/sukramj/go-daikin2mqtt-addon-{arch}` via the new
  `addon-image.yml` workflow, and point the manifest at it. The previous
  `image:` referenced the distroless daemon image (`ghcr.io/sukramj/go-daikin2mqtt`),
  which runs the binary directly and has **no `run.sh`**, so the add-on options
  were never translated into `DAIKIN_*` env and the add-on aborted at boot.

# Version 0.1.1 (2026-06-17)

## What's Changed

Home Assistant add-on completion plus OAuth and first-boot fixes on top of 0.1.0.

### Added

- German labels for the combined climate entity's fan / swing / preset
  dropdowns (mirroring `daikin_onecta`), reverse-mapped to the raw Daikin
  values on write.
- Home Assistant add-on manifest (`addon/config.yaml`) and repository
  descriptor (`repository.yaml`) so the add-on is installable from the repo.
- Zero-config MQTT: an empty `mqtt_server` auto-uses the Supervisor's MQTT
  service (host / port / credentials), like zigbee2mqtt.
- Automatic OAuth `redirect_uri` derivation from the request when unset —
  behind Ingress this yields the external HTTPS ingress URL (logged for portal
  registration), so no separate reverse-proxy rule is needed.
- Optional host-port exposure (`ports`, disabled by default) for the
  explicit reverse-proxy redirect route, plus a configurable `redirect_uri`
  option and an add-on Quickstart.

### Fixed

- Daemon no longer requires a config file: it boots from `DAIKIN_*`
  environment variables alone (the add-on ships no `config.yaml`).
- Add-on container resolves `characteristics.yaml` (runs from `WORKDIR /app`).
- `.gitignore` no longer excludes `addon/config.yaml` (the runtime-config rule
  is anchored to the repo root).
- Cross-platform (Windows) test assumptions for the token-store path and
  file permissions.

# Version 0.1.0 (2026-06-16)

## What's Changed

First functional release of go-daikin2mqtt — a pure-Go bridge between the
Daikin ONECTA cloud API and MQTT, validated end-to-end against live devices.

### Added

- ONECTA OAuth2 client (Authorization Code + PKCE) with rotated
  refresh-token persistence, proactive refresh, and 401 auto-refresh.
- Cloud client with global cloud lock, rate-limit accounting, GET retries
  with backoff, circuit breaker, and a post-write scan-ignore window.
- Device model parser and a curated characteristic catalog
  (`characteristics.yaml`) with nested value resolution (`sensoryData`,
  mode-scoped `temperatureControl` setpoints) and `consumptionData` energy.
- Coordinator: adaptive day/night polling, MQTT state publishing, bridge
  LWT, and a write path (MQTT `/set` → ONECTA PATCH).
- Home Assistant MQTT discovery (sensor / binary_sensor / number / select /
  switch) with English `entity_id`s and localized (en/de) display names;
  localized select options that map back to API codes.
- Optional diagnostic web UI with integrated OAuth (HA-ingress ready).
- `daikin2mqtt-util` CLI: auth, devices, points, set, ratelimit,
  catalog-check, plus a `--mock` mode using the ONECTA mock endpoint.
- Device coverage: climateControl (air-to-air & Altherma leaving-water),
  domesticHotWaterTank, air purifier, and hardware info points.
- Home Assistant add-on (`addon/`), multi-arch GHCR images, curl|bash
  installer with a hardened systemd unit, and build/release/CI workflows.
