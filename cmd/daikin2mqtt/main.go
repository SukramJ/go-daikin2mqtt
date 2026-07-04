// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Command daikin2mqtt is the standalone daemon that bridges Daikin climate
// devices (via the ONECTA cloud API) to MQTT, including optional Home
// Assistant discovery and an optional diagnostic web UI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/coordinator"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/auth"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/client"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/version"
	"github.com/SukramJ/go-daikin2mqtt/internal/web"
)

func main() {
	configPath := flag.String("config", "", "path to config.yaml (default: search standard locations)")
	catalogPath := flag.String("catalog", "characteristics.yaml", "path to the characteristics catalog")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(*configPath, *catalogPath, logger); err != nil {
		logger.Error("daikin2mqtt.fatal", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

// run wires dependencies and blocks until the context is cancelled
// (SIGINT/SIGTERM) or a component fails.
func run(configPath, catalogPath string, logger *slog.Logger) error {
	logger.Info("daikin2mqtt.boot", slog.String("build", version.String()))

	cfg, err := loadConfig(configPath, logger)
	if err != nil {
		return err
	}
	if cfg.Debug {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		slog.SetDefault(logger)
	}

	// Missing client credentials are not fatal so a fresh add-on install does
	// not crash-loop: the daemon comes up (web UI reachable for onboarding) but
	// stays idle until CLIENT_ID/CLIENT_SECRET are set.
	if !cfg.CredentialsConfigured() {
		logger.Warn("daikin2mqtt.credentials_missing",
			slog.String("hint", "set CLIENT_ID and CLIENT_SECRET (add-on options) from the Daikin Developer Portal; the bridge stays idle until then"))
	}

	cat, err := catalog.LoadFile(catalogPath)
	if err != nil {
		return err
	}
	logger.Info("daikin2mqtt.catalog_loaded",
		slog.String("path", catalogPath), slog.Int("entries", len(cat.Entries())))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// --- Auth + cloud client ---
	authCfg := auth.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURI: cfg.RedirectURI}
	tokens := auth.NewTokenSource(authCfg, auth.NewStore(cfg.ResolveTokenStorePath(config.OSEnv{})), nil)
	cloud := client.New(client.Options{
		Tokens:     tokens,
		ScanIgnore: cfg.ScanIgnoreDuration(),
		Logger:     logger,
	})

	// --- MQTT ---
	statusTopic := cfg.MQTTTopic + "/bridge/status"
	mqttClient := mqtt.NewTCPClient(mqtt.TCPConfig{
		BrokerURL:  fmt.Sprintf("tcp://%s:%d", cfg.MQTTServer, cfg.MQTTPort),
		ClientID:   config.MQTTClientID,
		Username:   cfg.MQTTLogin,
		Password:   cfg.MQTTPassword,
		CleanStart: true,
		Will: &mqtt.Will{
			Topic:   statusTopic,
			Payload: []byte("offline"),
			Retain:  true,
		},
		Logger: logger,
	})
	lifecycle := mqtt.NewLifecycle(mqtt.LifecycleConfig{Logger: logger}, mqttClient)
	if err := lifecycle.Start(ctx); err != nil {
		return fmt.Errorf("mqtt: %w", err)
	}
	defer func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = lifecycle.Stop(stopCtx)
	}()

	// --- Local Faikin broker (optional) ---
	// In local mode the daemon reads/writes the indoor units through their
	// Faikin modules' MQTT broker. When that is the same broker as the main one
	// (the common case) the existing connection is reused; otherwise a second
	// connection is opened.
	var faikinClient mqtt.Client
	if cfg.LocalEnabled() {
		if cfg.FaikinSharesMainBroker() {
			faikinClient = mqttClient
			logger.Info("daikin2mqtt.local_mode",
				slog.String("faikin_broker", cfg.FaikinBrokerAddress()), slog.Bool("shared_connection", true))
		} else {
			fc := mqtt.NewTCPClient(mqtt.TCPConfig{
				BrokerURL:  "tcp://" + cfg.FaikinBrokerAddress(),
				ClientID:   config.MQTTClientID + "-faikin",
				Username:   cfg.FaikinLogin(),
				Password:   cfg.FaikinPassword(),
				CleanStart: true,
				Logger:     logger,
			})
			flife := mqtt.NewLifecycle(mqtt.LifecycleConfig{Logger: logger}, fc)
			if err := flife.Start(ctx); err != nil {
				return fmt.Errorf("faikin mqtt: %w", err)
			}
			defer func() {
				stopCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				_ = flife.Stop(stopCtx)
			}()
			faikinClient = fc
			logger.Info("daikin2mqtt.local_mode",
				slog.String("faikin_broker", cfg.FaikinBrokerAddress()), slog.Bool("shared_connection", false))
		}
	}

	// --- HA discovery (optional) ---
	var discovery *hass.Discovery
	if cfg.HASSEnable {
		discovery = hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, cfg.Language, mqttClient)
	}

	// --- Coordinator ---
	coord := coordinator.New(coordinator.Deps{
		Cfg:        cfg,
		Client:     cloud,
		MQTT:       mqttClient,
		FaikinMQTT: faikinClient,
		Catalog:    cat,
		HASS:       discovery,
		Logger:     logger,
	})
	// Re-announce availability after every (re)connect.
	lifecycle.OnConnect(func(cctx context.Context) { coord.PublishOnline(cctx) })

	logger.Info("daikin2mqtt.starting",
		slog.String("mqtt", cfg.MQTTServer), slog.Bool("hass", cfg.HASSEnable),
		slog.Bool("web", cfg.WebEnable), slog.String("lang", cfg.Language))

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return coord.Run(gctx) })

	if cfg.WebEnable {
		srv := web.New(web.Deps{
			Cfg:     cfg,
			Auth:    authCfg,
			Tokens:  tokens,
			Client:  cloud,
			Catalog: cat,
			Logger:  logger,
		})
		g.Go(func() error { return srv.Run(gctx) })
	}

	return g.Wait()
}

// loadConfig resolves the config path (explicit flag or standard search) and
// loads it with environment overrides applied.
func loadConfig(configPath string, logger *slog.Logger) (*config.Config, error) {
	env := config.OSEnv{}
	path := configPath
	if path == "" {
		if located, ok := config.Locate(env); ok {
			path = located
		}
	}
	// No config file (explicit or located): build the config from environment
	// variables and defaults alone. The Home Assistant add-on supplies every
	// setting via DAIKIN_* env and ships no file, so a missing file must not be
	// fatal — Validate still enforces the required values (CLIENT_ID, etc.).
	if path == "" {
		cfg, err := config.Load(strings.NewReader(""), env)
		if err != nil {
			return nil, err
		}
		logger.Info("daikin2mqtt.config_loaded", slog.String("path", "(environment only)"))
		return cfg, nil
	}
	cfg, err := config.LoadFile(path, env)
	if err != nil {
		return nil, err
	}
	logger.Info("daikin2mqtt.config_loaded", slog.String("path", path))
	return cfg, nil
}
