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
   Daikin `client_id` and `client_secret`). If you use the Supervisor's
   Mosquitto broker, leave `mqtt_server` as `core-mosquitto`.
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
rejects `http://` (and `localhost`). The add-on serves the OAuth callback on
its `:8080` (behind Ingress), but that is plain HTTP, so you must front it
with an HTTPS endpoint that forwards to the add-on's `:8080` — for example an
HTTPS reverse proxy or a tunnel — and set that URL as the **`redirect_uri`**
option (path `…/callback`). Register the *same* URL for your client in the
[Daikin Developer Portal](https://developer.cloud.daikineurope.com). The
default empty value falls back to `http://localhost:8080/callback`, which
only works for a browser running on the same host as the add-on. See
`docs/konzept.md` (§11, §12.4) for the full redirect-URI guidance.

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
