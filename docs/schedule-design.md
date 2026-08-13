# Design: weekly schedules

This document describes the scheduler planned for the 0.9.x series: a
seven-day / 24-hour **weekly programme** that drives the climate devices from
the daemon itself, persisted to disk, configured in the web UI's calendar
view and switchable from Home Assistant.

It complements the package-level doc comments; read those for exact
signatures. For the control backend it builds on, see
[`design.md`](design.md).

## Goals

1. **A weekly programme per device** — several schedules may target the same
   device and are layered by priority, so a base programme can be overridden
   by "home office" or "holiday" without editing it.
2. **No second write path** — the scheduler produces the same internal write
   requests as an inbound MQTT `/set`, so multi-split mode sync, the
   local/cloud backend seam and the cloud lock apply unchanged.
3. **Predictable, cheap and quiet** — writes happen at block boundaries only;
   a manual change in between survives until the next boundary.
4. **Fully localised UI** — the calendar, the editor and the Home Assistant
   entity names follow the existing `LANGUAGE` rules; topics, entity ids and
   stored values stay language-independent.

## Decisions

| Aspect | Decision |
| --- | --- |
| Scope | Two schedule **types**: `indoor` drives a room's power/mode/setpoint, `outdoor` drives the settings that act on the shared compressor. |
| Time model | **Blocks with a target state.** A block occupies a time range and sets power / HVAC mode / setpoint at its start. The state holds until the next block; a gap means "no intervention". |
| Resolution | **30 minutes** — the week is a ring of 336 slots. |
| Actions | **Power + HVAC mode + setpoint.** Fan, swing and presets are deliberately out of scope; the `action` object is open for them. |
| Manual override | **Manual wins until the next block.** The engine writes at block boundaries only and never enforces in between. |
| Scope | A schedule carries a **list of target devices**; the calendar always shows exactly one device. |
| Conflicts | **Highest priority wins** per slot; on a tie the block that started later. |
| Home Assistant | **One switch per schedule** plus two per-device status sensors. |
| Persistence | `schedules.json` — atomic write, `0600`, same pattern as `auth.Store`. |

## Data model

`internal/schedule/model.go` defines the persisted shape. The file is the
source of truth; the engine only holds a derived resolution in memory.

An outdoor schedule looks the same except for its type, its targets and the
fields its blocks carry:

```jsonc
{
  "id": "nachtruhe",
  "name": "Nachtruhe",
  "type": "outdoor",
  "enabled": true,
  "priority": 0,
  "targets": [ { "outdoor_serial": "0J723746" } ],
  "blocks": [
    {
      "id": "b1",
      "label": "leise",
      "days": ["mon", "tue", "wed", "thu", "fri", "sat", "sun"],
      "start": "22:00",
      "end": "06:00",
      "action": {
        "outdoor_silent": true,   // omit a field to leave that setting alone
        "demand": 70              // econo would be the third
      }
    }
  ]
}
```

```jsonc
// $XDG_CONFIG_HOME/daikin2mqtt/schedules.json — 0600, written atomically
{
  "version": 1,
  "revision": 17,               // bumped on every write (optimistic concurrency)
  "timezone": "Europe/Berlin",  // empty = the daemon's system zone
  "schedules": [
    {
      "id": "werktag",          // slug derived from the name once, then frozen
      "name": "Werktag",        // display name, free text, renameable
      "enabled": true,
      "priority": 0,            // higher wins; tie → later block start
      "targets": [
        { "device_id": "cfcbab3e-…" },  // embedded_id optional, else the
        { "device_id": "a91f77d2-…" }   // device's climateControl point
      ],
      "blocks": [
        {
          "id": "b1",
          "label": "Aufstehen",                        // free text, optional
          "days": ["mon", "tue", "wed", "thu", "fri"], // language-independent keys
          "start": "05:30",
          "end": "08:00",                              // < start ⇒ wraps past midnight
          "action": {
            "power": "on",                             // on | off
            "hvac_mode": "heat",                       // heat|cool|auto|dry|fan_only
            "setpoint": 21.5                           // optional: omit to leave it alone
          },
          "on_end": "none"                             // none | off
        }
      ]
    }
  ]
}
```

### Two types, not one schedule with optional fields

A block that could carry both an indoor and an outdoor action would be half
meaningless for whichever target it reached — "21.5 °C" says nothing to an
outdoor unit, and "silent mode" says nothing to one room. So `type` is part of
the schedule, validation enforces the split in both directions (action fields
and targets), and the editor shows only the fields that apply.

The type is fixed once a schedule exists: changing it would invalidate every
block it already carries.

**Targets and the resolver.** A target addresses either a device (`device_id`)
or an outdoor unit (`outdoor_serial`), and `Target.Key()` maps both onto one
opaque string — an outdoor key is namespaced `outdoor:<serial>` so a serial can
never collide with a device id. Keeping resolution keyed on that string is what
lets the ring, the priority rules, the catch-up window and the idempotence
cache stay **literally unchanged** for both types.

**Backwards compatible.** A `schedules.json` written by 0.9.x has no `type` and
no outdoor targets; the empty value reads as `indoor`, so it keeps working
untouched.

Rationale:

- **Blocks, not a slot array.** The file stays readable and hand-editable;
  the 336-slot resolution is derived at load time.
- **Targets are a list of devices**, rather than schedules nested under a
  device: one programme for three rooms is maintained once.
- **`embedded_id` is optional.** The coordinator already knows each device's
  `climateControl` management point from `climateEmbedded`; the field only
  exists for devices exposing several zones.
- **`revision` instead of a timestamp.** The web UI sends the revision it
  loaded; a mismatch answers `409` instead of silently overwriting a
  concurrent edit.
- **`action` is its own object** so `fan_mode`, `swing` or `preset` can be
  added later without breaking the schema.
- **Enum values are language-independent** (`mon`, `heat`, `on`, `none`) —
  see "Internationalisation".

### Store

`internal/schedule/store.go` mirrors `auth.Store`: read the whole file,
validate, and write via temp file + `Chmod(0600)` + `Rename`. The parent
directory is created with `0700`. A missing file is not an error — it yields
an empty, valid configuration so a fresh install starts with no schedules.

Validation rejects: unknown device ids are *not* rejected (see "Edge cases"),
but times off the 30-minute grid, empty `days`, duplicate schedule ids,
unknown enum values, `setpoint` outside 5…35 °C and blocks longer than 24 h
are.

## Engine: the week as a ring

`internal/schedule/ring.go` expands every block into slot indices of a
336-slot ring (7 × 48). This turns three separate problems into array
arithmetic:

```
slot(day, hh:mm) = day*48 + (hh*60+mm)/30          day 0 = Monday

block 22:30 → 05:30 on Mon  ⇒ slots 45…58 (mod 336)   crosses midnight
block 23:00 → 05:30 on Sun  ⇒ slots 334…10 (mod 336)  crosses the week start
```

Resolution per device:

1. Collect the enabled schedules that list the device as a target.
2. Expand their blocks into slots (`end <= start` wraps into the next day,
   and past Sunday into Monday).
3. Per slot the highest `priority` wins; on a tie the candidate with the
   smaller distance to its own start — i.e. the block that started later.
4. Consecutive slots with the same winner collapse into an effective block.
   Its first slot is a **switch point**.

`Resolve(deviceID) []Winner` and `NextChange(deviceID, now) (time.Time,
Winner)` are the only two functions the engine needs from this file. Both are
pure and table-testable.

### Timer, not polling

`internal/schedule/engine.go` does not tick every 30 minutes. It arms a
single `time.Timer` on the next real switch point, computed as wall-clock
time (`time.Date(…, loc)`) and recomputed when it fires. A day without switch
points costs no wake-ups, and DST is handled by construction rather than by
arithmetic on durations.

### Idempotence and catch-up

The engine remembers the last action applied per device. A switch point that
would not change anything — two adjacent blocks with the same target state, a
reloaded but unchanged configuration — produces no write. Reloading the
configuration is therefore free of side effects.

On start-up (and after a reload) the engine does **not** blindly apply the
active block: that would overwrite a manual change, contradicting the
override rule. It applies the active block only when its start is less than
`SCHEDULE_CATCHUP` (default 1800 s) ago; otherwise it only seeds the
idempotence cache and waits for the next switch point.

## Coordinator integration

`internal/coordinator/schedule.go` implements the engine's `Applier`. It
produces the same `writeReq` values the MQTT `/set` subscription produces and
feeds them into the same channel:

```go
// ApplySchedule applies a block's target state. The requests travel through
// the regular queue: sequential drain, cloud lock, multi-split mode sync and
// the backend choice (Faikin/cloud) all apply unchanged.
func (c *Coordinator) ApplySchedule(ctx context.Context, deviceID string, a schedule.Action) {
    emb, ok := c.climateEmbeddedID(deviceID)
    if !ok {
        return // device not known yet — the next poll fills the cache
    }
    // Order matters: the setpoint PATCH path contains {mode}, resolved from
    // modeCache. noteWrite updates that cache immediately, so the mode must
    // be written before the temperature.
    c.enqueue(writeReq{deviceID, emb, hass.HVACModeTopic, a.HVACPayload()})
    if a.Setpoint != nil {
        c.enqueue(writeReq{deviceID, emb, "temperature_setpoint", a.SetpointPayload()})
    }
}
```

What that inherits:

| Existing mechanism | Effect on scheduled switches |
| --- | --- |
| `handleHVACModeWrite` | "off" and "on + mode" in one request, including the existing mode mapping |
| Multi-split mode sync | A scheduled mode change pulls the other *running* indoor units of the same outdoor unit along — never the ones that are off |
| Backend seam (`setCharacteristic`) | Mapped devices are driven locally, everything else through the cloud, with no case analysis in the scheduler |
| Sequential drain + cloud lock | Four devices at 06:00 produce an ordered series, not parallel requests |
| `noteWrite` / `modeCache` | The mode-scoped setpoint path hits the mode just set, without waiting for the next poll |
| powerful ⇄ eco | Untouched — the scheduler writes no presets and does not trigger the eco-suspend state machine |

### Outdoor schedules use the existing fan-out

`outdoor_silent`, `econo_mode` and `demand_control` are `scope: outdoor`
catalog topics, so `handleWrite` already fans every write out to each indoor
unit of the group and holds the optimistic value until a status confirms it.
The applier therefore addresses **one** member — looping here as well would
write each setting twice — and picks it deterministically (first sorted member
with a known management point) so the choice does not flap between polls.

Each of the three fields is optional: unset means "leave this alone", so a
night block can enable the silent mode without also resetting the demand limit.
econo interacts with the existing powerful ⇄ econo enforcement exactly as a
manual write does, including econo being restored after a boost ends.

The status sensors for an outdoor schedule are their own `scope: outdoor`
catalog entries, so they collapse into a single pair on the outdoor unit. Their
state is published on **every** member's topic, because which member's topic
the collapsed entity reads from depends on the discovery order — the same
reason the outdoor telemetry is published that way. They are only published
when an outdoor schedule exists, so an indoor-only installation does not grow
two empty sensors.

### Local-first coverage

All three scheduled characteristics are modelled by `faikinCommand`, so for a
mapped device a scheduled switch never falls back to the cloud:

| Action | Characteristic | Faikin command |
| --- | --- | --- |
| Power on/off | `onOffMode` | `command/<host>/power` → `true` / `false` |
| HVAC mode | `operationMode` | `command/<host>/mode` → `C H A D F` |
| Setpoint | `temperatureControl` | `command/<host>/temp` → `21.5` |

For a fully mapped installation the scheduler therefore consumes no ONECTA
quota and opens no `scan_ignore` window (that is set by `client.Patch`
only). The cloud remains a read source. Everything this document says about
request cost applies to unmapped devices only.

One caveat holds either way: `handleWrite` resolves `{mode}` from `modeCache`
*before* the backend seam is reached, so a setpoint write aborts while the
mode is unknown. Both `climateEmbedded` and `modeCache` are first filled by a
cloud poll, so the engine waits for the first successful poll and relies on
the catch-up window for a block start due in the meantime.

### The Home Assistant switch

The per-schedule switch needs no second subscription: its topic fits the
existing `<root>/+/+/+/set` filter exactly.

```
daikin/scheduler/werktag/enabled/set     # root · "scheduler" · schedule id · topic · set
```

`handleWrite` recognises the reserved device id `scheduler` and forwards to
the engine — the same routing the refresh button uses, only with state.
`scheduler` is therefore a reserved device id and rejected as a schedule id
by validation.

## Internationalisation

The scheduler follows the project's existing i18n contract (`LANGUAGE` is
`en`, the canonical fallback, or `de`). Three categories, and the important
part is which is which:

**Localised** — display only:

- Web-UI chrome: section titles, field labels, buttons, hints, error
  messages, the legend.
- The two per-device sensors' HA display names, via `name` / `name_de` in
  `characteristics.yaml` like every other entity.
- The idle value of `schedule_state`, via the catalog entry's `values`
  (`{value: idle, label: Idle, label_de: Kein Block}`) and the existing
  `LocalizedLabel` mechanism.
- HVAC mode names shown in the editor. They are **not** new strings: the API
  serves them from the existing `operation_mode` catalog entry
  (`heating → Heizen`, …), so the scheduler shows exactly the words the
  select entity already uses.
- Weekday names and clock/date formatting in the browser, via
  `Intl.DateTimeFormat(LOCALE, …)` rather than bundle entries — no duplicated
  translation table, and no drift between the two.

**Not localised** — machine-facing, stable across languages:

- MQTT topics (`schedule_state`, `schedule_next_change`,
  `scheduler/<id>/enabled`), `unique_id` and `default_entity_id`.
- Schedule ids, JSON field names and every enum value in `schedules.json`
  (`mon`, `heat`, `on`, `none`).
- Timestamps: `schedule_next_change` publishes RFC 3339 with
  `device_class: timestamp`, so Home Assistant formats it in the *viewer's*
  language rather than the daemon's.
- The `schedule_state` attributes document (`schedule_id`, `block_id`,
  `power`, `hvac_mode`, `setpoint`, `until`). Automations must match on these,
  never on the display string.

**Neither** — user content, stored and shown verbatim in every language:
a schedule's `name` and a block's `label`. There is no `name_de` for user
data; what the operator typed is what every language shows.

### Slug generation is the i18n-critical part

A schedule is user data, so unlike a catalog entry it has no English source
string to build an id from — the name the operator typed is the only source
there is. The `id` is therefore derived from that name **once, at creation**,
using the same transliteration `entityObjectID` applies (`ä→a`, `ö→o`, `ü→u`,
`ß→ss`, non-alphanumerics to `_`, adjacent duplicates collapsed), lower-cased,
and made unique with a numeric suffix when it collides:

| Name as typed | Slug | Entity |
| --- | --- | --- |
| `Werktag` | `werktag` | `switch.daikin_schedule_werktag` |
| `Bürozeit` | `burozeit` | `switch.daikin_schedule_burozeit` |
| `Urlaub / Reise` | `urlaub_reise` | `switch.daikin_schedule_urlaub_reise` |

`ü→u` rather than `ue` is not a typo: it is what `hass.slugify` — and Home
Assistant's own `slugify` — does, and a schedule id that transliterated
differently from every other entity id would be the odd one out.

The slug is then **frozen**: renaming a schedule changes `name` only, and
switching `LANGUAGE` changes nothing at all. That is what the project's
entity-id invariant actually demands — an id that never moves under a
display-name or language change; it does not demand that user-chosen names be
English. Home Assistant does not rename already-registered entities, so a slug
that tracked the name would strand the old entity on every rename. The UI shows
the slug read-only in the schedule editor, so the operator sees what the entity
will be called before saving.

The switch itself needs no localised prefix: its HA device is
`daikin2mqtt Scheduler` (a product name, not a translatable string) and the
entity `name` is the schedule's own name, so HA composes
"daikin2mqtt Scheduler Werktag" without any string the daemon would have to
translate.

### The week starts on Monday

`days` uses `mon`…`sun` with Monday as index 0, in every language —
including `en`, where a locale-driven calendar would start on Sunday. A
weekly heating programme is read as "weekdays vs. weekend", and a
language-dependent column order would make the same schedule look different
in the two UIs and break every screenshot in the documentation.

### Localised API errors

The REST layer keeps its current shape (an English developer-facing `error`
string, as `writeError` produces today) and adds a stable `code` the UI
resolves through its bundle:

```json
{ "error": "block 2: start must be on a 30-minute boundary",
  "code": "invalid_time",
  "field": "blocks[2].start" }
```

New bundle keys live under `sched.*` in both
`internal/web/assets/i18n/en.json` and `de.json`; error keys under
`sched.err.<code>`. A missing key falls back to the raw key, as `t()`
already does, so a new error code never renders as an empty string.

## HTTP API

`internal/web/schedules.go`. All mutating routes require
`Content-Type: application/json` — the same CSRF hardening `handlePatch`
uses.

| Route | Purpose |
| --- | --- |
| `GET /api/schedules` | All schedules, localised HVAC-mode options, the schedulable outdoor units, current revision |
| `POST /api/schedules` | Create; `201` with the generated slug |
| `PUT /api/schedules/{id}` | Replace; `409` on a stale revision |
| `DELETE /api/schedules/{id}` | Delete; also clears the retained HA config |
| `POST /api/schedules/{id}/enable` | Toggle only — the same path the HA switch takes |
| `GET /api/schedules/preview?target=…` | Resolved week for a device id or `outdoor:<serial>`: effective blocks, next switch point, mode conflicts (indoor only). `?device=` stays accepted for an SPA cached from 0.9.x |

The calendar renders exclusively from `preview`, so the browser never
re-implements the resolution rules — the Go engine stays the single source
of truth for what will actually happen.

## MQTT topics & discovery

```
daikin/scheduler/<scheduleID>/enabled/state                 # retained: ON | OFF
daikin/scheduler/<scheduleID>/enabled/set                    # subscribed
daikin/<deviceID>/<embeddedID>/schedule_state/state          # "Werktag · Absenkung"
daikin/<deviceID>/<embeddedID>/schedule_state/attributes     # structured, language-independent
daikin/<deviceID>/<embeddedID>/schedule_next_change/state    # 2026-08-13T16:30:00+02:00
daikin/<deviceID>/<embeddedID>/outdoor_schedule_state/state       # scope: outdoor → one entity per outdoor unit
daikin/<deviceID>/<embeddedID>/outdoor_schedule_next_change/state # published on every member (see above)
homeassistant/switch/daikin_schedule_<scheduleID>/config     # retained
```

The two per-device sensors are synthesised like the refresh button: catalog
entries matching a characteristic the cloud never reports
(`daemonSchedule`), so they travel the regular discovery path including
orphan reconciliation. The switches are published by
`internal/hass/schedule.go` against a dedicated device
(`identifiers: ["daikin_scheduler"]`, `configuration_url` pointing at the web
UI) and are included in the published-topic set so a deleted schedule's
config is cleared like any other orphan.

## Configuration reference

| Key | Default | Meaning |
| --- | --- | --- |
| `SCHEDULE_ENABLE` | `false` | Enables engine, REST routes and the UI section |
| `SCHEDULE_STORE_PATH` | XDG app dir | Path to `schedules.json` |
| `SCHEDULE_TIMEZONE` | system zone | IANA zone for the schedules' wall-clock times |
| `SCHEDULE_CATCHUP` | `1800` | Seconds a missed block start is still applied after |

Overridable via `DAIKIN_*` like every other key; surfaced as add-on options
with the mapping in `script/run.sh`.

## Web UI

A new "Schedules" section in the existing vanilla SPA (no build step):
a 7 × 24 calendar with 30-minute snapping, drag to create, a block dialog,
the schedule list with priority and enable switches, and a conflict warning.
Blocks that lost the priority resolution stay visible as hatched ghosts, so
the layering is legible rather than implied.

Colours encode the HVAC mode (heat / cool / auto / dry / off), reusing the
existing CSS custom properties and both themes.

## Edge cases

| Case | Behaviour |
| --- | --- |
| **Daemon restart** | The active block is applied only if its start is within `SCHEDULE_CATCHUP`; otherwise the cache is seeded and the next switch point awaited. A restart at 14:00 does not overwrite a manual change made at 13:00. |
| **DST, March** | Blocks starting in the missing hour have no real start time. The next switch point applies; if it falls inside the catch-up window, the skipped target state is caught up. |
| **DST, October** | The doubled hour produces two identical switch points; the second is dropped by the idempotence check. |
| **Multi-split mode conflict** | If one device schedules cooling while another on the same outdoor unit schedules heating, the hardware cannot comply. The UI checks this across all enabled schedules and warns with the time window and the devices involved. It still saves; at runtime the mode sync resolves it as "last write wins". |
| **Partially mapped installation** | The backend choice is per device. A schedule targeting a Faikin device and a cloud-only device drives one locally and PATCHes the other, with no special case in the scheduler. |
| **Cloud quota (unmapped devices only)** | Up to three PATCHes per device per switch. Three devices × four switches/day ≈ 36 requests — uncritical next to ~108 poll GETs at 200/day. |
| **Cold start** | Locally driven devices also need one cloud poll first (it fills `climateEmbedded` and `modeCache`); the engine waits for it. |
| **Device offline / write fails** | Logged, not retried: the next block start re-establishes the target state anyway. `schedule_state` reports intent, not the actual device state. |
| **Device gone from the cloud** | Targets whose device id is no longer reported are skipped and marked "unknown device" in the UI — never silently deleted, so a temporary cloud outage cannot destroy schedules. |
| **Schedule deleted** | The current device state stays as it is. There is no "revert" — the scheduler establishes states, it does not own them. |

## Testing

Table-driven, following the project conventions:

- `ring_test.go` — expansion, midnight and week wrap, priority resolution,
  tie-breaking, effective blocks, `NextChange`, conflict detection. Pure
  functions, no clock.
- `engine_test.go` — injected clock: switch point firing, idempotence,
  catch-up inside and outside the window, reload without side effects, DST
  transitions using a fixed `Europe/Berlin` location.
- `store_test.go` — round-trip, `0600`, atomic replace, missing file,
  rejected invalid documents.
- `coordinator/schedule_test.go` — stub applier and stub MQTT: request order
  (mode before setpoint), `scheduler/<id>/enabled/set` routing, no write while
  `modeCache` is empty.
- `web/schedules_test.go` — CRUD, revision conflict `409`, content-type
  rejection, error `code` presence.
- `hass/schedule_test.go` — slug stability across renames, `unique_id` and
  `default_entity_id` shape, localised sensor names in both languages.

## Milestones

1. **Model and store** — types, validation, atomic persistence.
   `internal/schedule/{model,store}.go`
2. **Ring and resolution** — expansion, priorities, effective blocks, next
   switch point, conflict detection. `internal/schedule/ring.go`
3. **Engine and wiring** — timer, catch-up, idempotence, `Applier`; the
   coordinator side plus the two status sensors.
   `internal/schedule/engine.go`, `internal/coordinator/schedule.go`
4. **REST and discovery** — six routes, switch discovery, two catalog
   entries. `internal/web/schedules.go`, `internal/hass/schedule.go`,
   `characteristics.yaml`
5. **Web UI** — calendar, dialog, management, conflict banner, new `sched.*`
   keys in both i18n bundles.

Steps 1 and 2 are pure library code with no dependency on the rest of the
daemon.

## Out of scope

- **Date ranges** ("holiday from the 3rd to the 17th") — expressed by a
  high-priority schedule switched by hand or by a Home Assistant automation.
- **Sun events, presence, conditions** — these belong in HA automations that
  operate the schedule switch. The scheduler stays a weekly clock.
- **Fan and swing** — the `action` object is open for them.
- **powerful on a schedule** — it is per indoor unit but drives the shared
  compressor and is mutually exclusive with econo, which an outdoor schedule
  now sets. Scheduling both would need the suspend/restore state machine to
  arbitrate between two automated writers, not just between a user and one.
- **Continuous enforcement** — deliberately not implemented; see the override
  decision. If wanted for locally driven devices (where writes are free), it
  would be a per-schedule flag, not a global mode.
