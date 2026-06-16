// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package auth

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"time"
)

// BrowserFunc is called with the authorization URL the user must open. A
// CLI implementation may print it (and optionally shell out to a browser);
// a headless caller can just log it.
type BrowserFunc func(authURL string) error

// Authorize runs the full interactive Authorization Code + PKCE flow: it
// starts a one-shot callback server on bind, asks the user (via open) to
// visit the authorization URL, waits for the redirect, validates state,
// and exchanges the code for a token set.
//
// bind is the listen address ("host:port"); the handled path is taken from
// cfg.RedirectURI. The returned token is NOT persisted — the caller seeds
// it into a [TokenSource] (which persists it).
func (c Config) Authorize(ctx context.Context, hc *http.Client, bind string, open BrowserFunc) (*Token, error) {
	redir, err := url.Parse(c.RedirectURI)
	if err != nil {
		return nil, fmt.Errorf("auth: parse redirect_uri: %w", err)
	}
	path := redir.Path
	if path == "" {
		path = "/"
	}

	pkce, err := GeneratePKCE()
	if err != nil {
		return nil, err
	}
	state, err := GenerateState()
	if err != nil {
		return nil, err
	}

	code, err := runCallbackServer(ctx, bind, path, state, func() error {
		if open == nil {
			return nil
		}
		return open(c.AuthorizeURL(state, pkce.Challenge))
	})
	if err != nil {
		return nil, err
	}
	return c.ExchangeCode(ctx, hc, code, pkce.Verifier)
}

// callbackOutcome carries either the captured code or an error from the
// HTTP handler back to the waiting goroutine.
type callbackOutcome struct {
	code string
	err  error
}

// runCallbackServer listens on bind, serves path, and returns the
// authorization code once the redirect arrives with a matching state. It
// calls afterListen after the listener is up (so the URL is only shown
// once the server can actually accept the redirect).
func runCallbackServer(ctx context.Context, bind, path, state string, afterListen func() error) (string, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", bind)
	if err != nil {
		return "", fmt.Errorf("auth: listen on %s: %w", bind, err)
	}

	outcome := make(chan callbackOutcome, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			writePage(w, http.StatusBadRequest, "Authorization failed", e+": "+q.Get("error_description"))
			outcome <- callbackOutcome{err: fmt.Errorf("auth: authorization error: %s: %s", e, q.Get("error_description"))}
			return
		}
		if q.Get("state") != state {
			writePage(w, http.StatusBadRequest, "Authorization failed", "state mismatch (possible CSRF)")
			outcome <- callbackOutcome{err: errors.New("auth: state mismatch in callback")}
			return
		}
		code := q.Get("code")
		if code == "" {
			writePage(w, http.StatusBadRequest, "Authorization failed", "no authorization code in redirect")
			outcome <- callbackOutcome{err: errors.New("auth: empty code in callback")}
			return
		}
		writePage(w, http.StatusOK, "Authorized", "You can close this window and return to the terminal.")
		outcome <- callbackOutcome{code: code}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { //nolint:contextcheck // intentional fresh context for shutdown
		// Use a fresh context: the parent ctx may already be cancelled when
		// we tear the server down, but graceful shutdown still needs time.
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	if afterListen != nil {
		if err := afterListen(); err != nil {
			return "", err
		}
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case o := <-outcome:
		return o.code, o.err
	}
}

// writePage renders a minimal HTML status page for the browser.
func writePage(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	// Escape all interpolated values: msg in particular can derive from
	// query parameters, so render it as text to avoid any XSS.
	safeTitle := html.EscapeString(title)
	safeMsg := html.EscapeString(msg)
	_, _ = fmt.Fprintf(w,
		"<!doctype html><html><head><meta charset=utf-8><title>%s</title></head>"+
			"<body style=\"font-family:sans-serif;max-width:32rem;margin:4rem auto;text-align:center\">"+
			"<h1>%s</h1><p>%s</p></body></html>", safeTitle, safeTitle, safeMsg)
}
