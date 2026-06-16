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
