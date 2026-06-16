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

The OAuth2 redirect URI used by the add-on is
`http://localhost:8080/callback`. You must register a matching redirect URI
for your client in the
[Daikin Developer Portal](https://developer.cloud.daikineurope.com).
Because the flow runs behind Ingress, the browser is proxied by the
Supervisor; register the URI exactly as expected by your client
configuration. If your portal requires a publicly reachable URI, follow the
guidance in the project's `docs/konzept.md` (§12.4).

## Image build paths

There are two ways the add-on image can be produced. The add-on is
configured for the **preferred** path by default.

### Preferred: pre-built GHCR image

`addon/config.yaml` sets:

```yaml
image: "ghcr.io/sukramj/go-daikin2mqtt-{arch}"
```

When `image:` is present, the Supervisor **pulls** the matching multi-arch
image (`{arch}` is replaced with `aarch64`, `amd64`, or `armv7`) instead of
building locally. These images are published by
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
