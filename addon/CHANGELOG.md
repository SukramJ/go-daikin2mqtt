<!--
Home Assistant renders this file as the add-on changelog in its UI.
Keep entries condensed; the full history lives in the repository's
top-level changelog.md. Newest version first.
-->

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
