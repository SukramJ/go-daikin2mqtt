#!/usr/bin/with-contenv bashio
# SPDX-License-Identifier: MIT
# Home Assistant add-on entrypoint for go-daikin2mqtt.
#
# Reads the user's add-on options (/data/options.json) via bashio, maps them
# onto the daemon's DAIKIN_* environment variables, wires up Ingress and
# persistent token storage, and finally exec's the binary so it becomes
# PID 1 and receives signals directly.
set -e

bashio::log.info "Starting go-daikin2mqtt add-on..."

# --- Daikin / ONECTA cloud (OAuth2) ---
export DAIKIN_CLIENT_ID="$(bashio::config 'client_id')"
export DAIKIN_CLIENT_SECRET="$(bashio::config 'client_secret')"

# --- MQTT ---
# Zero-config: when mqtt_server is left empty, borrow the
# broker the Supervisor already knows about (the HA MQTT integration /
# core-mosquitto add-on) via the mqtt service. An explicit mqtt_server always
# wins; if nothing is set and no service is offered, fall back to
# core-mosquitto:1883.
if bashio::config.has_value 'mqtt_server'; then
  export DAIKIN_MQTT_SERVER="$(bashio::config 'mqtt_server')"
  export DAIKIN_MQTT_PORT="$(bashio::config 'mqtt_port')"
  export DAIKIN_MQTT_LOGIN="$(bashio::config 'mqtt_login')"
  export DAIKIN_MQTT_PASSWORD="$(bashio::config 'mqtt_password')"
elif bashio::services.available 'mqtt'; then
  bashio::log.info "mqtt_server empty; using the Home Assistant MQTT service."
  export DAIKIN_MQTT_SERVER="$(bashio::services 'mqtt' 'host')"
  export DAIKIN_MQTT_PORT="$(bashio::services 'mqtt' 'port')"
  export DAIKIN_MQTT_LOGIN="$(bashio::services 'mqtt' 'username')"
  export DAIKIN_MQTT_PASSWORD="$(bashio::services 'mqtt' 'password')"
else
  bashio::log.warning "mqtt_server empty and no MQTT service offered; falling back to core-mosquitto:1883."
  export DAIKIN_MQTT_SERVER="core-mosquitto"
  export DAIKIN_MQTT_PORT="1883"
fi
export DAIKIN_MQTT_TOPIC="$(bashio::config 'mqtt_topic')"

# --- Home Assistant discovery ---
export DAIKIN_HASS_ENABLE="$(bashio::config 'hass_enable')"

# --- Polling / rate limiting ---
export DAIKIN_REFRESH_DAY_INTERVAL="$(bashio::config 'refresh_day_interval')"
export DAIKIN_REFRESH_NIGHT_INTERVAL="$(bashio::config 'refresh_night_interval')"

# --- Misc ---
export DAIKIN_LANGUAGE="$(bashio::config 'language')"

# --- Diagnostic web UI / Ingress ---
# Bind to all interfaces on 8080 so the Supervisor's Ingress proxy can reach
# the UI (the daemon's 127.0.0.1 default is unreachable from the proxy).
export DAIKIN_WEB_ENABLE="$(bashio::config 'web_enable')"
export DAIKIN_WEB_BIND="0.0.0.0:8080"

# The OAuth2 callback is served by the same web UI/port behind Ingress.
export DAIKIN_OAUTH_CALLBACK_BIND="0.0.0.0:8080"

# Redirect URI: Daikin requires an HTTPS, portal-registered URI, and behind
# Ingress the localhost default is not browser-reachable. Operators therefore
# set their own (an HTTPS reverse-proxy or tunnel URL that forwards to the
# add-on's :8080). Empty -> the local default, usable only for a browser on
# the same host as the add-on.
if bashio::config.has_value 'redirect_uri'; then
  export DAIKIN_REDIRECT_URI="$(bashio::config 'redirect_uri')"
else
  export DAIKIN_REDIRECT_URI="http://localhost:8080/callback"
fi

# --- Persistent state ---
# Store the rotated refresh token on the add-on's /data volume so it
# survives add-on restarts and updates.
export DAIKIN_TOKEN_STORE_PATH="/data/token-store.json"

bashio::log.info "Configuration prepared; MQTT server: ${DAIKIN_MQTT_SERVER}:${DAIKIN_MQTT_PORT}"
bashio::log.info "Web UI / OAuth callback bound to ${DAIKIN_WEB_BIND} (served via Ingress)."

# Hand off to the daemon (becomes PID 1).
exec /usr/bin/daikin2mqtt
