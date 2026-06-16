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
export DAIKIN_MQTT_SERVER="$(bashio::config 'mqtt_server')"
export DAIKIN_MQTT_PORT="$(bashio::config 'mqtt_port')"
export DAIKIN_MQTT_LOGIN="$(bashio::config 'mqtt_login')"
export DAIKIN_MQTT_PASSWORD="$(bashio::config 'mqtt_password')"
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
export DAIKIN_REDIRECT_URI="http://localhost:8080/callback"

# --- Persistent state ---
# Store the rotated refresh token on the add-on's /data volume so it
# survives add-on restarts and updates.
export DAIKIN_TOKEN_STORE_PATH="/data/token-store.json"

bashio::log.info "Configuration prepared; MQTT server: ${DAIKIN_MQTT_SERVER}:${DAIKIN_MQTT_PORT}"
bashio::log.info "Web UI / OAuth callback bound to ${DAIKIN_WEB_BIND} (served via Ingress)."

# Hand off to the daemon (becomes PID 1).
exec /usr/bin/daikin2mqtt
