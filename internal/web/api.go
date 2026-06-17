// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/auth"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
)

// configView is the sanitised, secret-free projection of the daemon config
// the SPA needs to render itself. It deliberately omits OAuth secrets,
// tokens and MQTT credentials.
type configView struct {
	Language   string  `json:"language"`
	HASSEnable bool    `json:"hass_enable"`
	Web        webView `json:"web"`
}

// webView exposes only the non-sensitive web settings (the bind address and
// whether Basic auth is enabled — never the password itself).
type webView struct {
	Bind     string `json:"bind"`
	AuthOn   bool   `json:"auth_on"`
	Language string `json:"language"`
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	lang := s.cfg.Language
	if lang == "" {
		lang = "en"
	}
	writeJSON(w, http.StatusOK, configView{
		Language:   lang,
		HASSEnable: s.cfg.HASSEnable,
		Web: webView{
			Bind:     s.cfg.WebBind,
			AuthOn:   s.cfg.WebUser != "",
			Language: lang,
		},
	})
}

// authStatusView reports whether a usable access token is available.
type authStatusView struct {
	Authenticated bool   `json:"authenticated"`
	ExpiresAt     string `json:"expires_at"`
	Detail        string `json:"detail"`
}

// handleAuthStatus reports authentication state. authenticated is true when
// the token source yields a token without error; ErrReauthRequired (and any
// other error) maps to authenticated:false with a human-readable detail.
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if s.tokens == nil {
		writeJSON(w, http.StatusOK, authStatusView{Detail: "token source unavailable"})
		return
	}
	_, err := s.tokens.Token(r.Context())
	if err != nil {
		detail := "re-authentication required"
		if !errors.Is(err, auth.ErrReauthRequired) {
			detail = err.Error()
		}
		writeJSON(w, http.StatusOK, authStatusView{Authenticated: false, Detail: detail})
		return
	}
	writeJSON(w, http.StatusOK, authStatusView{Authenticated: true, Detail: "authenticated"})
}

// handleAuthLogin starts the OAuth2 authorization-code-with-PKCE flow: it
// generates a fresh state + PKCE pair, stashes the verifier under the state
// in memory, and 302-redirects the browser to the IdP authorize URL.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	pkce, err := auth.GeneratePKCE()
	if err != nil {
		s.errorPage(w, http.StatusInternalServerError, "could not start login: "+err.Error())
		return
	}
	state, err := auth.GenerateState()
	if err != nil {
		s.errorPage(w, http.StatusInternalServerError, "could not start login: "+err.Error())
		return
	}
	// Resolve the redirect_uri for this login and replay it verbatim at the
	// token exchange (the IdP requires both to match). A per-request copy of
	// the OAuth config carries the resolved value without mutating shared state.
	redir := s.effectiveRedirectURI(r)
	s.states.put(state, pkce.Verifier, redir)
	if redir != s.auth.RedirectURI {
		s.log.Info("web.oauth_redirect_uri",
			slog.String("redirect_uri", redir),
			slog.String("hint", "register this exact URI with the Daikin developer portal"))
	}
	cfg := s.auth
	cfg.RedirectURI = redir
	http.Redirect(w, r, cfg.AuthorizeURL(state, pkce.Challenge), http.StatusFound)
}

// effectiveRedirectURI returns the OAuth redirect_uri to use for this request.
// An explicitly configured value always wins. Otherwise it is derived from the
// request so the flow works behind Home-Assistant ingress (or any TLS reverse
// proxy) with no manual configuration: <scheme>://<host><ingress-path>/callback.
// The derived value is logged at login time so the operator can register the
// exact URI with the IdP.
func (s *Server) effectiveRedirectURI(r *http.Request) string {
	// An explicit, non-default redirect_uri always wins. When it is unset or
	// still the localhost default (the value config fills in for an empty
	// option), derive it from the request instead.
	if s.auth.RedirectURI != "" && s.auth.RedirectURI != config.DefaultRedirectURI {
		return s.auth.RedirectURI
	}
	scheme := "http"
	switch {
	case firstValue(r.Header.Get("X-Forwarded-Proto")) != "":
		scheme = firstValue(r.Header.Get("X-Forwarded-Proto"))
	case r.TLS != nil:
		scheme = "https"
	case ingressPrefix(r) != "":
		// HA ingress always fronts the add-on over external HTTPS, even though
		// the proxied hop to the add-on itself is plain HTTP.
		scheme = "https"
	}
	host := firstValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host + ingressPrefix(r) + "/callback"
}

// firstValue returns the first comma-separated token of a header value,
// trimmed — proxies may append multiple values (e.g. "https, http").
func firstValue(h string) string {
	if i := strings.IndexByte(h, ','); i >= 0 {
		h = h[:i]
	}
	return strings.TrimSpace(h)
}

// handleCallback completes the OAuth flow: it validates the returned state
// (CSRF), exchanges the code for a token using the stored PKCE verifier,
// persists the token, then redirects back to the UI root (ingress-aware).
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if errParam := q.Get("error"); errParam != "" {
		desc := q.Get("error_description")
		s.errorPage(w, http.StatusBadRequest,
			fmt.Sprintf("authorization denied: %s %s", errParam, desc))
		return
	}

	code := q.Get("code")
	state := q.Get("state")
	if code == "" || state == "" {
		s.errorPage(w, http.StatusBadRequest, "missing code or state in callback")
		return
	}

	verifier, redir, ok := s.states.take(state)
	if !ok {
		s.errorPage(w, http.StatusBadRequest, "invalid or expired state (possible CSRF or timeout)")
		return
	}

	// Replay the exact redirect_uri recorded at login (it must match the one
	// sent to the authorize endpoint), via a per-request config copy.
	cfg := s.auth
	cfg.RedirectURI = redir
	tok, err := cfg.ExchangeCode(r.Context(), http.DefaultClient, code, verifier)
	if err != nil {
		s.log.Warn("web.callback_exchange_failed", slog.String("err", err.Error()))
		s.errorPage(w, http.StatusBadGateway, "token exchange failed: "+err.Error())
		return
	}

	if s.tokens != nil {
		if err := s.tokens.SetToken(tok); err != nil {
			s.log.Warn("web.callback_store_failed", slog.String("err", err.Error()))
			s.errorPage(w, http.StatusInternalServerError, "could not persist token: "+err.Error())
			return
		}
	}

	s.log.Info("web.authenticated")
	// rootRedirect only ever returns an internal relative path (validated
	// against scheme/host injection), so this cannot be an open redirect.
	http.Redirect(w, r, rootRedirect(r), http.StatusFound) //nolint:gosec // internal relative redirect only
}

// handleRateLimit returns the current rate-limit accounting snapshot.
func (s *Server) handleRateLimit(w http.ResponseWriter, _ *http.Request) {
	if s.client == nil {
		writeError(w, http.StatusServiceUnavailable, "client unavailable")
		return
	}
	writeJSON(w, http.StatusOK, s.client.RateLimit())
}

// deviceView is the JSON shape returned by /api/devices.
type deviceView struct {
	ID               string                `json:"id"`
	Model            string                `json:"model"`
	ManagementPoints []managementPointView `json:"management_points"`
}

type managementPointView struct {
	EmbeddedID      string               `json:"embedded_id"`
	Type            string               `json:"type"`
	Category        string               `json:"category"`
	Characteristics []characteristicView `json:"characteristics"`
}

// characteristicView flattens one characteristic plus optional catalog
// enrichment. Matched is false when no catalog entry applies; such
// characteristics are still emitted (raw value only).
type characteristicView struct {
	Name        string `json:"name"`
	Value       any    `json:"value"`
	Kind        string `json:"kind"` // string|number|bool|object
	Settable    bool   `json:"settable"`
	Matched     bool   `json:"matched"`
	Topic       string `json:"topic,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Platform    string `json:"platform,omitempty"`
	DeviceClass string `json:"device_class,omitempty"`
	Unit        string `json:"unit,omitempty"`
}

// handleDevices fetches the device tree from the cloud, parses it and
// enriches each characteristic via the catalog (when a match exists).
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if s.client == nil {
		writeError(w, http.StatusServiceUnavailable, "client unavailable")
		return
	}
	raw, err := s.client.GetDevices(r.Context())
	if err != nil {
		s.log.Warn("web.devices_failed", slog.String("err", err.Error()))
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	devices, err := model.ParseDevices(raw)
	if err != nil {
		writeError(w, http.StatusBadGateway, "parse devices: "+err.Error())
		return
	}

	lang := s.cfg.Language
	if lang == "" {
		lang = "en"
	}

	out := make([]deviceView, 0, len(devices))
	for _, d := range devices {
		dv := deviceView{ID: d.ID, Model: d.Model}
		for _, mp := range d.ManagementPoints {
			mpv := managementPointView{
				EmbeddedID: mp.EmbeddedID,
				Type:       mp.Type,
				Category:   mp.Category,
			}
			for name, c := range mp.Characteristics {
				mpv.Characteristics = append(mpv.Characteristics,
					s.characteristicView(mp.Type, name, c, lang))
			}
			dv.ManagementPoints = append(dv.ManagementPoints, mpv)
		}
		out = append(out, dv)
	}
	writeJSON(w, http.StatusOK, out)
}

// characteristicView builds the enriched view for a single characteristic.
func (s *Server) characteristicView(mpType, name string, c model.Characteristic, lang string) characteristicView {
	cv := characteristicView{
		Name:     name,
		Settable: c.Settable,
	}

	switch {
	case c.IsObject():
		cv.Kind = "object"
		var v any
		_ = json.Unmarshal(c.Value, &v)
		cv.Value = v
	default:
		if s, ok := c.String(); ok {
			cv.Kind = "string"
			cv.Value = s
		} else if b, ok := c.Bool(); ok {
			cv.Kind = "bool"
			cv.Value = b
		} else if f, ok := c.Float(); ok {
			cv.Kind = "number"
			cv.Value = f
		} else {
			cv.Kind = "object"
			var v any
			_ = json.Unmarshal(c.Value, &v)
			cv.Value = v
		}
	}

	if s.catalog != nil {
		if e, ok := s.catalog.Match(mpType, name); ok {
			cv.Matched = true
			cv.Topic = e.Topic
			cv.DisplayName = e.LocalizedName(lang)
			cv.Platform = e.Platform
			cv.DeviceClass = e.DeviceClass
			cv.Unit = e.Unit
			// The catalog is authoritative on settability when it covers a
			// characteristic.
			cv.Settable = e.Settable
		}
	}
	return cv
}

// patchRequest is the body of POST /api/patch. Value is decoded as a raw
// JSON value so the client may send a string, number or bool.
type patchRequest struct {
	DeviceID       string `json:"deviceId"`
	EmbeddedID     string `json:"embeddedId"`
	Characteristic string `json:"characteristic"`
	Value          any    `json:"value"`
	Path           string `json:"path"`
}

// handlePatch writes a single characteristic value to the cloud.
func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request) {
	if s.client == nil {
		writeError(w, http.StatusServiceUnavailable, "client unavailable")
		return
	}
	var req patchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.DeviceID == "" || req.EmbeddedID == "" || req.Characteristic == "" {
		writeError(w, http.StatusBadRequest, "deviceId, embeddedId and characteristic are required")
		return
	}
	if req.Value == nil {
		writeError(w, http.StatusBadRequest, "missing 'value'")
		return
	}

	err := s.client.Patch(r.Context(), req.DeviceID, req.EmbeddedID, req.Characteristic, req.Value, req.Path)
	if err != nil {
		s.log.Warn("web.patch_failed",
			slog.String("device", req.DeviceID),
			slog.String("characteristic", req.Characteristic),
			slog.String("err", err.Error()))
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.log.Info("web.patch_ok",
		slog.String("device", req.DeviceID),
		slog.String("characteristic", req.Characteristic))
	w.WriteHeader(http.StatusNoContent)
}

// rootRedirect resolves the URL to send the browser back to after a
// successful callback. Behind Home-Assistant ingress the add-on UI lives at
// the X-Ingress-Path prefix; otherwise a relative "./" returns to the SPA
// root regardless of where it is mounted.
func rootRedirect(r *http.Request) string {
	if p := ingressPrefix(r); p != "" {
		return p + "/"
	}
	return "./"
}

// ingressPrefix returns the validated Home-Assistant X-Ingress-Path prefix
// (without a trailing slash), or "" when the header is absent or unsafe. Only
// an internal, absolute path ("/...") with no scheme or protocol-relative
// ("//") prefix is honored, so the header cannot drive an open redirect or
// inject an external host into the derived redirect_uri.
func ingressPrefix(r *http.Request) string {
	p := r.Header.Get("X-Ingress-Path")
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") ||
		strings.Contains(p, ":") {
		return ""
	}
	return strings.TrimRight(p, "/")
}

// writeJSON encodes v as the response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends a small JSON error envelope.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// errorPage renders a minimal HTML error page for the human-facing OAuth
// endpoints (login / callback), where a JSON envelope would be unhelpful.
func (s *Server) errorPage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`+
		`<title>daikin2mqtt — error</title><meta name="viewport" content="width=device-width, initial-scale=1">`+
		`<style>body{font-family:-apple-system,Segoe UI,Roboto,sans-serif;background:#0b1120;color:#e6edf7;`+
		`display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}`+
		`.box{max-width:520px;padding:2rem;background:#16223c;border:1px solid #243352;border-radius:14px}`+
		`h1{margin-top:0;font-size:1.25rem}a{color:#5aa9ff}</style></head><body><div class="box">`+
		`<h1>Authentication error</h1><p>%s</p><p><a href="./">Back to dashboard</a></p></div></body></html>`,
		htmlEscape(msg))
}

// htmlEscape minimally escapes a string for safe embedding in HTML text.
func htmlEscape(s string) string {
	r := make([]rune, 0, len(s))
	for _, c := range s {
		switch c {
		case '<':
			r = append(r, []rune("&lt;")...)
		case '>':
			r = append(r, []rune("&gt;")...)
		case '&':
			r = append(r, []rune("&amp;")...)
		default:
			r = append(r, c)
		}
	}
	return string(r)
}
