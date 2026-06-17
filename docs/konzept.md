# Umsetzungskonzept: go-daikin2mqtt

Standalone Go-Daemon, der Daikin-Klimaanlagen über die **ONECTA Cloud API**
ausliest und steuert und in MQTT (inkl. Home Assistant Discovery) einbindet.

Architektur und Konventionen orientieren sich eng an
[`go-mtec2mqtt`](../../go-mtec2mqtt); die Daikin-/Cloud-Spezifika sind aus der
Home-Assistant-Integration [`daikin_onecta`](../../daikin_onecta) sowie der
OpenAPI-Spezifikation [`docs/api/onecta-cloud-api-openapi.json`](api/onecta-cloud-api-openapi.json)
abgeleitet. Wiederverwendbarer Code darf aus
[`go-mtec2mqtt`](../../go-mtec2mqtt) und [`openccu-loom`](../../openccu-loom)
übernommen werden (siehe §13).

---

## 1. Getroffene Entscheidungen (verbindlich)

| Thema | Entscheidung |
|---|---|
| **Auth-Flow** | OAuth2 Authorization Code Flow (PKCE) mit **Callback-Server im Daemon** beim ersten Start; danach reiner Refresh-Token-Betrieb. **In die Diagnose-UI integriert** (gemeinsamer HTTP-Server). |
| **Richtung** | **Bidirektional**: lesen (GET) + steuern (PATCH) via MQTT `/set`-Topics. |
| **Home Assistant** | **MQTT Discovery** für `climate`, `sensor`, `number`, `select`, `switch`, `binary_sensor` (abschaltbar). |
| **Gerätetypen** | `climateControl`, `domesticHotWaterTank`/`FlowThrough`, `gateway`/`indoorUnit`/`outdoorUnit`/`userInterface` (Info), Energie-/Verbrauchsdaten. |
| **Mapping** | **Kuratierter YAML-Katalog** `characteristics.yaml` analog `registers.yaml`. |
| **Token-Store** | Datei mit **konfigurierbarem Pfad** (`TOKEN_STORE_PATH`), Default `$XDG_CONFIG_HOME/daikin2mqtt/token-store.json`, `chmod 0600`. |
| **Diagnose-CLI** | Separates Binary **`daikin2mqtt-util`** (auth, devices, points, set, ratelimit, catalog-check). |
| **Diagnose-UI** | Optionale eingebettete Web-UI (Default **aus**, Bind `127.0.0.1`, opt. Basic-Auth). **OAuth integriert**; Scope: Auth-Status/Connect, Geräte-/Datenpunkt-Browser, manueller PATCH-Test, Rate-Limit/Status. |
| **i18n** | Voll lokalisiert wie go-mtec2mqtt: `LANGUAGE` (en/de, Default en), `name`/`name_de` + Enum-Labels im Katalog, JSON-Bundles für die Web-UI, Fallback auf Englisch. |
| **Deployment** | Binary + curl\|bash-Installer + systemd, Distroless-Docker (multi-arch via GHCR) **und Home-Assistant-Addon**. |

Übernommene mtec2mqtt-Konventionen: minimaler Dependency-Tree, `log/slog`,
flaches YAML-Schema + ENV-Overrides + Defaults + Validate, Coordinator-Pattern mit
`errgroup` + `context`, narrow Interfaces für Tests, Distroless-Docker, Makefile +
GitHub Actions, SPDX-Header, MIT-Lizenz.

---

## 2. Modul & Verzeichnisstruktur

Modul: `github.com/SukramJ/go-daikin2mqtt`, Go ≥ 1.26, `CGO_ENABLED=0`.

```
go-daikin2mqtt/
├── cmd/
│   ├── daikin2mqtt/        main.go        # Daemon: Config, Wiring, Signal-Handling
│   └── daikin2mqtt-util/   main.go        # Diagnose-/Auth-CLI
├── internal/
│   ├── config/                            # YAML + ENV + Defaults + Validate (mtec-Stil)
│   ├── daikin/
│   │   ├── auth/                          # OAuth2: oauth.go, callback.go, store.go, TokenSource
│   │   ├── client/                        # client.go, ratelimit.go, resilience.go
│   │   └── model/                         # device.go, parse.go
│   ├── catalog/                           # catalog.go, load.go, match.go, localize.go
│   ├── mqtt/                              # Pure-Go MQTT 3.1.1 (aus openccu-loom/mtec2mqtt)
│   ├── hass/                              # Discovery-Payloads (climate, sensor, …)
│   ├── coordinator/                       # coordinator.go, poll.go, process.go, write.go
│   ├── web/                               # eingebettete Diagnose-UI + OAuth-Callback
│   │   ├── backend.go  auth_handler.go    #   API + /callback (gemeinsamer Server)
│   │   └── static/                        #   go:embed SPA
│   │       ├── index.html  app.js  style.css
│   │       └── i18n/{en,de}.json          #   UI-Übersetzungen
│   ├── state/  version/  shutdown/  resilience/  health/
├── characteristics.yaml                   # Kuratierter Mapping-Katalog (name/name_de, …)
├── config-template.yaml
├── docs/
│   ├── konzept.md
│   └── api/onecta-cloud-api-openapi.json
├── script/
│   ├── install.sh                         # curl|bash Installer (Wizard, systemd)
│   ├── extract-release-notes.sh
│   └── run.sh                             # HA-Addon Startscript (bashio)
├── addon/                                 # Home-Assistant-Addon
│   ├── config.yaml  build.yaml  Dockerfile  README.md  icon.png
├── .github/workflows/{ci,release-on-tag,docker-build-push,codeql}.yml
├── .githooks/  .golangci.yaml
├── Makefile  Dockerfile  changelog.md  README.md  LICENSE
```

---

## 3. Daikin Cloud — Domänenwissen

### 3.1 Endpunkte
- **Auth:** `https://idp.onecta.daikineurope.com/v1/oidc/authorize` (Authorize),
  `.../v1/oidc/token` (Token + Refresh). Scopes:
  `openid onecta:basic.integration offline_access`.
- **API-Basis:** `https://api.onecta.daikineurope.com`
  - `GET  /v1/gateway-devices` — alle Geräte inkl. managementPoints.
  - `PATCH /v1/gateway-devices/{id}/management-points/{embeddedId}/characteristics/{dataPoint}`
    — Wert setzen.

### 3.2 Datenmodell
```
Device(id, deviceModel)
 └─ managementPoints[]            embeddedId, managementPointType, category(primary|secondary)
     └─ characteristics           z.B. onOffMode, operationMode, temperatureControl, sensoryData
         └─ Value-Wrapper         { value, settable, minValue, maxValue, stepValue, values[] }
```
- Setzen: `PATCH` mit Body `{"value": <v>}`, bei verschachtelten Feldern zusätzlich
  `"path": "operationModes.heating.setpoints.roomTemperature"`.
- `settable` + `values`/`min/max/step` steuern HA-Plattform-Wahl
  (switch/select/number) und Validierung.

### 3.3 Rate-Limit & Stale-Data (kritisch)
- Header `X-RateLimit-Limit/Remaining-{minute,day}`, `retry-after`, `ratelimit-reset`.
- **`_cloud_lock`**: nur ein Cloud-Request gleichzeitig (serialisiert GET/PATCH).
- Nach PATCH **`scan_ignore` (~30 s)** keine GETs (Cloud liefert sonst stale data).
- Bei `remaining_day == 0` Polling bis `retry-after + 60 s` pausieren.
- 429 ist **kein** CircuitBreaker-Failure (erwartet); 5xx schon.
- Adaptive Poll-Intervalle: Tag (07–22 Uhr) ~10 min, Nacht ~30 min (konfigurierbar).
- **Retry**: GETs 3× mit Exponential Backoff (1→2→4 s, Jitter); Auth-/429-Fehler ohne Retry.

---

## 4. Authentifizierung (OAuth2, in die UI integriert)

```
Start → Token-Store laden
 ├─ gültiger refresh_token? → TokenSource (auto-refresh vor Ablauf) → §5
 └─ kein/abgelaufener Token:
     1. PKCE (code_verifier/challenge) + state erzeugen
     2. HTTP-Server bereitstellen: bei aktiver Web-UI deren Server (dauerhaft),
        sonst temporärer Callback-Server auf AUTH_CALLBACK_BIND
     3. Authorize-URL loggen/öffnen (in UI: „Mit Daikin verbinden"-Button)
     4. Daikin redirectet Browser → /callback?code=…&state=…  (state prüfen!)
     5. Code → Token (POST /token, grant_type=authorization_code)
     6. Token-Store atomar (0600) schreiben
```
- **TokenSource**: kapselt access/refresh, erneuert proaktiv vor `expires_in`,
  persistiert rotierte Refresh-Tokens. Bei `invalid_grant`/401 → Re-Auth-Hinweis;
  in der UI per Klick neu autorisierbar, sonst via `daikin2mqtt-util auth`.
- `client_id`/`client_secret` aus Config/ENV (nie im Token-Store).

### 4.1 Netzwerk & Deployment (keine Portfreigaben nötig)

**Wichtig:** Der Callback-Server braucht **keine eingehende Portfreigabe / kein
Port-Forwarding** — auch nicht in einem frischen Privatnetzwerk. Im Authorization
Code Flow verbindet sich **Daikin nie** zum Callback-Server:

1. Browser öffnet die Daikin-Login-URL (ausgehend → Internet).
2. Nach Login schickt Daikin dem **Browser** eine HTTP-302-Weiterleitung auf die
   `redirect_uri` (`…/callback?code=…&state=…`).
3. Der **Browser** ruft diese URL auf → er verbindet sich lokal zum Callback-Server.

Der Callback-Server muss also nur **vom Browser** erreichbar sein, nicht aus dem
Internet. Der Daemon braucht ausschließlich **ausgehenden** Zugriff auf
`idp.onecta…` und `api.onecta…`.

| Szenario | Funktioniert | Hinweis |
|---|---|---|
| Browser auf derselben Maschine wie der Daemon | ja, ohne alles | `redirect_uri = http://localhost:8080/callback` |
| Browser auf anderem Gerät im selben LAN | ja | LAN-IP statt localhost, z.B. `http://192.168.x.y:8080/callback` |
| Daemon headless (Server/Container) | mit Einschränkung | SSH-Tunnel (`ssh -L 8080:localhost:8080`), LAN-IP, oder `daikin2mqtt-util auth` |
| Home-Assistant-Addon | ja | UI via HA-Ingress; Auth-Button in der Ingress-Oberfläche |

Zwei netzwerkunabhängige Voraussetzungen (am Daikin Developer Portal zu
**verifizieren**, noch nicht bestätigt):

1. **Redirect-URI-Whitelist:** Die `redirect_uri` muss im Developer Portal exakt
   registriert sein; nur vorab eingetragene URIs sind nutzbar.
2. **HTTP vs. HTTPS:** Manche Provider verlangen HTTPS-Redirects, erlauben aber
   `http://localhost`/Loopback als Ausnahme (RFC 8252). Für Daikin zu prüfen.

**Headless/Container-Empfehlung:** Erst-Auth einmalig via UI/`daikin2mqtt-util auth`
(lokal oder per SSH-Tunnel), danach Token-Store als Volume mounten — der Daemon läuft
dann rein über den Refresh-Token, komplett ohne eingehende Ports.

---

## 5. Coordinator (Orchestrierung)

```
Coordinator.Run(ctx)  (errgroup)
 ├─ pollLoop(devices)        adaptives Intervall; GET → merge → process → publish
 ├─ tokenWatcher             proaktiver Refresh, Re-Auth-Detection
 ├─ mqttLifecycle            Reconnect/Backoff (aus mtec2mqtt/openccu-loom)
 ├─ drainWriteQueue          MQTT /set → Validierung → PATCH → scan_ignore setzen
 └─ rateLimitGate            pausiert Poll bei day-Budget 0
```
Ablauf je Poll: `client.GetDevices` → JSON in `model` parsen → Katalog-Match →
Werte skalieren/normalisieren → MQTT publish (`state`) → optional Discovery-Refresh.
Write-Pfad: Topic→characteristic auflösen, `settable`/`values`/`min-max` prüfen,
PATCH absetzen, `scan_ignore` aktivieren, optimistisch State spiegeln.

---

## 6. Diagnose-UI (optional, mit integriertem OAuth)

Eingebettete SPA (`go:embed`), Default **aus** (`WEB_ENABLE=false`), Bind
`127.0.0.1:8080`, optionaler HTTP-Basic-Auth (`WEB_USER`/`WEB_PASSWORD`) — wie
go-mtec2mqtt. Derselbe HTTP-Server bedient UI **und** den OAuth-`/callback`.

**Funktionsumfang:**
- **Auth-Status & Connect:** Token-Status (gültig/Ablauf/Re-Auth), „Mit Daikin
  verbinden"-Button, Erfolgs-/Fehlerseite nach Callback.
- **Geräte-/Datenpunkt-Browser:** alle Geräte, managementPoints, characteristics
  (value/settable/min/max/values) live.
- **Manueller PATCH-Test:** einzelne Characteristic interaktiv setzen, validiert
  gegen `settable`/`values`/`min-max`.
- **Rate-Limit & Status:** Minute-/Tagesbudget, `scan_ignore`-Status,
  MQTT-Verbindungsstatus, letzte Poll-Zeit, CircuitBreaker-Zustand.

**API (Backend):** `/api/config` (inkl. `language`), `/api/auth/status`,
`/api/auth/start` + `/callback`, `/api/devices`, `/api/patch`, `/api/status`,
SSE/Polling für Live-Updates. Texte lokalisiert (§8); UI lädt `i18n/{lang}.json`.

**HA-Addon:** Die UI wird über **HA-Ingress** eingebunden — der Auth-Button
funktioniert direkt in der Ingress-Oberfläche, ohne offene Ports.

---

## 7. MQTT-Topics & HA-Discovery

**State/Command-Schema:**
```
daikin/<deviceId>/<embeddedId>/<characteristic>/state      # publish
daikin/<deviceId>/<embeddedId>/<characteristic>/set        # subscribe (settable)
daikin/bridge/status                                        # LWT online/offline
```
**HA-Discovery** (retained, abschaltbar): pro Gerät eine `device`-Gruppierung;
`climate`-Entity für `climateControl` (hvac_modes ↔ operationMode, temp-Setpoint,
fan_mode); `sensor`/`binary_sensor` für read-only; `number`/`select`/`switch` für
settable Felder. Energiezähler mit `device_class=energy`,
`state_class=total_increasing`. Entity-`name` lokalisiert (§8); MQTT-Topics/IDs
bleiben sprachunabhängig.

HVAC-Mapping (aus daikin_onecta): `heating↔heat, cooling↔cool, auto↔heat_cool,
dry↔dry, fanOnly↔fan_only, off↔off`.

---

## 8. Internationalisierung (i18n) — Muster aus go-mtec2mqtt

- **Sprachen:** `en` (Default/Fallback), `de`. Config `LANGUAGE` (ENV
  `DAIKIN_LANGUAGE`), validiert gegen Whitelist `{en, de}`.
- **Katalog-Übersetzung:** im `characteristics.yaml` Felder `name` (kanonisch
  englisch), `name_de` (optional) sowie Enum-Labels `values`/`values_de`. Im
  Go-Modell `LocalizedName(lang)` und `LocalizedValues(lang)` mit **Pro-Code-
  Fallback** auf Englisch. Reverse-Lookup `CodeForLabel` akzeptiert beide Sprachen
  (für Writes aus UI/HA).
- **Lokalisiert:** Katalog-/Entity-Namen, Enum-Labels, HA-Discovery-`name`,
  Web-UI-Texte. **Nicht** lokalisiert: MQTT-Topics, entity_ids, characteristic-Keys
  und — als Regel — settable Kommando-**Werte** (siehe Climate-Ausnahme unten).
- **Climate-Dropdowns (Ausnahme):** Die kombinierte `climate`-Entität führt
  fan/swing/preset als synthetische Optionslisten (aus `fanControl`/`powerfulMode`,
  nicht aus dem Katalog). HAs MQTT-Climate-Plattform kennt — anders als ein
  natives Integration-Translation-File (vgl. `daikin_onecta`,
  `openccu-loom`) — **kein** separates Label-Feld: der Listeneintrag ist
  Anzeige *und* Kommando-Wert zugleich. Damit die Dropdowns im DE-Modus deutsch
  erscheinen, emittiert `internal/coordinator/climate.go` daher bewusst das
  **deutsche Label als Wert** (z. B. `swing → "Schwingen"`, `windnice →
  "Komfort Luftstrom"`, Strings gespiegelt aus `daikin_onecta/de.json`) und
  bildet es beim Schreiben via `canonicalAux` auf den rohen Daikin-Wert zurück
  (inkl. mixed-case `windNice`/`floorHeatingAirflow`). Numerische Lüfterstufen
  bleiben Zahlen. Bei `LANGUAGE=en` bleiben die Werte sprachneutral roh. Dies ist
  die einzige Stelle, an der ein Kommando-Wert lokalisiert wird — der Sonderfall
  der fehlenden HA-Label/Wert-Trennung über MQTT.
- **Web-UI:** flache JSON-Bundles `internal/web/static/i18n/{en,de}.json`
  (Dot-Notation-Keys); Frontend `t(key)`/`tf(key, params)`, `loadI18n(lang)`,
  `data-i18n`-Attribute. Sprache kommt aus `/api/config` (Daemon-Konfiguration).
  Register-Namen/Enums liefert das Backend bereits lokalisiert.
- **Fallback:** fehlende `name_de`/Enum-DE → Englisch; unbekannte Sprache →
  Englisch; fehlender UI-Key → roher Key.
- **Tests:** Name-/Enum-Fallback, `CodeForLabel` (beide Sprachen),
  Config-Sprach-Validierung, UI serviert beide Bundles.

---

## 9. Kuratierter Katalog (`characteristics.yaml`)

Pro Characteristic ein Eintrag, der API-Feld → MQTT/HA abbildet:
```yaml
- match:                       # worauf der Eintrag passt
    managementPointType: climateControl
    characteristic: roomTemperature
  topic: room_temperature
  name: Room temperature
  name_de: Raumtemperatur
  platform: sensor
  device_class: temperature
  unit: "°C"
  state_class: measurement
  settable: false

- match:
    managementPointType: climateControl
    characteristic: onOffMode
  topic: power
  name: Power
  name_de: Betrieb
  platform: switch
  settable: true
  payload: { on: "on", off: "off" }
```
Felder: `match`, `topic`, `name`/`name_de`, `platform`, `device_class`, `unit`,
`state_class`, `settable`, `values`/`values_de`/`payload`, optional `path` (nested
PATCH), `scale`/`precision`, `enabled`. Nicht gelistete Felder werden ignoriert
(deterministische, kuratierte Abdeckung). Loader validiert Eindeutigkeit der
`match`-Schlüssel und Plattform-Pflichtfelder. `daikin2mqtt-util catalog-check`
meldet ungemappte Live-Felder.

---

## 10. Konfiguration (flaches YAML + ENV `DAIKIN_*`)

```yaml
# Daikin Cloud
DAIKIN_CLIENT_ID: ""
DAIKIN_CLIENT_SECRET: ""
DAIKIN_REDIRECT_URI: "http://localhost:8080/callback"
AUTH_CALLBACK_BIND: "127.0.0.1:8080"
TOKEN_STORE_PATH: ""            # leer → XDG-Default

# Polling / Rate-Limit
REFRESH_DAY_INTERVAL: 600       # s, 07–22 Uhr
REFRESH_NIGHT_INTERVAL: 1800    # s, 22–07 Uhr
SCAN_IGNORE: 30                 # s nach PATCH

# MQTT
MQTT_SERVER: localhost
MQTT_PORT: 1883
MQTT_LOGIN: ""
MQTT_PASSWORD: ""
MQTT_TOPIC: daikin

# Home Assistant
HASS_ENABLE: true
HASS_BASE_TOPIC: homeassistant

# Diagnose-UI (optional)
WEB_ENABLE: false
WEB_BIND: "127.0.0.1:8080"
WEB_USER: ""
WEB_PASSWORD: ""

# Misc
LANGUAGE: en                    # en | de
DEBUG: false
```
Loader-Pipeline (mtec-Stil): Datei → ENV-Override (Coercion) → Defaults → Validate.
Pflicht: `DAIKIN_CLIENT_ID/SECRET`, `MQTT_SERVER`. Hinweis: `WEB_BIND` und
`AUTH_CALLBACK_BIND` sollten konsistent zur registrierten `redirect_uri` sein.

---

## 11. `daikin2mqtt-util` (Diagnose-CLI)

- `auth` — interaktiver OAuth2-Flow (Callback-Server), schreibt Token-Store.
- `devices` — `GET /v1/gateway-devices`, formatierter Dump (+ `--raw`).
- `points <deviceId>` — managementPoints/characteristics mit settable/min/max/values.
- `set <deviceId> <embeddedId> <characteristic> <value> [--path …]` — Test-PATCH.
- `ratelimit` — Minute-/Tagesbudget aus letzter Antwort.
- `catalog-check` — prüft `characteristics.yaml` gegen Live-Dump (ungemappte Felder).

---

## 12. Deployment & Release

Deployment-Setup wird aus go-mtec2mqtt übernommen und um ein **Home-Assistant-Addon**
sowie **Multi-Arch-Docker-Push nach GHCR** erweitert.

### 12.1 Binary + Installer + systemd
- `script/install.sh` (curl\|bash, root, Linux): Arch-Detection, Download neuester
  Release über GitHub-API, SHA256-Verifikation, Service-User `daikin`, Binaries nach
  `/opt/go-daikin2mqtt`, Assets (`characteristics.yaml`, Template, README, LICENSE),
  Symlinks, **3-Fragen-Wizard** (CLIENT_ID, MQTT_SERVER, HASS_ENABLE), gehärtete
  systemd-Unit, enable+start, Healthcheck. Upgrade-Backup `*.bak.<ts>`.
- systemd-Unit gehärtet (NoNewPrivileges, ProtectSystem=strict, ProtectHome,
  PrivateTmp, RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6, leeres
  CapabilityBoundingSet), `Restart=on-failure`. ExecStart mit `--config` +
  `--catalog`.

### 12.2 Docker (Distroless, Multi-Arch GHCR)
- `Dockerfile`: Stage 1 `golang:1.26-alpine` (`CGO_ENABLED=0`, ldflags), Stage 2
  `gcr.io/distroless/static-debian12:nonroot`; beide Binaries + `characteristics.yaml`
  + Template; `VOLUME ["/config"]`, `XDG_CONFIG_HOME=/config`, `EXPOSE 8080`,
  `USER nonroot`.
- **Neu:** `.github/workflows/docker-build-push.yml` mit `docker/buildx`
  (`linux/amd64,arm64,armv7`) → Push nach `ghcr.io/sukramj/go-daikin2mqtt`
  (`:latest`, `:<version>`).

### 12.3 GitHub Actions
- `ci.yml`: lint (vet, gofumpt) + test (OS-Matrix, `-race`) + build (`--version`).
- `release-on-tag.yml`: `make release` (Cross-Compile `linux/amd64,arm64`,
  `darwin/arm64`), TAR.GZ + `SHA256SUMS`, Release-Notes aus `changelog.md`
  (`extract-release-notes.sh`), `softprops/action-gh-release`.
- `docker-build-push.yml` (neu, s.o.), `codeql.yml`, `dependabot.yml`.
- Versionierung via git-Tag → ldflags (`internal/version`); `changelog.md` mit
  `# Version X.Y.Z (YYYY-MM-DD)`-Headern.

### 12.4 Home-Assistant-Addon (`addon/`)
- `config.yaml`: Addon-Manifest — `slug`, `arch: [amd64, aarch64, armv7]`,
  `image: ghcr.io/sukramj/go-daikin2mqtt` (**ein** Multi-Arch-Manifest, kein
  `-{arch}`-Suffix; Docker wählt die Arch), `init: false`,
  **`ingress: true`** (Diagnose-UI inkl. OAuth-Button als HA-Panel),
  `ports`/`ports_description` optional, `services: ["mqtt:want"]`
  (MQTT-Discovery vom Supervisor), `options`/`schema` für
  `DAIKIN_CLIENT_ID/SECRET`, `redirect_uri` (HTTPS, da Daikin `http` ablehnt),
  MQTT, `LANGUAGE` etc. Repo-Root trägt zusätzlich `repository.yaml`, damit die
  Repo-URL als Add-on-Store hinzugefügt werden kann.
- `Dockerfile`: `FROM ghcr.io/home-assistant/{arch}-base`, kopiert das statische
  Daemon-Binary + `characteristics.yaml`, `run.sh` als Entrypoint.
- `script/run.sh`: liest `/data/options.json` via **bashio**, mappt Optionen auf
  `DAIKIN_*`-ENV, persistiert Token-Store unter `/data`, `exec` des Daemons.
- `build.yaml`: `build_from` je Arch. Token-Store liegt im Addon-`/data` (persistent
  über Updates). MQTT-Zugangsdaten bevorzugt über den HA-MQTT-Service.

---

## 13. Code-Wiederverwendung

Bevorzugt 1:1 bzw. leicht angepasst übernehmen:

| Quelle | Was |
|---|---|
| `openccu-loom` / `go-mtec2mqtt` `internal/mqtt` | Pure-Go MQTT 3.1.1 (Client, TCP-Adapter, Lifecycle/Reconnect, Protocol-Codec). |
| `go-mtec2mqtt` `internal/config` | YAML+ENV+Defaults+Validate-Pipeline, `Locate()`, Duration-Helper. |
| `go-mtec2mqtt` `internal/coordinator` | Poll-Loop-/Ticker-/Watchdog-/Write-Queue-Muster, errgroup-Wiring. |
| `go-mtec2mqtt` `internal/hass` | Discovery-Payload-Builder (Plattform-Mapping, Device-Gruppierung, LWT). |
| `go-mtec2mqtt` `internal/web` + `static/` | Diagnose-UI-Gerüst, SSE/Live-Updates, i18n-Bundles, `app.js`. |
| `go-mtec2mqtt` `internal/{version,resilience,health,shutdown,state}` | Hilfspakete (Backoff, CircuitBreaker, graceful shutdown). |
| `go-mtec2mqtt` Build/Deploy | `Makefile`, `Dockerfile`, `.github/workflows/*`, `script/install.sh`, `extract-release-notes.sh`, `.golangci.yaml`, `.githooks`. |

Neu zu schreiben: `internal/daikin/*` (auth/client/model), `internal/catalog`,
`characteristics.yaml`, OAuth-Erweiterung der UI, HA-Addon (`addon/`, `run.sh`),
`docker-build-push.yml`.

---

## 14. Qualität, Tests, Build

- **Tests**: table-driven; Stub-`CloudClient`/`MQTT`/`TokenSource` (narrow Interfaces);
  Golden-File-Tests gegen JSON-Fixtures aus `daikin_onecta/tests/fixtures`
  (climate, altherma, hot water) für Parser + Katalog-Match + Discovery-Payloads;
  i18n-Fallback-Tests; Rate-Limit-/scan_ignore-Logik mit injizierbarer Clock;
  `go test -race`.
- **Resilience**: Retry+Backoff, CircuitBreaker, cloud-lock, graceful degradation
  (MQTT down → Poll läuft weiter; Re-Auth nötig → klarer Log, kein Crash).
- **Build/Release**: Makefile (build/fmt/lint/test/release/docker), Distroless-Docker,
  GitHub Actions (CI, release-on-tag, docker-build-push, CodeQL), ldflags-Version,
  `.golangci.yaml`, SPDX-Header, MIT.

---

## 15. Vorgeschlagene Umsetzungsreihenfolge

1. Projekt-Gerüst (go.mod, cmd-Stubs, `config`, `version`, Makefile, CI) — Build/Deploy-
   Dateien aus go-mtec2mqtt übernehmen.
2. MQTT-Paket aus openccu-loom/go-mtec2mqtt übernehmen/anpassen.
3. `daikin/auth` (PKCE, Callback-Server, Token-Store, TokenSource) + `util auth`.
4. `daikin/client` (doBearerRequest, Rate-Limit, Resilience) + `util devices/points`.
5. `daikin/model` Parser + Golden-Tests gegen Fixtures.
6. `catalog` + `characteristics.yaml` (climateControl zuerst) inkl. i18n + `catalog-check`.
7. `coordinator` Poll/Process/Publish (read-only Ende-zu-Ende).
8. `hass` Discovery (sensor/binary_sensor → climate → number/select/switch), lokalisiert.
9. Write-Pfad (`/set` → PATCH, scan_ignore, optimistic state).
10. Diagnose-UI mit integriertem OAuth (Auth-Status, Browser, PATCH-Test, Status) + i18n.
11. Warmwasser + Energie + Hardware-Info ergänzen.
12. Deployment: Docker-Multi-Arch-Push, HA-Addon (`addon/`, `run.sh`, Ingress), README, Release.
```
