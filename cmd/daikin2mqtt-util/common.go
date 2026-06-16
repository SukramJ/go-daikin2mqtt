// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/auth"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/client"
)

// loadConfig resolves and loads the daemon config the way the daemon does:
// an explicit path, otherwise the standard search order.
func loadConfig(configPath string) (*config.Config, error) {
	env := config.OSEnv{}
	path := configPath
	if path == "" {
		located, ok := config.Locate(env)
		if !ok {
			return nil, fmt.Errorf("no config file found; pass --config or create %s in a standard location", config.ConfigFile)
		}
		path = located
	}
	cfg, err := config.LoadFile(path, env)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// authConfig builds the OAuth config from the daemon config.
func authConfig(cfg *config.Config) auth.Config {
	return auth.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURI:  cfg.RedirectURI,
	}
}

// tokenSource builds a TokenSource backed by the configured token store.
func tokenSource(cfg *config.Config) *auth.TokenSource {
	store := auth.NewStore(cfg.ResolveTokenStorePath(config.OSEnv{}))
	return auth.NewTokenSource(authConfig(cfg), store, nil)
}

// buildClient builds a cloud client wired to the configured token source.
// mockID, when non-empty, routes reads to the ONECTA mock endpoint.
func buildClient(cfg *config.Config, mockID string) *client.Client {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return client.New(client.Options{
		Tokens:        tokenSource(cfg),
		ScanIgnore:    cfg.ScanIgnoreDuration(),
		Logger:        logger,
		MockExampleID: mockID,
	})
}

// openBrowser makes a best-effort attempt to open url in the user's
// browser. It never fails the flow — the URL is always printed too.
func openBrowser(url string) {
	var cmd string
	args := make([]string, 0, 1)
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	_ = exec.CommandContext(context.Background(), cmd, append(args, url)...).Start() //nolint:gosec // fixed command, user-initiated
}
