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
