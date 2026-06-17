# go-daikin2mqtt — Home Assistant Add-on

This add-on runs the [go-daikin2mqtt](https://github.com/SukramJ/go-daikin2mqtt)
daemon inside Home Assistant. It bridges Daikin climate devices (via the
Daikin ONECTA cloud API) to MQTT, with optional Home Assistant MQTT
discovery and a diagnostic web UI that also drives the OAuth2 login.

## Installation

1. In Home Assistant go to **Settings → Add-ons → Add-on Store**.
2. Click the **⋮** menu (top right) → **Repositories** and add:
   `https://github.com/SukramJ/go-daikin2mqtt`
3. The **go-daikin2mqtt** add-on now appears in the store. Open it and
   click **Install**.
4. Open the **Configuration** tab and fill in your options (at minimum your
   Daikin `client_id` and `client_secret`). Leave `mqtt_server` **empty** to
   auto-use the Home Assistant MQTT broker; set it only to
   target a different broker.
5. **Start** the add-on.

## Connecting to Daikin (OAuth2)

1. After starting, open the add-on's **Web UI** (the side-panel icon, or the
   **Open Web UI** button). The UI is served through Home Assistant Ingress,
   so no port needs to be exposed.
2. Click **Connect to Daikin** and complete the Daikin login. The rotated
   refresh token is stored at `/data/token-store.json` on the add-on's
   persistent volume, so it survives restarts and updates.

### Redirect URI registration

The Daikin Developer Portal **requires the redirect URI to use HTTPS** and
rejects `http://` (and `localhost`).

**Recommended (Ingress, zero-config):** leave the `redirect_uri` option
**empty**. When you click *Connect to Daikin* the add-on derives the redirect
URI from the request — behind Ingress that is your Home Assistant's external
**HTTPS** ingress URL — and logs it as `web.oauth_redirect_uri`. Copy that
exact URL into your client in the
[Daikin Developer Portal](https://developer.cloud.daikineurope.com), then click
*Connect* again. No open port or extra proxy rule is needed, since you already
reach Home Assistant over TLS. The derived URL contains the add-on's ingress
token; re-register it if the token ever changes (e.g. after a reinstall).

**Alternative (explicit reverse proxy / tunnel):** set the `redirect_uri`
option to an HTTPS URL that forwards to the add-on's `:8080`. For a reverse
proxy, first expose the port — open the add-on's **Network** tab and map host
port `8080` (unmapped by default, since Ingress needs no open port) — then
point your proxy at `http://<ha-host>:8080` and register the *same* URL with
the portal.

See `docs/konzept.md` (§11, §12.4) for the full redirect-URI guidance.

## Image build paths

There are two ways the add-on image can be produced. The add-on is
configured for the **preferred** path by default.

### Preferred: pre-built GHCR image

`addon/config.yaml` sets:

```yaml
image: "ghcr.io/sukramj/go-daikin2mqtt"
```

When `image:` is present, the Supervisor **pulls** that image at the tag
matching the add-on `version:`. It is a single multi-arch manifest (amd64,
aarch64, armv7), so Docker selects the right architecture automatically — no
`-{arch}` suffix is needed. These images are published by
`.github/workflows/docker-build-push.yml`. This path is fast and requires no
toolchain on the Home Assistant host.

### Fallback: local build from source

If you want the add-on built on the host instead, remove (or comment out)
the `image:` key in `addon/config.yaml`. The Supervisor will then build
`addon/Dockerfile` using the base images from `addon/build.yaml`.

> **Caveat:** the Go sources live at the **repository root**, not inside
> `addon/`. Home Assistant's local add-on builder normally uses the add-on
> directory as the Docker build context, which does **not** contain the Go
> sources. `addon/Dockerfile` is written for a **repository-root** build
> context and can be built manually:
>
> ```sh
> docker build \
>   --build-arg BUILD_FROM=ghcr.io/home-assistant/amd64-base:latest \
>   -f addon/Dockerfile -t go-daikin2mqtt-addon .
> ```
>
> For normal Home Assistant installs, prefer the GHCR image path above.

## Options

See [DOCS.md](DOCS.md) for a one-line reference of every option.
