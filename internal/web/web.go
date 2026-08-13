// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package web serves the optional diagnostic web UI for go-daikin2mqtt and
// hosts the OAuth2 authorization callback.
//
// It is a thin standard-library HTTP layer over the ONECTA cloud client, the
// OAuth token source and the characteristic catalog. A hand-written
// vanilla single-page app (no build step) embeds assets at build time, so
// enabling the UI adds no third-party dependencies and keeps the daemon a
// single static binary.
//
// The package depends on the cloud client and token source through small
// interfaces ([cloudClient] / [tokenSource]) rather than the concrete types,
// which keeps the handlers trivially testable with fakes. The concrete
// types from internal/daikin satisfy these interfaces structurally.
//
// Home-Assistant ingress: HA serves the add-on UI behind a path prefix and
// sets the X-Ingress-Path header. All asset, API and redirect URLs are kept
// relative so the SPA works both when accessed directly and behind ingress.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/auth"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/client"
	"github.com/SukramJ/go-daikin2mqtt/internal/schedule"
	"github.com/SukramJ/go-daikin2mqtt/internal/version"
)

// staticFS holds the compiled single-page app. It is hand-written vanilla
// HTML/CSS/JS (no build step) so the asset tree is committed as-is and
// embedded verbatim. The tree is index.html at the root, /static for the
// JS/CSS assets and /i18n for the translation bundles — all referenced with
// relative URLs so the SPA works behind Home-Assistant ingress.
//
//go:embed assets
var staticFS embed.FS

// cloudClient is the subset of the ONECTA cloud client the web server
// needs. *client.Client satisfies it structurally; defining it here keeps
// the dependency arrow pointing one way and makes handlers testable.
type cloudClient interface {
	GetDevices(ctx context.Context) (json.RawMessage, error)
	Patch(ctx context.Context, deviceID, embeddedID, characteristic string, value any, path string) error
	RateLimit() client.RateLimit
}

// tokenSource is the subset of the OAuth token source the web server needs.
// *auth.TokenSource satisfies it structurally.
type tokenSource interface {
	Token(ctx context.Context) (string, error)
	SetToken(t *auth.Token) error
}

// scheduleEngine is the subset of the schedule engine the API needs.
// *schedule.Engine satisfies it structurally; nil disables the schedule routes.
type scheduleEngine interface {
	Document() *schedule.Document
	Replace(doc *schedule.Document, expectRev int) (*schedule.Document, error)
	SetEnabled(id string, on bool) error
	Delete(id string) error
	Location() *time.Location
}

// Deps carries the collaborators the server needs. Cloud client and token
// source are interfaces so tests can inject fakes; the concrete daikin types
// satisfy them.
type Deps struct {
	Cfg     *config.Config
	Auth    auth.Config // OAuth client config (id/secret/redirect)
	Tokens  tokenSource
	Client  cloudClient
	Catalog *catalog.Catalog
	// Schedule is the weekly-programme engine. nil answers the schedule routes
	// with 503 (scheduler disabled) rather than 404, so the UI can tell "off"
	// from "missing".
	Schedule scheduleEngine
	// Groups reports the outdoor-unit groups (serial → device ids). Only the
	// coordinator knows them, so it is injected; nil disables the mode-conflict
	// check, which is meaningless without knowing who shares an outdoor unit.
	Groups func() map[string][]string
	Logger *slog.Logger
}

// Server is the embedded diagnostic web server. Construct it with [New] and
// run it with [Run]; mount [Handler] directly for tests or future ingress
// embedding.
type Server struct {
	cfg      *config.Config
	auth     auth.Config
	tokens   tokenSource
	client   cloudClient
	catalog  *catalog.Catalog
	schedule scheduleEngine
	groups   func() map[string][]string
	log      *slog.Logger
	handler  http.Handler

	states *stateStore
}

// New builds a Server. It does not bind a socket — call [Server.Run].
func New(d Deps) *Server {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	cfg := d.Cfg
	if cfg == nil {
		cfg = &config.Config{}
	}
	s := &Server{
		cfg:      cfg,
		auth:     d.Auth,
		tokens:   d.Tokens,
		client:   d.Client,
		catalog:  d.Catalog,
		schedule: d.Schedule,
		groups:   d.Groups,
		log:      log,
		states:   newStateStore(),
	}
	// A nil interface value carrying a nil *schedule.Engine would pass a
	// plain `!= nil` check and panic on first use; normalise it here so every
	// handler can rely on a simple nil test.
	if e, ok := d.Schedule.(*schedule.Engine); ok && e == nil {
		s.schedule = nil
	}
	s.handler = s.withAuth(s.routes())
	return s
}

// Handler returns the fully wired HTTP handler (auth middleware + routes).
// Exported for tests and future Home-Assistant ingress mounting.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// routes wires the REST API, the OAuth endpoints and the embedded SPA onto
// a ServeMux.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	mux.HandleFunc("GET /api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("GET /callback", s.handleCallback)
	mux.HandleFunc("GET /api/devices", s.handleDevices)
	mux.HandleFunc("POST /api/patch", s.handlePatch)
	mux.HandleFunc("GET /api/ratelimit", s.handleRateLimit)

	// Weekly schedules. Registered unconditionally: with the scheduler off they
	// answer 503 with a code the UI understands, which is a clearer signal than
	// a 404 that could equally mean "old version".
	mux.HandleFunc("GET /api/schedules", s.handleSchedulesList)
	mux.HandleFunc("POST /api/schedules", s.handleScheduleCreate)
	mux.HandleFunc("PUT /api/schedules/{id}", s.handleScheduleUpdate)
	mux.HandleFunc("DELETE /api/schedules/{id}", s.handleScheduleDelete)
	mux.HandleFunc("POST /api/schedules/{id}/enable", s.handleScheduleEnable)
	mux.HandleFunc("GET /api/schedules/preview", s.handleSchedulePreview)

	// Embedded SPA: index.html + static/* assets + i18n/* bundles. The
	// FileServerFS serves "/" as index.html, /static/* and /i18n/* directly,
	// and 404s unknown paths (there is no client-side routing to fall back
	// for), which is exactly the desired behaviour.
	sub, err := fs.Sub(staticFS, "assets")
	if err != nil {
		// Embedded path is a compile-time constant; this cannot fail in a
		// correctly built binary.
		panic("web: embed sub fs: " + err.Error())
	}
	mux.Handle("GET /", revalidating(http.FileServerFS(sub)))

	return mux
}

// assetsETag fingerprints the embedded asset tree, computed once at start-up.
//
// The build version would be the obvious validator — the assets are baked into
// the binary — but it is wrong for anyone running a build between releases (a
// main build, the :latest image, a local `make build`): the version string
// stays the same while the files change, so the browser would keep a stale
// copy on the strength of a matching ETag. Hashing the content cannot get that
// wrong. fs.WalkDir visits entries in lexical order, so the result is stable
// across processes.
var assetsETag = func() string {
	h := fnv.New64a()
	err := fs.WalkDir(staticFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = h.Write([]byte(path))
		_, _ = h.Write(b)
		return nil
	})
	if err != nil {
		// The tree is embedded at compile time; a read failure here would mean
		// a corrupt binary. Fall back to the version rather than panicking.
		return `"` + version.Version + `"`
	}
	return `"` + strconv.FormatUint(h.Sum64(), 36) + `"`
}()

// revalidating makes the browser check back before reusing an embedded asset.
//
// Files in an embed.FS carry no modification time, so net/http sends neither
// Last-Modified nor ETag — and a response with no validator and no
// Cache-Control is cached heuristically, which in practice means a browser
// keeps serving the previous style.css and app.js after the daemon is
// upgraded. The symptom is a UI that looks unchanged no matter what the new
// version fixed.
//
// With an ETag set before the file server runs, it answers a conditional
// request with 304 itself, so revalidation costs a round trip and no body.
func revalidating(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", assetsETag)
		next.ServeHTTP(w, r)
	})
}

// withAuth wraps next with HTTP Basic auth when both credentials are
// configured. With no credentials it returns next unchanged so an
// unauthenticated deployment (e.g. behind HA ingress) has zero overhead.
func (s *Server) withAuth(next http.Handler) http.Handler {
	if s.cfg.WebUser == "" && s.cfg.WebPassword == "" {
		return next
	}
	wantUser := []byte(s.cfg.WebUser)
	wantPass := []byte(s.cfg.WebPassword)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		// Constant-time compares so a timing side-channel can't probe the
		// credentials. Both fields must match.
		userOK := subtle.ConstantTimeCompare([]byte(user), wantUser) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), wantPass) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="daikin2mqtt", charset="UTF-8"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Run binds the socket on Cfg.WebBind and serves until ctx is cancelled,
// then shuts down gracefully. A clean shutdown returns nil; a bind failure
// surfaces as a non-nil error so the daemon tears everything down (a
// misconfigured port should be loud, not silently ignored).
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.WebBind,
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		// Generous write timeout: /api/devices can wait on the cloud lock plus
		// GET retries with backoff before it starts responding.
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	s.log.Info("web.listening",
		slog.String("bind", s.cfg.WebBind),
		slog.Bool("auth", s.cfg.WebUser != ""))

	select {
	case <-ctx.Done():
		// Fresh context on purpose: the parent ctx is already cancelled here,
		// but graceful shutdown still needs a deadline of its own.
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx) //nolint:contextcheck // intentional fresh context for shutdown
		s.log.Info("web.stopped")
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
