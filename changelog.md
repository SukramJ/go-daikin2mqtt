# Version 0.2.20 (2026-06-20)

## What's Changed

### Added

- **Self-cleaning Home Assistant discovery.** The daemon now clears its own
  retained discovery configs that it no longer publishes — entities removed,
  moved to another device, or renamed across versions. After publishing
  discovery it collects the retained configs under the HA discovery prefix and
  clears the ones it owns (`unique_id` in the `daikin_…` namespace with a state
  topic under the configured root) that are not in the current set. Other
  integrations' configs are never touched. This removes the previously-needed
  manual broker cleanup after entity changes — including the 0.2.19 telemetry
  rework — so stale, unavailable entities disappear on their own.

# Version 0.2.19 (2026-06-20)

## What's Changed

### Changed

- Reworked the placement of the local telemetry to follow the physics (verified
  on a live multi-split):
  - **Per indoor unit** (each unit's own value): `energy_total`,
    `heating_energy_total`, `cooling_energy_total`, `power_consumption`.
  - **System total per outdoor unit** (the SUM across the indoor units): new
    `outdoor_energy_total`, `outdoor_heating_energy_total`,
    `outdoor_cooling_energy_total`, `outdoor_power`.
  - **Shared outdoor values** (identical on every indoor unit) shown **once** per
    outdoor unit: `compressor_frequency`, `fan_frequency`,
    `refrigerant_temperature`, and now also `outdoor_temperature`.

  **Migration:** the previous outdoor-aggregated `energy_total` /
  `power_consumption` / … entities and the duplicated per-indoor
  `outdoor_temperature` / `compressor_frequency` / `fan_frequency` /
  `refrigerant_temperature` entities change device/scope. Stale ones go
  unavailable in Home Assistant — delete them; the new layout appears
  automatically.

# Version 0.2.18 (2026-06-20)

## What's Changed

### Added

- Every entity now exposes a **`data_source`** attribute (`cloud` or `local`) so
  you can see, per entity, whether its value comes from the ONECTA cloud or the
  local Faikin interface. Published as retained JSON attributes alongside
  discovery (`json_attributes_topic`).

### Changed

- Removed `gateway_firmware_version` and `gateway_mac_address` sensors — both are
  already in the Home Assistant **device** info (`sw_version` / `connections`).
  `gateway_ip_address` and `gateway_software_version` stay (not in device info).

  **Migration:** the removed entities go unavailable in Home Assistant after the
  update — delete them.

# Version 0.2.17 (2026-06-20)

## What's Changed

### Fixed

- In local mode the climate **preset** (boost) now stays in sync with the
  powerful state. It was published from the (stale) cloud value, so toggling
  powerful — via the switch or the preset itself — left the preset showing
  `none`, making boost impossible to turn off from the climate card. The preset
  is now fed from the Faikin `powerful` state like the powerful switch, and the
  cloud poll no longer overwrites it for locally-controlled units.

### Changed

- Model, serial number and (hydro) software version are kept in the Home
  Assistant **device** info, not as separate sensors. Removed
  `indoor_unit_model`, `indoor_unit_serial_number`, `outdoor_unit_model`,
  `outdoor_unit_serial_number`, `gateway_serial_number`, `indoor_hydro_model`,
  `indoor_hydro_software_version` — the same information is on the device page.

  **Migration:** the removed entities go unavailable in Home Assistant after the
  update — delete them.

# Version 0.2.16 (2026-06-20)

## What's Changed

### Added

- In local mode, each mapped device's Home Assistant **device link**
  (`configuration_url`) now points at its **Faikin module web UI**
  (`http://<ip>/`, from the module's reported `ipv4`/`ipv6`), mirroring Faikin's
  own discovery — so the device page opens the local module instead of the
  ONECTA cloud. Cloud-only devices keep the ONECTA link.

# Version 0.2.15 (2026-06-20)

## What's Changed

### Changed

- Model and software/firmware versions are kept in the Home Assistant **device**
  info (the device's `model` / `sw_version` / `serial_number`, already populated
  from the cloud data) rather than duplicated as separate sensors. The redundant
  entities added in 0.2.14 are removed: `gateway_model`,
  `indoor_unit_software_version`, `outdoor_unit_software_version`. Gateway SSID,
  time zone, and firmware-update-supported are no longer surfaced
  (`gateway_ssid`, `gateway_timezone`, `gateway_firmware_update_supported`
  removed). The status-flag diagnostics from 0.2.14 stay: `caution_state`,
  `mode_conflict`, `holiday_mode`, and the outdoor unit `outdoor_error_code` /
  `outdoor_error_state` / `outdoor_warning_state` / `outdoor_caution_state`.

  **Migration:** the removed entities go unavailable in Home Assistant after the
  update — delete them; the same information is shown on the device page.

# Version 0.2.14 (2026-06-20)

## What's Changed

### Added

- Catalogued more ONECTA characteristics as **diagnostic** entities (previously
  unmapped), surfaced on the matching sub-device:
  - Gateway: `gateway_model`, `gateway_ssid`, `gateway_timezone`,
    `gateway_firmware_update_supported`.
  - Indoor unit: `indoor_unit_software_version`.
  - Outdoor unit: `outdoor_unit_software_version`, `outdoor_error_code`,
    `outdoor_error_state`, `outdoor_warning_state`, `outdoor_caution_state`.
  - Climate: `caution_state`, `mode_conflict` (useful on a multi-split),
    `holiday_mode`.

# Version 0.2.13 (2026-06-20)

## What's Changed

### Added

- Safeguard for the summed outdoor energy totals: if every reporting indoor unit
  shows the **same** energy counter, it is treated as a single shared meter and
  published as-is instead of being summed (which would multiply it by the unit
  count). Per-unit meters (different values) keep summing to the system total.
  This protects hardware where Faikin's `energy` (the outside power meter) is a
  shared outdoor counter rather than per indoor unit. Confirmed against the
  firmware: the energy fields are decoded from S21 device responses, and
  `energyheat`/`energycool` are the per-device command `'U'`.

# Version 0.2.12 (2026-06-20)

## What's Changed

### Fixed

- Corrected the outdoor-unit telemetry aggregation introduced in 0.2.11. Live
  multi-split data showed that **power and energy are per indoor unit** (each unit
  reports its own), not a single shared outdoor value — only the compressor
  frequency is shared. The 0.2.11 *max* aggregation therefore showed just the
  single highest unit, which is meaningless. Now:
  - `power_consumption`, `energy_total`, `heating_energy_total`,
    `cooling_energy_total` are **summed** across the group = the system total.
    Energy is **held** per unit at its highest seen value, so an idle unit that
    stops reporting (reads 0) does not drop the summed `total_increasing` total.
  - `compressor_frequency` stays the shared outdoor value (max; identical across
    members).

# Version 0.2.11 (2026-06-20)

## What's Changed

### Changed

- The outdoor-unit telemetry added in 0.2.10 — **power draw**, **compressor
  frequency** and the **lifetime energy totals** — now appears as **one sensor
  per outdoor unit** instead of one per indoor unit. On a multi-split only the
  active indoor unit reports these over the S21 bus, so the per-unit sensors were
  misleading (idle units showed 0). They are aggregated as the max across the
  outdoor group (= the reporting unit); energy totals are never republished as 0,
  so the `total_increasing` counter is not reset when no unit is currently
  reporting. `fan_frequency` and `refrigerant_temperature` remain per indoor
  unit.

  **Migration:** after updating, the old per-indoor-unit `power_consumption` /
  `energy_total` / `heating_energy_total` / `cooling_energy_total` /
  `compressor_frequency` entities become unavailable (their discovery scheme
  changed) — delete them in Home Assistant; the new outdoor-unit sensors appear
  automatically.

### Documentation

- New [docs/faikin-home-assistant.md](docs/faikin-home-assistant.md): how to run
  local mode alongside the Faikin firmware **without duplicate Home Assistant
  entities**. Key point — Faikin's `ha.enable` gates **both** its own HA
  discovery **and** the AC fields the daemon reads from `state/<host>`, so it
  must stay enabled; avoid duplicates by pointing Faikin's `topic.ha` at a prefix
  Home Assistant does not scan (e.g. `homeassistant_disabled`), not by disabling
  HA. (Corrects the earlier `{"ha":false}` guidance.)

# Version 0.2.10 (2026-06-20)

## What's Changed

### Added

- Local-only telemetry sensors, read straight from the Faikin state document
  (the cloud does not expose these per indoor unit):
  - **Energy totals** — `energy_total`, `heating_energy_total`,
    `cooling_energy_total` (lifetime kWh, `total_increasing`). These complement
    the cloud's daily/weekly/monthly buckets and keep working without the cloud.
  - **Power** — `power_consumption` (current draw, W).
  - **Diagnostics** — `compressor_frequency`, `fan_frequency`,
    `refrigerant_temperature` (entity category `diagnostic`).

  They appear only for devices mapped to a Faikin host. The live climate sensors
  (room/outdoor temperature, humidity, setpoint) were already read locally; what
  still comes from the cloud is static device info (model/serial/firmware) and
  diagnostics (error/warning states).

# Version 0.2.9 (2026-06-19)

## What's Changed

### Added

- Local-first now also covers the climate **fan speed** and **swing** controls.
  Previously `fan_mode` and `swing_mode`/`swing_h_mode` always fell back to the
  cloud (rate-limited, slower); they now read from and write to the local Faikin
  interface for mapped devices, like the rest of the climate entity. The cloud
  and Faikin vocabularies are mapped explicitly: fan via single-character codes
  (`A`/`Q`/`1`..`5`, robust to 3- vs 5-speed units); swing combines the cloud's
  two axes (vertical/horizontal) into Faikin's single `command/<host>/swing`,
  including comfort airflow (`C`). `floorheatingairflow` has no Faikin equivalent
  and still uses the cloud.

# Version 0.2.8 (2026-06-19)

## What's Changed

This release aligns the local Faikin read and write paths with the firmware's
actual MQTT interface (verified against the revk/ESP32-Faikout source and the
firmware's own Home Assistant discovery), replacing earlier guesses.

### Fixed

- **Writes** now reach Faikin: the command topic is `command/<host>/<suffix>`
  (e.g. `command/Klima WZ/quiet`) — the firmware convention, with no app name in
  the path (just like the `state/<host>` topic), matching Faikin's own HA
  discovery. The previous `<prefix>/<host>/command/<suffix>` form (with the
  "Faikout" prefix) was never received by the firmware, so toggles had no effect.
  Switch payloads are now `true`/`false` (the firmware also accepts `1`/`on`).
- **Reads** now use the firmware's canonical state topic `state/<host>` (the app
  document every entity in Faikin's own HA discovery reads from: `mode` as a
  word, `temp` = room temperature, `target` = setpoint) instead of the
  non-standard `state/<host>/status` (S21) topic. `state/<host>` is retained and
  published on change; OS/heartbeat documents lacking `power` are still ignored.
- `LOCAL_FAIKIN_PREFIX` / `local_faikin_prefix` is **deprecated and ignored**
  (the command topic is fixed); existing configs keep loading.

# Version 0.2.7 (2026-06-19)

## What's Changed

### Fixed

- Toggling an outdoor-shared setting (outdoor silent, demand) no longer snaps
  back. The optimistic value published on write was immediately overwritten by
  the group aggregate computed from a still-stale Faikin status (the active
  indoor unit reports the change only on its next, sparse status). The
  just-written value is now **held** until a status confirms it (or a 2-minute
  timeout), so Home Assistant shows the change immediately and stably.

# Version 0.2.6 (2026-06-19)

## What's Changed

### Fixed

- The single outdoor-shared entities (outdoor silent, demand limit) now reflect
  the whole outdoor unit instead of one fixed indoor unit. On a multi-split only
  the **active** indoor unit applies an outdoor-unit setting and reports it back,
  so a toggle changed one unit while the entity (reading another) appeared not to
  react. The state is now **aggregated across the outdoor group** (silent on if
  any member reports it; demand the most restrictive) and published to every
  member, and a write is reflected **optimistically** on all members at once for
  immediate Home Assistant feedback. The command was already fanned out to all
  units (it must be uniform across the outdoor unit).

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
