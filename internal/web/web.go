// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package web serves the optional web UI for go-daikin2mqtt and hosts the
// OAuth2 authorization callback.
//
// The dashboard — server, hardening, SSE stream and single-page app — is
// go-bridgeui, the one every go-*2mqtt bridge embeds; the adapter here
// presents the state store (what the bridge publishes) and the
// coordinator's write queue as its contract. Beside it, as bridge routes
// behind the same auth, runs the cloud tools page (tools.html): the OAuth
// login, the raw device view, the weekly-schedule editor, the manual PATCH
// test and the rate limit — a hand-written vanilla page over a thin
// standard-library HTTP layer on the ONECTA cloud client, the OAuth token
// source and the characteristic catalog.
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
	"embed"
	"encoding/json"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/SukramJ/go-bridgeui"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/auth"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/client"
	"github.com/SukramJ/go-daikin2mqtt/internal/schedule"
	"github.com/SukramJ/go-daikin2mqtt/internal/state"
	"github.com/SukramJ/go-daikin2mqtt/internal/version"
)

// staticFS holds the tools page. It is hand-written vanilla HTML/CSS/JS (no
// build step) so the asset tree is committed as-is and embedded verbatim.
// The tree is tools.html at the root, /static for the JS/CSS assets and
// /tools-i18n for its translation bundles — all referenced with relative
// URLs so the page works behind Home-Assistant ingress.
//
//go:embed assets
var staticFS embed.FS

// i18nFS holds the dashboard's labels — sections, groups, status and
// configuration tiles — merged over go-bridgeui's built-in bundles.
//
//go:embed i18n
var i18nFS embed.FS

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
	// State is what the bridge publishes, for the dashboard; nil shows no
	// devices.
	State *state.Store
	// Bridge queues the dashboard's writes and reports the cloud's health
	// (*coordinator.Coordinator); nil makes every write fail as not ready.
	Bridge Bridge
	// MQTTUp reports the broker connection; nil counts it as up.
	MQTTUp func() bool
	Logger *slog.Logger
}

// Bridge is the subset of the coordinator the dashboard needs.
type Bridge interface {
	Submit(deviceID, embeddedID, topic, payload string) error
	Upstream() (cloudOK, devicesKnown bool, lastPoll time.Time)
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
	ui       *bridgeui.Server

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
	tr, err := bridgeui.LoadTranslations(i18nFS, "i18n")
	if err != nil {
		// The bundles are embedded and covered by a test.
		panic("web: " + err.Error())
	}
	s.ui = bridgeui.New(bridgeui.Options{
		Bind:         cfg.WebBind,
		User:         cfg.WebUser,
		Password:     cfg.WebPassword,
		Realm:        "daikin2mqtt",
		Logger:       log,
		Language:     cfg.Language,
		Layout:       dashboardLayout(cfg),
		Translations: tr,
		Routes:       s.register,
	}, newAdapter(d, cfg))
	return s
}

// Handler returns the fully wired HTTP handler: the dashboard and the tools
// page behind one auth. Exported for tests.
func (s *Server) Handler() http.Handler {
	return s.ui.Handler()
}

// Run binds the socket on Cfg.WebBind and serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	return s.ui.Run(ctx)
}

// cloudRequestTimeout bounds the routes that wait on the cloud: GET
// /api/devices can wait on the cloud lock plus GET retries with backoff before
// it starts responding, longer than the dashboard's write deadline.
const cloudRequestTimeout = 60 * time.Second

// slow extends the response write deadline of a route that waits on the cloud.
func slow(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(cloudRequestTimeout))
		next(w, r)
	}
}

// register wires the tools page — its REST API, the OAuth endpoints and its
// assets — onto the dashboard's mux. The mutating routes take JSON from the
// same origin only.
func (s *Server) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tools/config", s.handleConfig)
	mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	mux.HandleFunc("GET /api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("GET /callback", s.handleCallback)
	mux.HandleFunc("GET /api/devices", slow(s.handleDevices))
	mux.HandleFunc("POST /api/patch", slow(s.handlePatch))
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

	// The tools page: tools.html + static/* assets + tools-i18n/* bundles.
	// "/" is the dashboard's.
	sub, err := fs.Sub(staticFS, "assets")
	if err != nil {
		// Embedded path is a compile-time constant; this cannot fail in a
		// correctly built binary.
		panic("web: embed sub fs: " + err.Error())
	}
	files := revalidating(http.FileServerFS(sub))
	mux.Handle("GET /tools.html", files)
	mux.Handle("GET /static/", files)
	mux.Handle("GET /tools-i18n/", files)
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
