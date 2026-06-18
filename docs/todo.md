# Implementierungs-Tracking: go-daikin2mqtt

Trackbare Aufgabenliste zum [Umsetzungskonzept](konzept.md). Abhaken mit `[x]`.
Reihenfolge entspricht den Meilensteinen (M0–M12); §-Verweise zeigen auf das Konzept.

Legende: `[ ]` offen · `[~]` in Arbeit · `[x]` erledigt · `[!]` blockiert/zu klären

---

## M0 — Vorab zu klären / verifizieren
- [ ] Daikin Developer Portal: `client_id`/`client_secret` beschaffen
- [ ] Redirect-URI-Whitelist klären (welche `redirect_uri` registrierbar) — §4.1
- [ ] HTTP-Loopback-Redirect vs. HTTPS-Pflicht prüfen (RFC 8252) — §4.1
- [ ] Modulpfad bestätigen: `github.com/SukramJ/go-daikin2mqtt` — §2
- [ ] GHCR-Image-Namen/Owner bestätigen — §12.2

## M1 — Projekt-Gerüst & Build/Deploy-Basis (§2, §12, §14)
- [x] `go.mod` (Go 1.26.3), Verzeichnisstruktur, SPDX/MIT
- [x] `cmd/daikin2mqtt` + `cmd/daikin2mqtt-util` Stubs
- [x] `internal/version` (ldflags-Injektion)
- [x] `internal/config` adaptiert (YAML+ENV `DAIKIN_*`+Defaults+Validate, `Locate()`, Token-Store-Pfad) + Tests
- [x] `config-template.yaml` mit Daikin-Keys (§10)
- [x] `Makefile`, `Dockerfile`, `.golangci.yaml`, `.githooks`, `.dockerignore`
- [x] `.github/workflows/{ci,release-on-tag,docker-build-push,codeql}.yml` + `dependabot.yml`
- [x] `script/{extract-release-notes,install}.sh`, `changelog.md`, `LICENSE`, `README`, `.gitignore`
- [x] Lokal verifiziert: `gofmt`/`go vet`/`go build`/`go test -race` grün, `make build` ok (CI sollte grün sein)

## M2 — MQTT (§13)
- [x] `internal/mqtt` (+ `protocol/`) aus go-mtec2mqtt übernommen, Importpfade angepasst
- [x] Lifecycle/Reconnect-Backoff enthalten
- [x] Tests (Publish/Subscribe, Lifecycle) grün

## M3 — OAuth2 / Auth (§4)
- [x] `internal/daikin/auth`: PKCE (code_verifier/challenge) + state
- [x] Callback-Server (temporär + UI-integrierbar via `Config.Authorize`)
- [x] Code-Exchange + Refresh (`/v1/oidc/token`)
- [x] `TokenSource` (proaktiver Refresh + skew, Re-Auth-Detection → `ErrReauthRequired`)
- [x] Token-Store (JSON, 0600, atomic write, konfigurierbarer Pfad/XDG)
- [ ] `daikin2mqtt-util auth` (Paket fertig; CLI-Subcommand noch zu verdrahten)
- [x] Tests (Token-Refresh, invalid_grant, state-Validierung, Store-Roundtrip, voller Flow)

## M4 — Cloud-Client (§3, §13) — LIVE verifiziert
- [x] `internal/daikin/client`: GetDevices/Patch, Header, Base-URL
- [x] Rate-Limit-Header (`X-RateLimit-*`/`retry-after`/`ratelimit-reset`) parsen
- [x] Retry+Backoff, CircuitBreaker, cloud-lock
- [x] `scan_ignore`-Logik nach PATCH (injizierbare Clock)
- [x] **gzip-Bug gefixt** (kein manuelles Accept-Encoding) + Regressionstest
- [x] `daikin2mqtt-util` devices/points/ratelimit/set + auth (live gegen Cloud getestet)
- [x] Tests (Rate-Limit, CircuitBreaker, scan_ignore, gzip)

## M5 — Datenmodell (§3.2) — LIVE verifiziert
- [x] `internal/daikin/model`: Device/ManagementPoint/DataPoint/Characteristic + Parser + Accessoren
- [x] Golden-File-Tests gegen Fixtures (climate, altherma, gas)
- [ ] JSON-Merge inkrementeller Updates (optional, später)

## M6 — Katalog + i18n (§8, §9) — LIVE verifiziert
- [x] `internal/catalog`: Loader, Validierung (Topic-Eindeutigkeit/Pflichtfelder), ByTopic/EntriesForType
- [x] `LocalizedName`/`LocalizedLabel`/`CodeForLabel` (Pro-Item-Fallback)
- [x] `internal/process`: Auflösung inkl. verschachtelter `value_path` + `{mode}`-Ersetzung
- [x] `characteristics.yaml`: climateControl + gateway/indoor/outdoor (19 Einträge, name/name_de)
- [x] i18n-/Process-Tests
- [x] `daikin2mqtt-util catalog-check` (live verifiziert)

## M7 — Coordinator (§5) — LIVE verifiziert (57 Punkte / 4 Geräte publiziert)
- [x] Poll-Loop (adaptiv Tag/Nacht), Fehlerbehandlung (scan_ignore/rate-limit/reauth)
- [x] Process → MQTT-Publish (`state`, retained), Bridge-LWT online/offline
- [x] Unit-Tests für Coordinator (Stub-Client/MQTT): poll/publish, write, select-i18n, Fehlerfälle

## M8 — HA-Discovery (§7, §8) — LIVE verifiziert
- [x] `internal/hass`: Discovery-Builder (Device-Gruppierung, Availability/LWT)
- [x] sensor/binary_sensor/switch/select/number, lokalisierte Entity-Namen
- [x] **englische `entity_id` (via `default_entity_id`=`<domain>.<uid>`) + lokalisierter `name`** + Unit-Tests
- [x] **kombinierte `climate`-Entity** inkl. hvac_mode, temperature, fan_mode (fixed→1..N),
      swing/swing_horizontal, preset (none/boost) — synthetische Topics, Suppression der Einzel-Controls
- [x] **erweiterte device_info** (model/sw_version/serial/mac-connections/configuration_url)
- [x] **Gateway & Outdoor als eigene HA-Geräte** (via_device / serial-Dedup); Gateway-Namen je Raum; homehub erscheint
- [x] **entity_category=diagnostic** für Hardware-/Fehler-/Konnektivitäts-Entities
- [x] Unit-Tests (entity_id/name, select-i18n, climate-Suppression, Sub-Device-Dedup, fan/swing/preset-Parsing)

## M9 — Write-Pfad (§5, §7) — LIVE verifiziert ✅
- [x] MQTT `/set` subscribe → Topic→Entry (ByTopic) auflösen
- [x] Validierung (settable), Wert-Coercion (number/select/switch), nested `{mode}`-Pfad → PATCH
- [x] **Live-Test: Wohnzimmer-Solltemperatur 24,5 → 24,0 °C erfolgreich gesetzt**
- [x] CLI-Flag-Bug gefixt (Flags nach Positionals) + PATCH-Fehlerbody im Log
- [x] 401-Auto-Refresh-Retry (GET+PATCH) bei serverseitig invalidiertem Token
- [x] Unit-Tests (Write-Validierung, nested `path`, select-Rückmapping) — in M7-Tests

## M10 — Diagnose-UI + OAuth (§6, §8) — LIVE verifiziert
- [x] `internal/web`: SPA (embed), Basic-Auth, ingress-aware
- [x] OAuth in UI: Auth-Status, Login/`/callback` (PKCE+state), Re-Auth
- [x] Geräte-/Datenpunkt-Browser, manueller PATCH-Test, Rate-Limit/Status
- [x] UI-i18n-Bundles `i18n/{en,de}.json` + Tests
- [x] In `main.go` verdrahtet; `/api/*` + SPA live geprüft

## M11 — Weitere Gerätetypen (§1)
- [x] gateway/indoorUnit/outdoorUnit (Info-Sensoren)
- [x] domesticHotWaterTank (Altherma) + Altherma leavingWater-Setpoints (Katalog + Mock-Test)
- [x] **Mock-API entdeckt** (`/mock/v1/gateway-devices` + `X-Mocking-Example-Id`): Fixtures in `internal/process/testdata/` (altherma, dx4, gas-boiler)
- [x] Energie-/Verbrauchsdaten (consumptionData → energy, total_increasing) — **live verifiziert** (0.5/9.1 kWh)
- [x] Luftreiniger (airPurificationMode) + indoorUnitHydro-Info + Mock-Tests
- [x] `--mock <example-id>` Flag im Client/util (ONECTA-Mock-Endpoint)
- [ ] domesticHotWaterFlowThrough, userInterface (kein Fixture/Gerät verfügbar)

## M12 — Deployment-Vervollständigung (§12)
- [x] `script/install.sh` (Wizard, Service-User `daikin`, gehärtete systemd-Unit)
- [x] `docker-build-push.yml` (buildx multi-arch → GHCR)
- [x] HA-Addon `addon/`: `config.yaml` (ingress, schema), `Dockerfile`, `build.yaml`, README/DOCS
- [x] `script/run.sh` (bashio, options.json → DAIKIN_*, Token-Store unter `/data`)
- [x] README-Installwege + Features finalisiert; changelog 0.1.0 gefüllt
- [x] Addon `icon.png` (256×256) + `logo.png` ergänzt (aus `tmp/logo.png`)
- [ ] erster Release-Tag (vom User auszulösen)

---

## Querschnitt (laufend)
- [x] `go test -race` grün über die gesamte Suite
- [x] `gofmt`/`go vet` sauber
- [x] graceful degradation (scan_ignore/rate-limit/reauth/401-Refresh; MQTT-Reconnect via Lifecycle)
- [x] `govulncheck`: keine Schwachstellen
- [~] `golangci-lint`: Cleanup läuft (Ziel: 0 Findings)
- [x] changelog 0.1.0 gepflegt
- [x] **LIVE END-TO-END bestätigt**: Daemon publiziert Entities (inkl. Energie) an MQTT + HA-Discovery; Web-UI + OAuth + Write-Pfad (Solltemperatur) live geprüft; Select-Werte lokalisiert (Kühlen) inkl. Rückmapping auf API-Codes
