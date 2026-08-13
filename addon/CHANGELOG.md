<!--
Home Assistant renders this file as the add-on changelog in its UI.
Keep entries condensed; the full history lives in the repository's
top-level changelog.md. Newest version first.
-->

# 0.9.1 (2026-08-13)

Fix: **the Schedules view showed no selection.**

- The toggle buttons in the schedule editor (device picker, weekdays, mode,
  state) were rendered unstyled, so it was invisible which device was selected
  or which weekdays a block covered. They are styled again.
- The device picker no longer offers the Home Hub, which has no climate control
  and therefore cannot be scheduled.

# 0.9.0 (2026-08-13)

New: **weekly schedules.** A seven-day programme run by the add-on itself,
edited in a calendar view and switchable from Home Assistant. Off by default —
turn on the `schedule_enable` option.

- Blocks set power, mode and setpoint **at their start**; the state holds until
  the next block, so a change you make by hand stays until then.
- Several schedules can target the same device and are layered by **priority**,
  so a base programme can be overridden by "home office" or "holiday" that you
  switch on when needed.
- Every schedule becomes a **switch** entity under the new device
  *daikin2mqtt Scheduler*, and each climate device gains *Active schedule* and
  *Next schedule change* sensors — so schedules can be driven from ordinary HA
  automations.
- On devices mapped for local-first mode the switching goes to Faikin and
  consumes **no ONECTA requests** at all.
- The calendar warns when two units on one outdoor unit are scheduled to heat
  and cool at the same time.
- Schedules are stored on `/data` and survive add-on updates. New options:
  `schedule_enable`, `schedule_timezone`, `schedule_catchup`.

# 0.8.2 (2026-07-20)

Fix: **fan speed and refrigerant temperature are shown per indoor unit again.**

- Both readings had been collapsed into single aggregated sensors on the
  outdoor unit, hiding the individual units' values. Each indoor unit now gets
  its own diagnostic `fan_speed` (rpm) and `refrigerant_temperature` (°C)
  sensors; the misleading outdoor aggregates are removed automatically.

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
