// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ
//
// Diagnostic UI logic for go-daikin2mqtt. Plain vanilla JS (no framework,
// no build step). All fetch/asset URLs are relative so the page works both
// directly and behind Home-Assistant ingress.
//
// Localisation: the server reports the configured LANGUAGE via api/config;
// the matching bundle in i18n/<lang>.json drives all static chrome
// (data-i18n attributes) and the dynamic strings via t()/tf().
"use strict";

// ---------- i18n state ----------
let I18N = {};
let LANG = "en";
let LOCALE = "en-US";

function t(key) {
  return key in I18N ? I18N[key] : key;
}

// tf looks up key and substitutes {name} placeholders from params.
function tf(key, params) {
  let s = t(key);
  for (const k in params) s = s.replace("{" + k + "}", params[k]);
  return s;
}

async function loadI18n(lang) {
  LANG = lang === "de" ? "de" : "en";
  LOCALE = LANG === "de" ? "de-DE" : "en-US";
  document.documentElement.lang = LANG;
  try {
    I18N = await fetchJSON("i18n/" + LANG + ".json");
  } catch (e) {
    I18N = {}; // fall back to raw keys / English HTML defaults
  }
  applyStaticI18n();
}

// applyStaticI18n fills every element carrying a data-i18n / data-i18n-title
// attribute from the loaded bundle. Missing keys leave the HTML default.
function applyStaticI18n() {
  document.querySelectorAll("[data-i18n]").forEach((el) => {
    const key = el.dataset.i18n;
    if (key in I18N) el.textContent = I18N[key];
  });
  document.querySelectorAll("[data-i18n-title]").forEach((el) => {
    const key = el.dataset.i18nTitle;
    if (key in I18N) el.title = I18N[key];
  });
}

// ---------- bootstrap ----------
document.addEventListener("DOMContentLoaded", init);

async function init() {
  initTheme();
  initNav();
  // Language first so all subsequent rendering is localised.
  let config = null;
  try {
    config = await fetchJSON("api/config");
  } catch (e) {
    /* non-critical */
  }
  await loadI18n((config && config.language) || "en");
  if (config) renderConfig(config);

  wireActions();
  initSchedules(config);
  await refreshAuth();
  await refreshRateLimit();
}

function wireActions() {
  document.getElementById("auth-refresh").addEventListener("click", refreshAuth);
  document.getElementById("status-refresh").addEventListener("click", refreshRateLimit);
  document.getElementById("devices-load").addEventListener("click", loadDevices);
  document.getElementById("patch-form").addEventListener("submit", submitPatch);
}

// ---------- auth ----------
async function refreshAuth() {
  let st = null;
  try {
    st = await fetchJSON("api/auth/status");
  } catch (e) {
    toast(t("toast.authFail") + e.message, "err");
    return;
  }
  const badge = document.getElementById("auth-badge");
  if (st.authenticated) {
    badge.className = "badge badge-ok";
    badge.textContent = t("status.authenticated");
  } else {
    badge.className = "badge badge-err";
    badge.textContent = t("status.unauthenticated");
  }
  const items = [
    tile(t("auth.state"), st.authenticated ? t("status.authenticated") : t("status.unauthenticated"),
      st.authenticated ? "ok" : "err"),
    tile(t("auth.detail"), st.detail || "—", null),
  ];
  if (st.expires_at) items.push(tile(t("auth.expires"), st.expires_at, null));
  // Surface the redirect_uri the next login will send so the operator can
  // register exactly this value with the Daikin portal (behind ingress it is
  // derived from the request and is easy to get wrong).
  if (!st.authenticated && st.redirect_uri)
    items.push(tile(t("auth.redirect"), st.redirect_uri, null));
  setGrid("auth-grid", items);

  // The connect button is most relevant when not yet authenticated.
  const btn = document.getElementById("login-btn");
  btn.textContent = st.authenticated ? t("btn.reconnect") : t("btn.connect");
}

// ---------- devices ----------
async function loadDevices() {
  const status = document.getElementById("devices-status");
  status.textContent = t("devices.loading");
  let devices = null;
  try {
    devices = await fetchJSON("api/devices");
  } catch (e) {
    status.textContent = "";
    toast(t("toast.devicesFail") + e.message, "err");
    return;
  }
  status.textContent = tf("devices.count", { n: devices.length });
  renderDevices(devices);
}

function renderDevices(devices) {
  const host = document.getElementById("devices-host");
  host.innerHTML = "";
  if (!devices.length) {
    host.innerHTML = `<div class="card empty">${t("devices.none")}</div>`;
    return;
  }
  for (const d of devices) {
    const card = el("div", "card device-card");
    const head = el("div", "group-head");
    head.appendChild(el("h3", null, d.model || d.id));
    head.appendChild(el("small", "muted", d.id));
    card.appendChild(head);

    for (const mp of d.management_points || []) {
      const mpHead = el("div", "mp-head");
      const title = [mp.type || mp.embedded_id];
      if (mp.category) title.push(mp.category);
      mpHead.appendChild(el("strong", null, title.join(" · ")));
      mpHead.appendChild(el("small", "muted", " " + mp.embedded_id));
      card.appendChild(mpHead);

      const table = el("table", "dp-table");
      const thead = el("tr");
      for (const h of ["dp.char", "dp.value", "dp.unit", "dp.settable", "dp.catalog"]) {
        thead.appendChild(el("th", null, t(h)));
      }
      table.appendChild(thead);

      const chars = (mp.characteristics || []).slice().sort((a, b) =>
        a.name.localeCompare(b.name, LOCALE));
      for (const c of chars) {
        const tr = el("tr");
        const nameTd = el("td", "dp-name");
        nameTd.appendChild(el("strong", null, c.display_name || c.name));
        if (c.display_name) nameTd.appendChild(el("small", "muted", " " + c.name));
        tr.appendChild(nameTd);

        tr.appendChild(el("td", "dp-value", fmtValue(c.value)));
        tr.appendChild(el("td", null, c.unit || ""));
        tr.appendChild(el("td", null, c.settable ? t("val.yes") : t("val.no")));

        const cat = el("td");
        if (c.matched) {
          cat.appendChild(el("span", "badge badge-ok small", c.platform || t("val.yes")));
          if (c.topic) cat.appendChild(el("small", "muted", " " + c.topic));
        } else {
          cat.appendChild(el("span", "badge badge-neutral small", t("dp.unmatched")));
        }
        tr.appendChild(cat);

        // Clicking a settable row pre-fills the PATCH form.
        if (c.settable) {
          tr.classList.add("clickable");
          tr.addEventListener("click", () =>
            fillPatch(d.id, mp.embedded_id, c.name, c.value));
        }
        table.appendChild(tr);
      }
      card.appendChild(table);
    }
    host.appendChild(card);
  }
}

function fillPatch(device, embedded, char, value) {
  document.getElementById("p-device").value = device;
  document.getElementById("p-embedded").value = embedded;
  document.getElementById("p-char").value = char;
  document.getElementById("p-value").value =
    value === null || value === undefined ? "" : String(value);
  document.getElementById("p-json").checked = typeof value === "number" || typeof value === "boolean";
  location.hash = "#sec-patch";
  toast(t("toast.prefilled"), "ok");
}

// ---------- patch ----------
async function submitPatch(ev) {
  ev.preventDefault();
  const device = val("p-device");
  const embedded = val("p-embedded");
  const char = val("p-char");
  const raw = val("p-value");
  const path = val("p-path");
  const asJSON = document.getElementById("p-json").checked;

  if (!device || !embedded || !char) {
    toast(t("toast.patchFields"), "err");
    return;
  }

  let value = raw;
  if (asJSON) {
    try {
      value = JSON.parse(raw);
    } catch (e) {
      toast(t("toast.badJSON"), "err");
      return;
    }
  }

  const body = { deviceId: device, embeddedId: embedded, characteristic: char, value };
  if (path) body.path = path;

  try {
    const res = await fetch("api/patch", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      const data = await res.json().catch(() => ({}));
      throw new Error(data.error || res.statusText);
    }
    toast(tf("toast.patchOk", { char }), "ok");
  } catch (e) {
    toast(t("toast.errorPrefix") + e.message, "err");
  }
}

// ---------- rate limit / config ----------
async function refreshRateLimit() {
  let rl = null;
  try {
    rl = await fetchJSON("api/ratelimit");
  } catch (e) {
    toast(t("toast.statusFail") + e.message, "err");
    return;
  }
  const items = [
    tile(t("rl.minute"), rl.remaining_minute + " / " + rl.limit_minute, null),
    tile(t("rl.day"), rl.remaining_day + " / " + rl.limit_day, null),
    tile(t("rl.retry"), (rl.retry_after || 0) + " " + t("unit.seconds"), null),
    tile(t("rl.reset"), rl.reset_at && !rl.reset_at.startsWith("0001") ? rl.reset_at : "—", null),
    tile(t("rl.updated"), rl.updated && !rl.updated.startsWith("0001") ? rl.updated : "—", null),
  ];
  setGrid("ratelimit-grid", items);
  document.getElementById("last-update").textContent =
    t("footer.updated") + new Date().toLocaleTimeString(LOCALE);
}

function renderConfig(c) {
  const web = c.web || {};
  const items = [
    tile(t("cfg.language"), c.language || "en", null),
    tile(t("cfg.hass"), c.hass_enable ? t("val.active") : t("val.off"), c.hass_enable ? "ok" : null),
    tile(t("cfg.bind"), web.bind || "—", null),
    tile(t("cfg.auth"), web.auth_on ? t("val.on") : t("val.off"), null),
  ];
  setGrid("config-grid", items);
}

// ---------- helpers ----------
function val(id) {
  return document.getElementById(id).value.trim();
}

function fmtValue(v) {
  if (v === null || v === undefined || v === "") return "–";
  if (typeof v === "boolean") return v ? t("val.yes") : t("val.no");
  if (typeof v === "number") {
    if (Number.isInteger(v)) return v.toLocaleString(LOCALE);
    return v.toLocaleString(LOCALE, { maximumFractionDigits: 3 });
  }
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}

function tile(label, value, tone) {
  return { label, value, tone };
}

function setGrid(id, items) {
  const host = document.getElementById(id);
  host.innerHTML = "";
  for (const it of items) {
    const item = el("div", "status-item" + (it.tone ? " tone-" + it.tone : ""));
    item.appendChild(el("div", "label", it.label));
    item.appendChild(el("div", "value", it.value));
    host.appendChild(item);
  }
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

async function fetchJSON(url) {
  const res = await fetch(url);
  if (!res.ok) throw new Error(res.status + " " + res.statusText);
  return res.json();
}

let toastTimer = null;
function toast(msg, kind) {
  const el = document.getElementById("toast");
  el.textContent = msg;
  el.className = "toast " + (kind || "ok");
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (el.hidden = true), 3500);
}

// ---------- nav + theme ----------
function initNav() {
  const items = [...document.querySelectorAll(".nav-item")];
  const sections = items
    .map((i) => document.getElementById(i.dataset.target))
    .filter(Boolean);
  const obs = new IntersectionObserver(
    (entries) => {
      for (const e of entries) {
        if (e.isIntersecting) {
          items.forEach((i) =>
            i.classList.toggle("active", i.dataset.target === e.target.id));
        }
      }
    },
    { rootMargin: "-40% 0px -55% 0px" }
  );
  sections.forEach((s) => obs.observe(s));
}

function initTheme() {
  const saved = localStorage.getItem("daikin-theme");
  if (saved) document.documentElement.dataset.theme = saved;
  document.getElementById("theme-toggle").addEventListener("click", () => {
    const cur = document.documentElement.dataset.theme === "light" ? "dark" : "light";
    document.documentElement.dataset.theme = cur;
    localStorage.setItem("daikin-theme", cur);
  });
}

// ============================================================================
// Weekly schedules
//
// The calendar renders exclusively from api/schedules/preview: the Go engine
// owns the priority rules, and duplicating them here would guarantee the two
// eventually disagree. The only geometry done in the browser is turning the
// raw blocks into the hatched "covered" layer behind the effective one.
// ============================================================================

const SCHED = {
  revision: 0,
  schedules: [],
  modes: [],
  slotMinutes: 30,
  days: [],
  devices: [],
  device: null,
  preview: null,
};

const MODE_COLOR = {
  heat: "var(--mode-heat)",
  cool: "var(--mode-cool)",
  auto: "var(--mode-auto)",
  dry: "var(--mode-dry)",
  fan_only: "var(--mode-fan)",
  off: "var(--mode-off)",
};

// dayNames renders the weekday headers in the UI language. Monday stays first
// in every language: the stored day keys are Monday-based, and a locale-driven
// order would make the same schedule look different in the two UIs.
function dayNames(style) {
  const out = [];
  for (let i = 0; i < 7; i++) {
    // 2024-01-01 was a Monday.
    const d = new Date(Date.UTC(2024, 0, 1 + i));
    out.push(new Intl.DateTimeFormat(LOCALE, { weekday: style, timeZone: "UTC" }).format(d));
  }
  return out;
}

function fmtTemp(v) {
  if (v === null || v === undefined) return "—";
  return v.toLocaleString(LOCALE, { minimumFractionDigits: 1, maximumFractionDigits: 1 }) + " °C";
}

function toMin(hhmm) {
  const [h, m] = hhmm.split(":").map(Number);
  return h * 60 + m;
}

function fmtMin(min) {
  if (min >= 1440) return "24:00";
  return String(Math.floor(min / 60)).padStart(2, "0") + ":" + String(min % 60).padStart(2, "0");
}

function modeLabel(value) {
  const m = SCHED.modes.find((x) => x.value === value);
  return m ? m.label : value;
}

function deviceName(id) {
  const d = SCHED.devices.find((x) => x.id === id);
  return d ? d.name || d.model || d.id : id;
}

// ---------- loading ----------

async function loadSchedules() {
  let data = null;
  try {
    data = await fetchJSON("api/schedules");
  } catch (e) {
    toast(t("sched.err.load") + e.message, "err");
    return;
  }
  SCHED.revision = data.revision;
  SCHED.schedules = data.schedules || [];
  SCHED.modes = data.modes || [];
  SCHED.slotMinutes = data.slot_minutes || 30;
  SCHED.days = data.days || [];
  document.getElementById("tz-note")?.replaceChildren(document.createTextNode(data.timezone || ""));

  if (!SCHED.devices.length) await loadScheduleDevices();
  if (!SCHED.device) SCHED.device = pickDefaultDevice();
  await refreshPreview();
}

// loadScheduleDevices reuses api/devices — the same call the device browser
// makes — so the picker needs no extra endpoint.
//
// Only devices with a climateControl management point can be scheduled: the
// blocks write hvac_mode and temperature_setpoint, which live there. A gateway
// (the Home Hub) has no such point and would only be an unselectable entry.
// Devices already referenced by a schedule are kept regardless, so a plan never
// becomes invisible because its device is momentarily unresolvable.
async function loadScheduleDevices() {
  try {
    const devices = await fetchJSON("api/devices");
    const referenced = new Set();
    for (const s of SCHED.schedules) for (const tgt of s.targets || []) referenced.add(tgt.device_id);
    SCHED.devices = devices
      .filter((d) =>
        referenced.has(d.id) ||
        (d.management_points || []).some((mp) => mp.type === "climateControl"))
      .map((d) => ({ id: d.id, name: d.name, model: d.model }));
  } catch (e) {
    // The cloud may be unreachable; schedules still edit fine, the picker
    // just shows the ids it finds in the existing schedules.
    const ids = new Set();
    for (const s of SCHED.schedules) for (const tgt of s.targets || []) ids.add(tgt.device_id);
    SCHED.devices = [...ids].map((id) => ({ id, name: id }));
  }
}

function pickDefaultDevice() {
  for (const s of SCHED.schedules) {
    if (s.targets && s.targets.length) return s.targets[0].device_id;
  }
  return SCHED.devices.length ? SCHED.devices[0].id : null;
}

async function refreshPreview() {
  if (!SCHED.device) {
    SCHED.preview = null;
    renderSchedules();
    return;
  }
  try {
    SCHED.preview = await fetchJSON("api/schedules/preview?device=" + encodeURIComponent(SCHED.device));
  } catch (e) {
    SCHED.preview = null;
    toast(t("sched.err.load") + e.message, "err");
  }
  renderSchedules();
}

// ---------- rendering ----------

function renderSchedules() {
  renderScheduleDevices();
  renderScheduleList();
  renderCalendar();
  renderScheduleStatus();
  renderConflicts();
  renderLegend();
}

function renderScheduleDevices() {
  const host = document.getElementById("sched-devices");
  host.innerHTML = "";
  for (const d of SCHED.devices) {
    const c = el("button", "chip", d.name || d.model || d.id);
    c.type = "button";
    c.setAttribute("aria-pressed", String(d.id === SCHED.device));
    c.addEventListener("click", async () => {
      SCHED.device = d.id;
      await refreshPreview();
    });
    host.appendChild(c);
  }
}

function scheduleColor(s) {
  const b = (s.blocks || [])[0];
  if (!b) return "var(--mode-fan)";
  return b.action.power === "off" ? MODE_COLOR.off : MODE_COLOR[b.action.hvac_mode] || MODE_COLOR.auto;
}

function renderScheduleList() {
  const host = document.getElementById("sched-list");
  host.innerHTML = "";
  if (!SCHED.schedules.length) {
    host.appendChild(el("div", "muted", t("sched.none")));
    return;
  }
  const sorted = [...SCHED.schedules].sort((a, b) => b.priority - a.priority);
  for (const s of sorted) {
    const targets = (s.targets || []).map((x) => x.device_id);
    const row = el("button", "sched-row" + (targets.includes(SCHED.device) ? "" : " dim"));
    row.type = "button";
    row.setAttribute("role", "switch");
    row.setAttribute("aria-checked", String(s.enabled));

    const nm = el("span", "nm");
    const sw = el("i", "swatch");
    sw.style.background = scheduleColor(s);
    nm.append(sw, document.createTextNode(s.name));

    const knob = el("span", "switch");
    knob.setAttribute("aria-checked", String(s.enabled));
    knob.setAttribute("aria-hidden", "true");

    const sub = el("span", "sub",
      tf("sched.summary", { prio: s.priority, dev: targets.length, blocks: (s.blocks || []).length }));

    row.append(nm, knob, sub);
    row.addEventListener("click", () => toggleSchedule(s));
    // A second, explicit control for editing: the row itself is the switch.
    const edit = el("button", "iconbtn edit", "✎");
    edit.type = "button";
    edit.title = t("sched.editSchedule");
    edit.addEventListener("click", (ev) => {
      ev.stopPropagation();
      openPlanDialog(s);
    });

    const wrap = el("div", "inline");
    wrap.style.cssText = "align-items:stretch;gap:4px";
    wrap.append(row, edit);
    host.appendChild(wrap);
  }
}

function renderLegend() {
  const host = document.getElementById("sched-legend");
  host.innerHTML = "";
  for (const m of SCHED.modes) {
    const s = el("span");
    const i = el("i");
    i.style.background = MODE_COLOR[m.value] || MODE_COLOR.auto;
    s.append(i, document.createTextNode(m.label));
    host.appendChild(s);
  }
  const off = el("span");
  const oi = el("i");
  oi.style.background = MODE_COLOR.off;
  off.append(oi, document.createTextNode(t("sched.off")));
  host.appendChild(off);

  const ghost = el("span");
  const gi = el("i");
  gi.style.cssText =
    "background:repeating-linear-gradient(135deg,var(--text-soft) 0 3px,transparent 3px 6px);border:1px dashed var(--text-soft)";
  ghost.append(gi, document.createTextNode(t("sched.covered")));
  host.appendChild(ghost);
}

// coveredSegments returns the per-day segments of every enabled block that
// targets the current device. Drawn behind the effective layer, the parts that
// lost the priority resolution show through as hatching.
function coveredSegments() {
  const out = [];
  for (const s of SCHED.schedules) {
    if (!s.enabled) continue;
    if (!(s.targets || []).some((x) => x.device_id === SCHED.device)) continue;
    for (const b of s.blocks || []) {
      const start = toMin(b.start);
      let dur = toMin(b.end) - start;
      if (dur <= 0) dur += 1440;
      for (const day of b.days || []) {
        const di = SCHED.days.indexOf(day);
        if (di < 0) continue;
        // Split at midnight, walking into the following days.
        let remaining = dur;
        let cur = start;
        let d = di;
        while (remaining > 0) {
          const take = Math.min(remaining, 1440 - cur);
          out.push({ day: d % 7, from: cur, to: cur + take, schedule: s, block: b });
          remaining -= take;
          cur = 0;
          d = (d + 1) % 7;
        }
      }
    }
  }
  return out;
}

function renderCalendar() {
  const cal = document.getElementById("sched-cal");
  cal.innerHTML = "";
  const names = dayNames("short");
  const now = new Date();
  const today = (now.getDay() + 6) % 7;

  cal.appendChild(el("div", "cal-corner"));
  names.forEach((n, i) => cal.appendChild(el("div", "cal-head" + (i === today ? " today" : ""), n)));

  const axis = el("div", "cal-axis");
  for (let h = 0; h <= 24; h += 3) {
    const s = el("span", null, String(h).padStart(2, "0") + ":00");
    s.style.top = "calc(var(--hour) * " + h + ")";
    axis.appendChild(s);
  }
  cal.appendChild(axis);

  const effective = (SCHED.preview && SCHED.preview.segments) || [];
  const covered = coveredSegments();

  for (let day = 0; day < 7; day++) {
    const col = el("div", "cal-col" + (day >= 5 ? " weekend" : ""));

    for (const c of covered.filter((x) => x.day === day)) {
      // Skip what is already drawn as effective at exactly this position.
      const isEffective = effective.some(
        (e) => e.day === day && toMin(e.from) === c.from && toMin(e.to) === c.to && e.block_id === c.block.id
      );
      if (isEffective) continue;
      col.appendChild(coveredEl(c));
    }
    for (const seg of effective.filter((x) => x.day === day)) {
      col.appendChild(effectiveEl(seg));
    }

    if (day === today) {
      const line = el("div", "nowline");
      line.style.top = "calc(var(--hour) * " + ((now.getHours() * 60 + now.getMinutes()) / 60).toFixed(3) + ")";
      col.appendChild(line);
    }

    col.addEventListener("click", (ev) => {
      if (ev.target !== col) return;
      const rect = col.getBoundingClientRect();
      const raw = ((ev.clientY - rect.top) / rect.height) * 1440;
      const snapped = Math.min(Math.round(raw / SCHED.slotMinutes) * SCHED.slotMinutes, 1440 - SCHED.slotMinutes);
      openNewBlock(day, snapped);
    });
    cal.appendChild(col);
  }
}

function placeBlock(node, from, to) {
  node.style.top = "calc(var(--hour) * " + (from / 60).toFixed(4) + ")";
  node.style.height = "calc(var(--hour) * " + ((to - from) / 60).toFixed(4) + " - 2px)";
}

function effectiveEl(seg) {
  const btn = el("button", "blk");
  btn.type = "button";
  btn.style.setProperty("--c", seg.power === "off" ? MODE_COLOR.off : MODE_COLOR[seg.hvac_mode] || MODE_COLOR.auto);
  placeBlock(btn, toMin(seg.from), toMin(seg.to));
  btn.appendChild(el("b", null, seg.power === "off" ? t("sched.off") : fmtTemp(seg.setpoint)));
  btn.appendChild(el("span", null, seg.schedule_name + (seg.label ? " · " + seg.label : "")));
  btn.title = seg.schedule_name + " · " + seg.from + "–" + seg.to + " · " + modeLabel(seg.hvac_mode);
  btn.addEventListener("click", () => {
    const s = SCHED.schedules.find((x) => x.id === seg.schedule_id);
    const b = s && (s.blocks || []).find((x) => x.id === seg.block_id);
    if (s && b) openBlockDialog(s, b, false);
  });
  return btn;
}

function coveredEl(c) {
  const div = el("div", "blk ghost");
  const a = c.block.action;
  div.style.setProperty("--c", a.power === "off" ? MODE_COLOR.off : MODE_COLOR[a.hvac_mode] || MODE_COLOR.auto);
  placeBlock(div, c.from, c.to);
  div.appendChild(el("b", null, a.power === "off" ? t("sched.off") : fmtTemp(a.setpoint)));
  div.title = c.schedule.name + " · " + t("sched.covered");
  return div;
}

function renderScheduleStatus() {
  const host = document.getElementById("sched-status");
  host.innerHTML = "";
  const p = SCHED.preview;
  const add = (label, value) => {
    const w = el("span");
    w.append(document.createTextNode(label + " "), el("b", null, value));
    host.appendChild(w);
  };

  const active = p && p.active;
  add(t("sched.now"), active
    ? active.schedule_name + (active.label ? " · " + active.label : "") +
      (active.power === "off" ? " (" + t("sched.off") + ")" : " · " + fmtTemp(active.setpoint))
    : t("sched.idle"));
  add(t("sched.next"), p && p.next_change
    ? new Date(p.next_change).toLocaleString(LOCALE, { weekday: "short", hour: "2-digit", minute: "2-digit" })
    : "—");
  const switches = p && p.counts ? p.counts.switches_per_week || 0 : 0;
  add(t("sched.switches"), (switches / 7).toLocaleString(LOCALE, { maximumFractionDigits: 1 }));
  const tz = el("span", "muted");
  tz.id = "tz-note";
  host.appendChild(tz);
}

function renderConflicts() {
  const banner = document.getElementById("sched-conflict");
  const list = (SCHED.preview && SCHED.preview.conflicts) || [];
  if (!list.length) {
    banner.hidden = true;
    return;
  }
  const c = list[0];
  const names = dayNames("short");
  banner.hidden = false;
  banner.innerHTML = "";
  banner.appendChild(el("span", "ico", "▲"));
  banner.appendChild(el("span", null, tf("sched.conflict", {
    group: c.group,
    from: names[c.from_day] + " " + c.from,
    to: names[c.to_day] + " " + c.to,
    cooling: c.cooling.map(deviceName).join(", "),
    heating: c.heating.map(deviceName).join(", "),
    more: list.length > 1 ? tf("sched.conflictMore", { n: list.length - 1 }) : "",
  })));
}

// ---------- editing ----------

async function toggleSchedule(s) {
  try {
    const res = await fetch("api/schedules/" + encodeURIComponent(s.id) + "/enable", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ enabled: !s.enabled }),
    });
    if (!res.ok) throw await apiError(res);
    await loadSchedules();
  } catch (e) {
    toast(e.message, "err");
  }
}

// apiError turns an error response into an Error carrying the localized text
// for its stable code, falling back to the server's English message.
async function apiError(res) {
  const data = await res.json().catch(() => ({}));
  const key = data.code ? "sched.err." + data.code : null;
  const localized = key && t(key) !== key ? t(key) : null;
  return new Error(localized || data.error || res.statusText);
}

let blockCtx = null;

function openNewBlock(day, startMin) {
  const s = pickTargetSchedule();
  if (!s) {
    toast(t("sched.needSchedule"), "err");
    return;
  }
  const end = Math.min(startMin + 60, 1440);
  openBlockDialog(s, {
    id: "",
    label: "",
    days: [SCHED.days[day]],
    start: fmtMin(startMin),
    end: fmtMin(end),
    action: { power: "on", hvac_mode: SCHED.modes.length ? SCHED.modes[0].value : "heat", setpoint: 21 },
    on_end: "none",
  }, true);
}

// pickTargetSchedule chooses which schedule a newly drawn block joins: the
// highest-priority enabled one that targets the current device.
function pickTargetSchedule() {
  const candidates = SCHED.schedules
    .filter((s) => s.enabled && (s.targets || []).some((x) => x.device_id === SCHED.device))
    .sort((a, b) => b.priority - a.priority);
  return candidates[0] || null;
}

function openBlockDialog(sched, block, isNew) {
  const copy = JSON.parse(JSON.stringify(block));
  blockCtx = { schedule: sched, block, copy, isNew };
  document.getElementById("dlg-title").textContent = sched.name;
  document.getElementById("dlg-delete").hidden = isNew;
  const body = document.getElementById("dlg-body");
  body.innerHTML = "";
  body.appendChild(buildBlockEditor(copy));
  document.getElementById("block-dialog").showModal();
}

function buildBlockEditor(b) {
  const wrap = el("div");
  wrap.style.cssText = "display:flex;flex-direction:column;gap:13px";
  const row = (labelKey, node) => {
    const r = el("div", "field-row");
    r.appendChild(el("label", null, t(labelKey)));
    r.appendChild(node);
    wrap.appendChild(r);
    return r;
  };

  const label = el("input", "field");
  label.type = "text";
  label.value = b.label || "";
  label.addEventListener("input", () => (b.label = label.value));
  row("sched.label", label);

  const days = el("div", "inline");
  dayNames("narrow").forEach((n, i) => {
    const key = SCHED.days[i];
    const c = el("button", "chip", n);
    c.type = "button";
    c.setAttribute("aria-pressed", String((b.days || []).includes(key)));
    c.addEventListener("click", () => {
      b.days = (b.days || []).includes(key) ? b.days.filter((x) => x !== key) : [...(b.days || []), key];
      c.setAttribute("aria-pressed", String(b.days.includes(key)));
    });
    days.appendChild(c);
  });
  row("sched.days", days);

  const times = el("div", "inline");
  times.append(
    timeSelect(b.start, false, (v) => (b.start = v)),
    el("span", "muted", "–"),
    timeSelect(b.end, true, (v) => (b.end = v))
  );
  row("sched.time", times);

  const power = el("div", "inline");
  [["on", t("sched.on")], ["off", t("sched.off")]].forEach(([v, lbl]) => {
    const c = el("button", "chip", lbl);
    c.type = "button";
    c.dataset.power = v;
    c.setAttribute("aria-pressed", String(b.action.power === v));
    c.addEventListener("click", () => {
      b.action.power = v;
      wrap.querySelectorAll("[data-power]").forEach((x) =>
        x.setAttribute("aria-pressed", String(x.dataset.power === v)));
      syncPowerState();
    });
    power.appendChild(c);
  });
  row("sched.power", power);

  const modes = el("div", "inline");
  for (const m of SCHED.modes) {
    const c = el("button", "chip", m.label);
    c.type = "button";
    c.dataset.mode = m.value;
    c.setAttribute("aria-pressed", String(b.action.hvac_mode === m.value));
    c.addEventListener("click", () => {
      b.action.hvac_mode = m.value;
      wrap.querySelectorAll("[data-mode]").forEach((x) =>
        x.setAttribute("aria-pressed", String(x.dataset.mode === m.value)));
    });
    modes.appendChild(c);
  }
  const modeRow = row("sched.mode", modes);

  const stepper = el("div", "stepper");
  const val = el("span", "val", fmtTemp(b.action.setpoint));
  const step = (delta) => {
    const base = b.action.setpoint === null || b.action.setpoint === undefined ? 21 : b.action.setpoint;
    b.action.setpoint = Math.min(35, Math.max(5, Math.round((base + delta) * 2) / 2));
    val.textContent = fmtTemp(b.action.setpoint);
    keep.setAttribute("aria-pressed", "false");
  };
  const minus = el("button", "iconbtn", "−");
  const plus = el("button", "iconbtn", "+");
  minus.type = plus.type = "button";
  minus.addEventListener("click", () => step(-0.5));
  plus.addEventListener("click", () => step(0.5));
  const keep = el("button", "chip", t("sched.keepTemp"));
  keep.type = "button";
  keep.setAttribute("aria-pressed", String(b.action.setpoint === null || b.action.setpoint === undefined));
  keep.addEventListener("click", () => {
    b.action.setpoint = b.action.setpoint === null || b.action.setpoint === undefined ? 21 : null;
    val.textContent = fmtTemp(b.action.setpoint);
    keep.setAttribute("aria-pressed", String(b.action.setpoint === null));
  });
  stepper.append(minus, val, plus, keep);
  const tempRow = row("sched.setpoint", stepper);

  const onEnd = el("div", "inline");
  [["none", t("sched.endKeep")], ["off", t("sched.endOff")]].forEach(([v, lbl]) => {
    const c = el("button", "chip", lbl);
    c.type = "button";
    c.dataset.end = v;
    c.setAttribute("aria-pressed", String((b.on_end || "none") === v));
    c.addEventListener("click", () => {
      b.on_end = v;
      wrap.querySelectorAll("[data-end]").forEach((x) =>
        x.setAttribute("aria-pressed", String(x.dataset.end === v)));
    });
    onEnd.appendChild(c);
  });
  row("sched.onEnd", onEnd);

  function syncPowerState() {
    const off = b.action.power === "off";
    for (const r of [modeRow, tempRow]) {
      r.style.opacity = off ? "0.45" : "1";
      r.style.pointerEvents = off ? "none" : "auto";
    }
  }
  syncPowerState();
  return wrap;
}

// timeSelect builds a 30-minute grid dropdown; allow24 adds the "24:00" end.
function timeSelect(value, allow24, set) {
  const s = el("select", "field");
  const last = allow24 ? 1440 : 1440 - SCHED.slotMinutes;
  for (let m = 0; m <= last; m += SCHED.slotMinutes) {
    const o = el("option", null, fmtMin(m));
    o.value = fmtMin(m);
    s.appendChild(o);
  }
  s.value = value;
  s.addEventListener("change", () => set(s.value));
  return s;
}

async function saveBlock() {
  if (!blockCtx) return;
  const { schedule: s, block, copy, isNew } = blockCtx;
  const next = JSON.parse(JSON.stringify(s));
  if (isNew) {
    next.blocks = [...(next.blocks || []), copy];
  } else {
    next.blocks = (next.blocks || []).map((b) => (b.id === block.id ? copy : b));
  }
  await putSchedule(next);
}

async function deleteBlock() {
  if (!blockCtx || blockCtx.isNew) return;
  const { schedule: s, block } = blockCtx;
  const next = JSON.parse(JSON.stringify(s));
  next.blocks = (next.blocks || []).filter((b) => b.id !== block.id);
  await putSchedule(next);
}

async function putSchedule(next) {
  try {
    const res = await fetch("api/schedules/" + encodeURIComponent(next.id), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ revision: SCHED.revision, schedule: next }),
    });
    if (!res.ok) throw await apiError(res);
    document.getElementById("block-dialog").close();
    document.getElementById("plan-dialog").close();
    await loadSchedules();
    toast(t("sched.saved"), "ok");
  } catch (e) {
    toast(e.message, "err");
  }
}

// ---------- schedule (plan) dialog ----------

let planCtx = null;

function openPlanDialog(s) {
  const isNew = !s;
  const copy = s
    ? JSON.parse(JSON.stringify(s))
    : { name: "", enabled: true, priority: nextPriority(), targets: SCHED.device ? [{ device_id: SCHED.device }] : [], blocks: [] };
  planCtx = { original: s, copy, isNew };
  document.getElementById("plan-title").textContent = isNew ? t("sched.newSchedule") : copy.name;
  document.getElementById("plan-delete").hidden = isNew;

  const body = document.getElementById("plan-body");
  body.innerHTML = "";

  const nameRow = el("div", "field-row");
  nameRow.appendChild(el("label", null, t("sched.name")));
  const name = el("input", "field");
  name.type = "text";
  name.value = copy.name;
  name.addEventListener("input", () => (copy.name = name.value));
  nameRow.appendChild(name);
  body.appendChild(nameRow);

  const prioRow = el("div", "field-row");
  prioRow.appendChild(el("label", null, t("sched.priority")));
  const prio = el("input", "field");
  prio.type = "number";
  prio.value = String(copy.priority);
  prio.addEventListener("input", () => (copy.priority = parseInt(prio.value, 10) || 0));
  prioRow.appendChild(prio);
  body.appendChild(prioRow);

  const devRow = el("div", "field-row");
  devRow.appendChild(el("label", null, t("sched.targets")));
  const devs = el("div", "inline");
  for (const d of SCHED.devices) {
    const c = el("button", "chip", d.name || d.id);
    c.type = "button";
    const has = () => (copy.targets || []).some((x) => x.device_id === d.id);
    c.setAttribute("aria-pressed", String(has()));
    c.addEventListener("click", () => {
      copy.targets = has()
        ? copy.targets.filter((x) => x.device_id !== d.id)
        : [...(copy.targets || []), { device_id: d.id }];
      c.setAttribute("aria-pressed", String(has()));
    });
    devs.appendChild(c);
  }
  devRow.appendChild(devs);
  body.appendChild(devRow);

  if (!isNew) {
    const idRow = el("div", "field-row");
    idRow.appendChild(el("label", null, t("sched.entityId")));
    const id = el("input", "field");
    id.type = "text";
    id.readOnly = true;
    id.value = "switch.daikin_schedule_" + copy.id;
    id.title = t("sched.entityIdHint");
    idRow.appendChild(id);
    body.appendChild(idRow);
  }

  document.getElementById("plan-dialog").showModal();
}

function nextPriority() {
  const max = SCHED.schedules.reduce((m, s) => Math.max(m, s.priority), -5);
  return max + 5;
}

async function savePlan() {
  if (!planCtx) return;
  const { copy, isNew } = planCtx;
  if (!copy.name.trim()) {
    toast(t("sched.err.validation_failed"), "err");
    return;
  }
  if (isNew) {
    try {
      const res = await fetch("api/schedules", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ revision: SCHED.revision, schedule: copy }),
      });
      if (!res.ok) throw await apiError(res);
      document.getElementById("plan-dialog").close();
      await loadSchedules();
      toast(t("sched.saved"), "ok");
    } catch (e) {
      toast(e.message, "err");
    }
    return;
  }
  await putSchedule(copy);
}

async function deletePlan() {
  if (!planCtx || planCtx.isNew) return;
  try {
    const res = await fetch("api/schedules/" + encodeURIComponent(planCtx.copy.id), { method: "DELETE" });
    if (!res.ok) throw await apiError(res);
    document.getElementById("plan-dialog").close();
    await loadSchedules();
  } catch (e) {
    toast(e.message, "err");
  }
}

// ---------- wiring ----------

function initSchedules(config) {
  if (!config || !config.schedule_enable) return;
  for (const id of ["sec-schedules"]) document.getElementById(id).hidden = false;
  document.querySelector('.nav-item[data-target="sec-schedules"]').hidden = false;

  document.getElementById("sched-new").addEventListener("click", () => openPlanDialog(null));
  document.getElementById("dlg-close").addEventListener("click", () => document.getElementById("block-dialog").close());
  document.getElementById("dlg-cancel").addEventListener("click", () => document.getElementById("block-dialog").close());
  document.getElementById("dlg-save").addEventListener("click", saveBlock);
  document.getElementById("dlg-delete").addEventListener("click", deleteBlock);
  document.getElementById("plan-close").addEventListener("click", () => document.getElementById("plan-dialog").close());
  document.getElementById("plan-cancel").addEventListener("click", () => document.getElementById("plan-dialog").close());
  document.getElementById("plan-save").addEventListener("click", savePlan);
  document.getElementById("plan-delete").addEventListener("click", deletePlan);

  loadSchedules();
}
