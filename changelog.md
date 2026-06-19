# Version 0.2.5 (2026-06-19)

## What's Changed

### Fixed

- Local writes now use Faikin's dedicated per-setting command topics
  (`<prefix>/<host>/command/<suffix>`, payload `1`/`0` for switches, matching
  the firmware's own HA discovery) instead of the combined
  `command/control` JSON. The JSON form did not take effect for outdoor silent
  on multi-split units — the command was delivered but the unit never changed.
  `onOffMode`→`power`, `operationMode`→`mode` (single-letter C/H/A/D/F),
  `temperatureControl`→`temp`, `powerfulMode`/`econoMode`/`streamerMode`/
  `outdoorSilentMode`→their switches, `demandControl`→`demand`.

# Version 0.2.4 (2026-06-19)

## What's Changed

### Fixed

- Local mode showed no values until a live Faikin update happened to arrive.
  `subscribeLocal` runs before the first cloud poll, so the **retained** Faikin
  state delivered at subscribe time was dropped — the `embeddedID` needed to
  route it onto the daemon's topics is only learned from a cloud poll. Since
  Faikin publishes the AC state infrequently, entities stayed empty/stale. The
  latest AC state per device is now cached and (re)published after each cloud
  poll once the `embeddedID` is known, so the retained Faikin state reaches Home
  Assistant promptly.

# Version 0.2.3 (2026-06-19)

## What's Changed

### Fixed

- Local mode now reads the AC state from the Faikin S21 status topic
  (`state/<host>/status`), which reliably carries every field (power, mode,
  setpoint, quiet, econo, streamer, demand, …) on each poll. The app-level
  `state/<host>` topic publishes the full document only occasionally and is
  otherwise flooded with OS heartbeats, so entities (e.g. outdoor silent)
  showed stale values and toggles snapped back. The status document uses S21
  field forms — `home` = room temperature, `temp` = setpoint, single-letter
  `mode` (C/H/A/D/F) — mapped by the new `ParseStatus`. Both topics are still
  subscribed for robustness across firmware variants.

# Version 0.2.2 (2026-06-19)

## What's Changed

### Fixed

- Local mode no longer resets entities to off/zero between updates. Faikin
  interleaves OS/heartbeat documents (uptime, rssi, …) on `state/<host>`
  alongside the full AC state. The daemon parsed every message, so a heartbeat
  (which carries no AC fields) published `power off`, `temp 0`, `outdoor_silent
  off`, … overwriting the real values — entities flickered and a just-set
  toggle (e.g. outdoor silent) snapped straight back. Heartbeat documents are
  now detected (no `power` field) and skipped; only real AC state is published.

# Version 0.2.1 (2026-06-19)

## What's Changed

### Fixed

- Local-only settings now appear in Home Assistant in local mode. HA discovery
  is driven by the cloud poll, so settings the ONECTA cloud does not expose for
  a unit (e.g. econo, streamer, outdoor silent, demand on the FTXA range, which
  Faikin reads off the serial bus) previously published their local state but no
  discovery config — so no entity was created. In local mode the daemon now
  synthesizes discovery points for these topics (per mapped device, skipping any
  the cloud already resolves), so `econo_mode`, `streamer`, `outdoor_silent` and
  `demand_control` show up and are fed by the Faikin read path.

# Version 0.2.0 (2026-06-19)

## What's Changed

Local-first control through the indoor units' Faikin modules, multi-split
outdoor-unit handling, and new comfort/air entities.

### Added

- **Local-first mode** (`LOCAL_MODE`): read and control the indoor units through
  their local Faikin / Faikout (revk/ESP32-Faikout) MQTT interface instead of
  the ONECTA cloud. State is sourced from `state/<host>` and commands are sent
  to `<prefix>/<host>/command/control`; the cloud still serves what Faikin does
  not expose. `LOCAL_DEVICE_MAP` maps each ONECTA device to a Faikin host and
  accepts a YAML map or an `id=host,…` string. The Faikin broker defaults to the
  main MQTT broker (shared connection) or can be separate (`LOCAL_FAIKIN_SERVER`,
  credentials falling back to the main MQTT login). Unmapped devices and
  settings Faikin does not model fall back to the cloud.
- **Outdoor-shared settings** surface as a single entity per outdoor unit with
  write fan-out to every member indoor unit: **Outdoor silent** (Außen
  Geräuscharm) and **Demand limit**. New per-indoor-unit entities **Econo mode**
  and **Streamer**.
- **Home Assistant add-on**: local-first and multi-split options (`local_mode`,
  `local_faikin_*`, `local_device_map`, `multisplit_*`, `enforce_mutual_exclusive`)
  in the add-on schema, wired through `run.sh`.

### Changed

- **Multi-split outdoor-unit constraints** (on by default, each configurable):
  a heat/cool mode change propagates to the other indoor units of the same
  outdoor unit (`MULTISPLIT_MODE_SYNC`) — a standard multi-split cannot cool and
  heat simultaneously, so conflicting units would otherwise drop to standby; the
  mutually-exclusive **Powerful** and **Econo** clear each other
  (`ENFORCE_MUTUAL_EXCLUSIVE`).
- **Gateways** now appear nested under their respective indoor unit (`via_device`)
  instead of as standalone devices. Their `unique_id`s are unchanged, so Home
  Assistant re-parents the existing devices automatically on the next discovery.

# Version 0.1.6 (2026-06-18)

## What's Changed

Cleaner, room-prefixed English Home Assistant entity IDs.

### Changed

- The MQTT discovery `default_entity_id` is now built from the device name plus
  the English topic (e.g. `sensor.galerie_room_temperature`,
  `switch.galerie_powerful_mode`) instead of the raw
  `daikin_<device-id>_<topic>` form. Entity IDs stay English and
  language-independent while keeping the human-friendly room/label prefix; the
  display **name** remains localized. German umlauts in device names are
  transliterated (ä→a, ö→o, ü→u, ß→ss) and adjacent duplicate tokens are
  collapsed.

  Note (as in 0.1.5): Home Assistant does not rename already-created entities.
  Delete the affected entities (or the device) and let them be re-discovered to
  adopt the new IDs.

# Version 0.1.5 (2026-06-18)

## What's Changed

Home Assistant entity IDs are English again, independent of the display
language.

### Fixed

- Home Assistant `entity_id`s are now always English, regardless of the
  selected display language. The MQTT discovery configs seeded the entity ID
  via the `object_id` field, which current Home Assistant no longer honors — it
  was replaced by `default_entity_id`. Without it, HA derived the entity ID
  from the device plus the (localized) entity name, producing e.g. German
  `entity_id`s under the German locale. The discovery payloads now emit
  `default_entity_id` (`<domain>.<object_id>`), so entity IDs stay English while
  the display **name** remains localized (German under the `de` locale).

  Note: Home Assistant does not rename already-created entities. To pick up the
  English IDs on an existing install, delete the Daikin device (or the affected
  entities) in HA and let them be re-discovered.

# Version 0.1.4 (2026-06-17)

## What's Changed

Smoother first-install onboarding for the Home Assistant add-on.

### Fixed

- The daemon no longer crash-loops when `CLIENT_ID`/`CLIENT_SECRET` are empty
  (a fresh add-on install starts with them unset). It now starts unconfigured —
  the web UI is reachable and the auth panel reports **"client credentials not
  configured"** — and the bridge stays idle until the credentials are entered.
  A startup warning (`daikin2mqtt.credentials_missing`) points to the fix.
- `client_id`/`client_secret` are marked optional in the add-on options schema
  (`str?`/`password?`) so the Supervisor does not require them to start.

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
