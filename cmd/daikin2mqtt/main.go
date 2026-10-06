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
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	// Embed the IANA time-zone database: the scheduler interprets its blocks as
	// wall-clock times in a named zone, and the distroless image the daemon ships
	// in carries no system tzdata.
	_ "time/tzdata"

	"golang.org/x/sync/errgroup"

	"github.com/SukramJ/go-hamqtt/publisher"
	hagomqtt "github.com/SukramJ/go-hamqtt/publisher/gomqtt"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/coordinator"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/auth"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/client"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/layout"
	"github.com/SukramJ/go-daikin2mqtt/internal/schedule"
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

	// One level variable behind the one handler, so DEBUG and the
	// maintenance loglevel command change the level of the logger every
	// component already holds instead of building a second one.
	level := new(slog.LevelVar)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if err := run(*configPath, *catalogPath, logger, level); err != nil {
		logger.Error("daikin2mqtt.fatal", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

// run wires dependencies and blocks until the context is cancelled
// (SIGINT/SIGTERM, or the maintenance restart command) or a component fails.
func run(configPath, catalogPath string, logger *slog.Logger, level *slog.LevelVar) error {
	logger.Info("daikin2mqtt.boot", slog.String("build", version.String()))

	cfg, err := loadConfig(configPath, logger)
	if err != nil {
		return err
	}
	if cfg.Debug {
		level.Set(slog.LevelDebug)
	}
	if !layout.New(cfg.MQTTTopic).Conformant() {
		logger.Warn("daikin2mqtt.topic_not_conformant",
			slog.String("mqtt_topic", cfg.MQTTTopic),
			slog.String("effect", "a name containing / runs outside mqtt-smarthome 2.0 §3; tools scanning +/info will not see this instance"))
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
	// One shared HTTP client with a hard timeout: every cloud request runs
	// under the single-in-flight cloudLock (and token refresh under the token
	// source's mutex), so a stalled peer must never hang a request forever.
	hc := &http.Client{Timeout: 60 * time.Second}
	authCfg := auth.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURI: cfg.RedirectURI}
	tokens := auth.NewTokenSource(authCfg, auth.NewStore(cfg.ResolveTokenStorePath(config.OSEnv{})), hc)
	cloud := client.New(client.Options{
		Tokens:     tokens,
		HTTPClient: hc,
		ScanIgnore: cfg.ScanIgnoreDuration(),
		Logger:     logger,
	})

	// --- MQTT ---
	// The discovery-plane runtime is built from a FACTORY, not held as an
	// instance: the coordinator rebuilds it on every (re)connect, because
	// everything it remembers — what it has superseded, declared and announced
	// — is a statement about one broker connection. See coordinator.RuntimeFactory.
	//
	// The transport is deferred because of an ordering this cannot escape: the
	// Last Will is part of CONNECT, so the client needs it before it exists,
	// while the will IS the runtime's answer. haLink is wired to the real
	// client below, and refuses use before that rather than panicking — a
	// publish path losing one message beats the daemon dying.
	haLink := &deferredTransport{}
	haConfig := coordinator.RuntimeConfig(cfg, logger)
	newHARuntime := func() *publisher.Runtime { return publisher.New(haLink, haConfig) }

	bootRuntime := newHARuntime()
	defer bootRuntime.Close()
	// One function for both halves of the availability policy: this will ("0")
	// and the level the coordinator announces on every connect are the same
	// topic, `<name>/connected`, and that topic is the one every discovery
	// payload's bridge availability entry names (RuntimeConfig's Layout derives
	// it from internal/layout). Three literals in three packages is how a
	// sibling bridge ended up with a will no entity reads.
	will, err := bootRuntime.Will()
	if err != nil {
		return fmt.Errorf("mqtt will: %w", err)
	}
	mqttClient := mqtt.NewTCPClient(mqtt.TCPConfig{
		BrokerURL:  fmt.Sprintf("tcp://%s:%d", cfg.MQTTServer, cfg.MQTTPort),
		ClientID:   mainClientID(cfg),
		Username:   cfg.MQTTLogin,
		Password:   cfg.MQTTPassword,
		CleanStart: true,
		Will:       bridgeWill(will),
		Logger:     logger,
	})
	lifecycle := mqtt.NewLifecycle(mqtt.LifecycleConfig{Logger: logger}, mqttClient)
	if err := lifecycle.Start(ctx); err != nil {
		return fmt.Errorf("mqtt: %w", err)
	}
	// Circuit breaker between the bridge and the broker: during a
	// degraded-broker phase (TCP link up, acks missing) publishes fail
	// fast with mqtt.ErrCircuitOpen instead of each stalling on the ack
	// timeout, and bounded half-open probes test recovery. Defaults: 5
	// consecutive broker-side failures open the circuit, recovery is
	// probed after 30s. The lifecycle's reconnect loop stays in charge
	// of the link itself.
	breaker := mqtt.NewBreaker(mqttClient, mqtt.BreakerConfig{
		OnStateChange: func(from, to mqtt.BreakerState) {
			logger.Warn("daikin2mqtt.mqtt_breaker_state",
				slog.String("from", from.String()),
				slog.String("to", to.String()))
		},
	})
	// The MQTT surface handed to the coordinator and HA discovery: Publish is
	// gated by the circuit breaker, while Subscribe/Unsubscribe go straight to
	// the client — the write-command subscription is a startup-path call with
	// its own SUBACK-bounded wait and must not be rejected during a
	// publish-side broker brownout. go-mqtt v1.4.0 extracted this pairing from
	// the five bridges that each carried their own copy of it.
	session := mqtt.SplitClient(breaker, mqttClient)
	// Publishes go through the circuit breaker, subscribes straight to the
	// client — the same split the coordinator gets, so the library planes and
	// the daemon's own calls cannot end up on different policies.
	haLink.wire(hagomqtt.Split(breaker, mqttClient))
	statePlane := coordinator.NewStatePlane(haLink, layout.New(cfg.MQTTTopic), nil, logger)
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
	var faikinConnected func() bool
	if cfg.LocalEnabled() {
		if cfg.FaikinSharesMainBroker() {
			faikinClient = mqttClient
			faikinConnected = mqttClient.IsConnected
			logger.Info("daikin2mqtt.local_mode",
				slog.String("faikin_broker", cfg.FaikinBrokerAddress()), slog.Bool("shared_connection", true))
		} else {
			fc := mqtt.NewTCPClient(mqtt.TCPConfig{
				BrokerURL:  "tcp://" + cfg.FaikinBrokerAddress(),
				ClientID:   faikinClientID(cfg),
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
			faikinConnected = fc.IsConnected
			logger.Info("daikin2mqtt.local_mode",
				slog.String("faikin_broker", cfg.FaikinBrokerAddress()), slog.Bool("shared_connection", false))
		}
	}

	// --- HA discovery (optional) ---
	var discovery *hass.Discovery
	if cfg.HASSEnable {
		discovery = hass.New(cfg.HASSBaseTopic, cfg.MQTTTopic, cfg.Language, session)
	}

	// --- mqtt-smarthome instance plane ---
	// `<name>/info` on every connect and, unless MQTT_MAINTENANCE is off, the
	// maintenance topics: the log level goes to the one LevelVar behind the
	// daemon's handler, and a restart is this process's own graceful
	// shutdown — the same cancel SIGTERM triggers, after which run publishes
	// `<name>/connected` = 0 and returns nil, so the process exits 0. It is
	// refused unless something will start it again (DAIKIN_SUPERVISED, else
	// systemd, Kubernetes or a container marker).
	instance := publisher.NewInstance(haLink, publisher.InstanceConfig{
		Layout:              hass.Layout(cfg.MQTTTopic),
		Name:                hass.OriginName,
		Version:             version.Version,
		Extra:               map[string]any{"mode": upstreamMode(cfg)},
		MaintenanceDisabled: !cfg.MaintenanceEnabled(),
		SetLogLevel:         publisher.LevelVarSetter(level),
		Supervised:          publisher.DetectSupervised("DAIKIN_SUPERVISED"),
		Shutdown:            cancel,
		StatsInterval:       publisher.StatsInterval(cfg.StatsIntervalSeconds()),
		Logger:              logger,
	})

	// --- Coordinator ---
	// FaikinMQTT deliberately stays ungated: it carries low-rate,
	// user-intent device commands (and its own state subscription),
	// possibly on a separate local broker whose health is independent of
	// the main link — a main-broker brownout must not reject them.
	coord := coordinator.New(coordinator.Deps{
		Cfg:          cfg,
		Client:       cloud,
		MQTT:         session,
		FaikinMQTT:   faikinClient,
		Catalog:      cat,
		HASS:         discovery,
		Logger:       logger,
		NewHARuntime: newHARuntime,
		StatePlane:   statePlane,
		Instance:     instance,
		// Half of `<name>/connected` in local mode: the link the Faikin
		// modules are reached over.
		FaikinConnected: faikinConnected,
		// The broker's own outbound limit, renegotiated on every connect. The
		// device document is preflighted against it BEFORE the retraction —
		// go-mqtt refuses an oversized packet from its write path, by which
		// time the per-entity configs the document supersedes are already
		// gone. This is the broker's ADVERTISED limit, not
		// mqtt.TCPConfig.MaximumPacketSize, which is this client's own inbound
		// cap and says nothing about what the broker will accept.
		BrokerMaxPacketSize: func() (uint32, bool) {
			res, ok := mqttClient.ConnectResult()
			if !ok {
				return 0, false
			}
			return res.MaximumPacketSize, true
		},
	})
	// Re-announce the connected level, the info and the retained status
	// after every (re)connect.
	lifecycle.OnConnect(func(cctx context.Context) { coord.PublishOnline(cctx) })

	// --- Weekly schedules (optional) ---
	// The engine and the coordinator reference each other: the engine applies
	// through the coordinator, the coordinator routes the HA enable switch back
	// to the engine. Build the engine second and attach it, which keeps the
	// dependency explicit instead of hiding it behind a lazy lookup.
	var scheduleEngine *schedule.Engine
	if cfg.ScheduleEnable {
		storePath := cfg.ResolveScheduleStorePath(config.OSEnv{})
		scheduleEngine, err = schedule.NewEngine(schedule.Options{
			Store:    schedule.NewStore(storePath),
			Logger:   logger,
			Timezone: cfg.ScheduleTimezone,
			Catchup:  cfg.ScheduleCatchupDuration(),
			Applier:  coord,
			States:   coord,
		})
		if err != nil {
			return fmt.Errorf("schedule: %w", err)
		}
		coord.AttachScheduler(scheduleEngine)
		logger.Info("daikin2mqtt.schedule_enabled",
			slog.String("store", storePath),
			slog.String("timezone", scheduleEngine.Location().String()))
	}

	logger.Info("daikin2mqtt.starting",
		slog.String("mqtt", cfg.MQTTServer), slog.Bool("hass", cfg.HASSEnable),
		slog.Bool("web", cfg.WebEnable), slog.Bool("schedule", cfg.ScheduleEnable),
		slog.String("lang", cfg.Language))

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return coord.Run(gctx) })
	if scheduleEngine != nil {
		g.Go(func() error { return scheduleEngine.Run(gctx) })
	}

	if cfg.WebEnable {
		srv := web.New(web.Deps{
			Cfg:     cfg,
			Auth:    authCfg,
			Tokens:  tokens,
			Client:  cloud,
			Catalog: cat,
			// Only the coordinator knows which indoor units share an outdoor
			// unit, which is what makes a scheduled heat/cool clash detectable.
			Schedule: scheduleEngine,
			Groups:   coord.OutdoorGroups,
			Logger:   logger,
		})
		g.Go(func() error { return srv.Run(gctx) })
	}

	err = g.Wait()
	// A normal DISCONNECT discards the Last Will, so a graceful stop states
	// `<name>/connected` = 0 itself (spec §3.1) before the deferred stop of
	// the MQTT lifecycle closes the connection.
	offCtx, offStop := context.WithTimeout(context.Background(), 2*time.Second)
	coord.PublishOffline(offCtx)
	offStop()
	return err
}

// upstreamMode is the `mode` project field of `<name>/info`: whether the
// mapped indoor units are read and written over Faikin or the ONECTA cloud.
func upstreamMode(cfg *config.Config) string {
	if cfg.LocalEnabled() {
		return "local"
	}
	return "cloud"
}

// bridgeWill copies publisher.Will onto the client's own will type, field for
// field and with no literal of its own.
//
// A function rather than an inline literal because run() is a composition root
// that dials a broker and blocks, so nothing can assert what it passed; this can
// be asserted, and what it asserts is that the CONNECT will and the connected
// level the coordinator announces come from one object. Two literals in two
// packages is how a sibling bridge ended up with a will no entity reads — the
// broker dutifully writes its will on a crash and every entity stays available
// forever, showing the last value it ever saw.
func bridgeWill(w publisher.Will) *mqtt.Will {
	return &mqtt.Will{
		Topic:   w.Topic,
		Payload: w.Payload,
		QoS:     mqtt.QoS(w.QoS),
		Retain:  w.Retain,
	}
}

// mainClientID is the MQTT client identifier the bridge presents on the main
// broker connection. It is read from the config, not from a constant: before
// MQTT_CLIENT_ID existed every installation presented the same string, and a
// broker MUST disconnect an existing session when a second client presents the
// identifier it holds (MQTT 3.1.1 §3.1.3.2 / 5.0 §3.1.4), so two daemons on
// one broker took each other down in a loop with nothing in either log saying
// why. That is F1 of the ADR 0070 phase 8 measurement.
func mainClientID(cfg *config.Config) string { return cfg.MQTTClientID }

// faikinClientID is the identifier for the second connection opened when the
// Faikin modules publish to a different broker. It derives from the same
// configured id, so setting MQTT_CLIENT_ID separates BOTH of an instance's
// sessions from another instance's, not just the main one.
func faikinClientID(cfg *config.Config) string {
	return cfg.MQTTClientID + config.FaikinClientIDSuffix
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
