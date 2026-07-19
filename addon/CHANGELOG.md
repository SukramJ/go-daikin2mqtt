<!--
Home Assistant renders this file as the add-on changelog in its UI.
Keep entries condensed; the full history lives in the repository's
top-level changelog.md. Newest version first.
-->

# 0.8.1 (2026-07-19)

Fix: **the Eco mode switch no longer snaps back to off.**

- Indoor units in standby accept the econo command but never confirm it on the
  serial bus, so the switch reverted to off after two minutes even though the
  Daikin app showed eco on everywhere. The daemon now latches the last reliably
  known eco state per outdoor unit: running units remain the truth, and while
  the whole group is off the latched value stays in effect.

# 0.8.0 (2026-07-19)

Fix: **turning one indoor unit on no longer switches on the others.**

- The multi-split mode sync now only adjusts units that are **already running**
  in the opposite direction (heating vs cooling/dry) — the last command wins.
  Units that are off stay off (on the local Faikin path the `mode` command
  force-powers a unit on, so the old blind sync switched on the whole house).
- `auto`/`fan only` no longer trigger or receive a mode sync.

# 0.7.0 (2026-07-14)

New: a **manual refresh button** on the outdoor unit.

- **Refresh from cloud / Aus Cloud aktualisieren** — pressing it runs one poll
  cycle immediately (fetch all devices from the ONECTA cloud and republish every
  entity state), instead of waiting for the next scheduled poll. One button per
  outdoor unit.
- To protect the ONECTA daily request quota, a press within 30s of the last poll
  is ignored, and presses during a running poll are merged into one refresh.

# 0.6.0 (2026-07-07)

Hardening release — security/robustness audit of the whole codebase, all
confirmed findings fixed. No new dependencies, no config changes.

- **Security:** web-UI request timeouts (slow-body/idle-connection
  exhaustion), OAuth login state store capped at 128 pending entries,
  `POST /api/patch` requires `Content-Type: application/json` (CSRF),
  ONECTA PATCH URL segments path-escaped, `NaN`/`Inf` write payloads
  rejected and local `demand_control` range-checked (0–100).
- **Fixed:** a stalled cloud/token endpoint could freeze the daemon forever
  (HTTP clients now have a 60s timeout); crash-loop when a device exposes a
  climate control without a power point; retained `.../set` commands were
  re-applied on every reconnect/restart; a failed Home Assistant discovery
  publish was never retried until the entity set changed; Faikin state
  handling no longer blocks the MQTT read loop.

# 0.5.1 and earlier

See the full history in
[changelog.md](https://github.com/SukramJ/go-daikin2mqtt/blob/main/changelog.md).
