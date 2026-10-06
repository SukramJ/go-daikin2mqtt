// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package coordinator orchestrates the Daikin → MQTT data flow: it polls the
// cloud on an adaptive (day/night) interval, resolves devices against the
// catalog, publishes state and Home Assistant discovery, and applies inbound
// MQTT /set commands as cloud PATCHes.
package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/SukramJ/go-hamqtt/discovery"
	hamodel "github.com/SukramJ/go-hamqtt/model"
	"github.com/SukramJ/go-hamqtt/publisher"
	hagomqtt "github.com/SukramJ/go-hamqtt/publisher/gomqtt"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-daikin2mqtt/internal/layout"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/auth"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/client"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
	"github.com/SukramJ/go-daikin2mqtt/internal/faikin"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
	"github.com/SukramJ/go-daikin2mqtt/internal/schedule"
)

// CloudClient is the slice of the cloud client the coordinator needs.
type CloudClient interface {
	GetDevices(ctx context.Context) (json.RawMessage, error)
	Patch(ctx context.Context, deviceID, embeddedID, characteristic string, value any, path string) error
}

// Deps are the coordinator's dependencies.
type Deps struct {
	Cfg     *config.Config
	Client  CloudClient
	MQTT    mqtt.Client
	Catalog *catalog.Catalog
	HASS    *hass.Discovery // optional; nil disables discovery
	Logger  *slog.Logger
	Clock   func() time.Time
	// FaikinMQTT is the connection to the indoor units' local Faikin broker,
	// used for local-first reads/writes. Optional; nil disables local mode
	// regardless of LOCAL_MODE.
	FaikinMQTT mqtt.Client
	// NewHARuntime builds a fresh publisher.Runtime over the wired transport.
	// A factory, not an instance: see [RuntimeFactory]. Optional — nil builds
	// one over MQTT with [RuntimeConfig], which is what the tests get.
	NewHARuntime RuntimeFactory
	// BrokerMaxPacketSize reports the Maximum Packet Size the broker advertised
	// in its CONNACK (MQTT 5.0 property 0x27) and whether a connect has yet
	// happened. Optional; nil skips the preflight in [Coordinator.bundleFits].
	//
	// A function rather than a value because the answer is renegotiated on
	// every connect, and a function rather than a widened mqtt interface
	// because it lives on the concrete *mqtt.TCPClient — which this package
	// deliberately does not name.
	BrokerMaxPacketSize func() (uint32, bool)
	// StatePlane is the go-hamqtt state publisher every retained status item
	// goes out through. Optional; nil builds one over MQTT with
	// [NewStatePlane], so the QoS and the encoding are stated in one place
	// either way.
	StatePlane *publisher.StatePublisher
	// Instance is the mqtt-smarthome instance plane: the retained
	// `<name>/info`, the maintenance commands it registers on the command
	// router, and the periodic `<name>/maintenance/stats`. Optional; nil
	// publishes none of them.
	Instance *publisher.Instance
	// FaikinConnected reports whether the connection the Faikin modules are
	// reached over is up. Optional; nil counts it as up. It is half of what
	// `<name>/connected` says in local mode — see [Coordinator.upstreamLevel].
	FaikinConnected func() bool
}

// Coordinator owns the poll/publish/write loops.
type Coordinator struct {
	deps      Deps
	topicRoot layout.Root

	writes      chan writeReq
	localStates chan localStateMsg
	// refresh carries a manual poll request from the refresh button to the poll
	// loop. Capacity 1: a pending request already covers any further press.
	refresh chan struct{}

	mu              sync.Mutex
	lastPoll        time.Time                    // start of the last poll cycle (throttles manual refreshes)
	modeCache       map[string]string            // deviceID/embeddedID -> current operationMode
	powerCache      map[string]bool              // deviceID -> last known onOffMode == "on" (mode sync never touches off/unknown units)
	climateEmbedded map[string]string            // deviceID -> climateControl embeddedID (for local routing)
	outdoorSerial   map[string]string            // deviceID -> outdoor-unit serial (for multi-split grouping)
	lastLocal       map[string]*faikin.State     // deviceID -> last AC state received from Faikin
	lastEnergy      map[string]energyTotals      // deviceID -> last non-zero per-unit energy (held; see localOutdoorAgg)
	pendingOutdoor  map[string]outdoorHold       // "<group>|<topic>" -> just-written value, held until confirmed
	econoSuspend    map[string]econoSuspendState // outdoor group key -> powerful<->econo save/restore state
	econoLatch      map[string]bool              // outdoor group key -> last reliable econo state (see localOutdoorAgg)
	lastDiscSig     string                       // signature of the last published discovery set
	// priorComponents is, per device-document node id, the component set that
	// document carried BEFORE this batch — the only thing that can tell a
	// removed entity from an omitted one. Read back once per connection
	// (priorLoaded), then maintained in process. See
	// [Coordinator.loadPriorComponents].
	priorComponents map[string]map[string]discovery.Component
	priorLoaded     bool
	reconcileGate   sync.Mutex // try-locked gate so only one orphan sweep runs at a time
	// schedule is the weekly-programme engine, attached by AttachScheduler.
	// nil (the default) leaves the scheduler disabled.
	schedule scheduler

	// haRuntime is the CURRENT connection's publisher.Runtime. It is replaced
	// wholesale by resetHAPlane on every (re)connect, never mutated, and is
	// only ever read through ha() — never stored across a call that can block
	// on the broker, because the connection it belongs to may be gone by then.
	haRuntime atomic.Pointer[publisher.Runtime]
	// commands is the go-hamqtt router for "<name>/set/+/+/+" and the
	// instance's "<name>/maintenance/set/#".
	commands *publisher.CommandRouter
	// collectWindow is how long each retained-config snapshot listens. Written
	// once, by New, before anything can read it.
	collectWindow time.Duration
	// discoveryGen counts connections, not publishes. PublishOnline bumps it;
	// the discovery publish samples it before and after, and refuses to commit
	// its signature across a change — a batch that went out on a connection
	// that has since been replaced must not suppress the republish the new
	// connection is owed.
	discoveryGen atomic.Uint64

	// connMu serialises every write of `<name>/connected` with the runtime
	// swap of a reconnect, so a level change cannot land on a runtime that is
	// being replaced and then be lost. connLevel is the level the daemon has
	// decided on (0 until the first decision, which announces as 1).
	connMu    sync.Mutex
	connLevel int
	// cloudOK is whether the last cloud poll succeeded, and devicesKnown
	// whether any ever did since start; both guarded by mu. See
	// [Coordinator.upstreamLevel].
	cloudOK, devicesKnown bool
	// bounds holds each number point's live min/max/step from the last poll,
	// keyed deviceID|embeddedID|topic, for the clamping spec §5.3 asks of a
	// set. Guarded by mu.
	bounds map[string]numberBounds
	// lastDevices is the device ids the last successful poll resolved, guarded
	// by mu; swept is set once the start-up sweep of the 0.13 layout has run.
	lastDevices []string
	swept       atomic.Bool
}

// numberBounds is one number point's live range.
type numberBounds struct{ min, max, step *float64 }

// econoSuspendState tracks the powerful<->econo save/restore per outdoor group.
// econo limits the shared outdoor compressor, so a powerful (boost) on any member
// suspends it group-wide; the hardware does not restore econo afterwards, so the
// coordinator does. boosting is the last-observed "any member in powerful";
// pending records that econo was on when the boost began and must be restored
// when the last member leaves powerful. See reconcileEconoSuspend.
type econoSuspendState struct {
	boosting bool
	pending  bool
}

// energyTotals holds a unit's lifetime energy counters (Wh). They are monotonic
// per unit, so each field is held at its highest seen value to bridge the gaps
// when an idle unit stops reporting them (would otherwise read 0 and drop the
// summed outdoor total).
type energyTotals struct {
	total, heat, cool int64
}

// outdoorHold remembers a just-written outdoor-shared value so a stale Faikin
// status (the active indoor unit has not reported the change yet) cannot revert
// it before it is confirmed.
type outdoorHold struct {
	value string
	until time.Time
}

type writeReq struct {
	deviceID, embeddedID, topic string
	payload                     string
}

// localStateMsg carries a parsed Faikin state document from the MQTT read
// loop to the drain goroutine, which does the (blocking) republish work.
type localStateMsg struct {
	deviceID string
	st       *faikin.State
}

// New builds a coordinator.
func New(d Deps) *Coordinator {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	root := layout.New(d.Cfg.MQTTTopic)
	// The library planes are built here when the caller supplied none, over the
	// same client the daemon publishes everything else on. One construction
	// path, so the QoS that reaches the wire is stated in exactly one place
	// (NewStatePlane / RuntimeConfig) whether the daemon or a test built it.
	if d.MQTT != nil {
		tr := hagomqtt.Split(d.MQTT, d.MQTT)
		if d.StatePlane == nil {
			d.StatePlane = NewStatePlane(tr, root, d.Clock, d.Logger)
		}
		if d.NewHARuntime == nil {
			cfg := RuntimeConfig(d.Cfg, d.Logger)
			d.NewHARuntime = func() *publisher.Runtime { return publisher.New(tr, cfg) }
		}
	}
	c := &Coordinator{
		deps:            d,
		topicRoot:       root,
		collectWindow:   reconcileCollectWindow,
		writes:          make(chan writeReq, 64),
		localStates:     make(chan localStateMsg, 64),
		refresh:         make(chan struct{}, 1),
		modeCache:       map[string]string{},
		powerCache:      map[string]bool{},
		climateEmbedded: map[string]string{},
		outdoorSerial:   map[string]string{},
		lastLocal:       map[string]*faikin.State{},
		lastEnergy:      map[string]energyTotals{},
		pendingOutdoor:  map[string]outdoorHold{},
		econoSuspend:    map[string]econoSuspendState{},
		econoLatch:      map[string]bool{},
		bounds:          map[string]numberBounds{},
	}
	if d.NewHARuntime != nil {
		rt := d.NewHARuntime()
		checkLegacyForms(rt)
		c.haRuntime.Store(rt)
		c.commands = NewCommandRouter(hagomqtt.Split(d.MQTT, d.MQTT), d.Logger)
	}
	return c
}

// Run starts the poll loop and the write drain, and subscribes to /set
// commands. It blocks until ctx is cancelled or a component fails.
func (c *Coordinator) Run(ctx context.Context) error {
	c.PublishOnline(ctx)

	// Before anything is subscribed, and a hard failure rather than a warning:
	// a state topic that fell inside this daemon's own command filter would be
	// delivered straight back as a command it issued to itself, and there is no
	// safe way to run with that. It is a property of the layout and the
	// catalogue, so it cannot be transient — unlike the subscribe below, which
	// can fail on a broker hiccup and is retried by the reconnect.
	if err := c.checkCommandDisjoint(); err != nil {
		return err
	}
	if err := c.subscribeWrites(ctx); err != nil {
		c.deps.Logger.Warn("coordinator.subscribe_failed", slog.String("err", err.Error()))
	}
	// Drains the handlers already accepted; safe on a router that never
	// started.
	defer func() {
		stopCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer stop()
		if c.commands != nil {
			_ = c.commands.Stop(stopCtx)
		}
	}()
	c.subscribeLocal(ctx)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return c.pollLoop(gctx) })
	g.Go(func() error { return c.drainWrites(gctx) })
	g.Go(func() error { return c.drainLocalStates(gctx) })
	if c.deps.Cfg.LocalEnabled() {
		g.Go(func() error { return c.watchUpstream(gctx) })
	}
	if c.deps.Instance != nil {
		g.Go(func() error {
			// Off (MQTT_STATS_INTERVAL: 0, or maintenance disabled) is not a
			// failure; neither is the shutdown that ends the loop.
			_ = c.deps.Instance.RunStats(gctx)
			return nil
		})
	}
	return g.Wait()
}

// PublishOnline announces the instance on a broker connection. Wire this to
// the MQTT lifecycle OnConnect to re-announce after a reconnect.
//
// The order matters. The state plane re-sends every status item it holds
// first, byte for byte with its original `ts` (spec §3.2: published again after
// every broker reconnect, so a broker that came back without its retained
// store is complete again without waiting for each value to change). The
// discovery runtime is then rebuilt for the new connection (see
// [Coordinator.resetHAPlane]), so no bookkeeping survives a connection it was
// made on, and `<name>/connected` is announced at the level the daemon last
// decided — the fresh runtime starts at 1, so a daemon whose upstream is usable
// raises it to 2 at once. `<name>/info` follows (spec §6: on every connect).
//
// The discovery plane's own gate is re-opened with the runtime, and that is
// F16 of the phase 8 measurement. lastDiscSig is a statement about what a
// BROKER holds, and a broker restarted without its retained store holds nothing
// — so a gate that outlived the connection it was computed on meant the device
// documents never came back until the entity set happened to change. Clearing
// it here is safe precisely because the library has a second, byte-level gate
// underneath: an unchanged fleet re-renders and writes nothing.
//
// priorLoaded is cleared for the same reason in the other direction: the
// component sets the tombstone diff is taken against were read from the broker
// this connection replaced.
//
// The level itself is publisher.Runtime's: the same object produced the Last
// Will the broker publishes when this daemon dies ([RuntimeConfig]'s Layout).
// That pairing used to be three independent literals in three packages, which
// is how a sibling bridge ended up with a will no entity references.
func (c *Coordinator) PublishOnline(ctx context.Context) {
	if c.deps.StatePlane != nil {
		if _, err := c.deps.StatePlane.Republish(ctx); err != nil {
			c.deps.Logger.Warn("coordinator.state_republish_failed", slog.String("err", err.Error()))
		}
	}
	c.connMu.Lock()
	c.resetHAPlane()
	c.discoveryGen.Add(1)
	c.mu.Lock()
	c.lastDiscSig = ""
	c.priorLoaded = false
	c.priorComponents = nil
	c.mu.Unlock()
	if rt := c.ha(); rt != nil {
		var err error
		if c.connLevel == discovery.ConnectedOperational {
			_, err = rt.SetConnected(ctx, discovery.ConnectedOperational)
		} else {
			err = rt.AnnounceOnline(ctx)
		}
		if err != nil {
			c.deps.Logger.Warn("coordinator.publish_online_failed", slog.String("err", err.Error()))
		}
	}
	c.connMu.Unlock()
	if c.deps.Instance != nil {
		if err := c.deps.Instance.AnnounceInfo(ctx); err != nil {
			c.deps.Logger.Warn("coordinator.publish_info_failed", slog.String("err", err.Error()))
		}
	}
}

// PublishOffline writes `<name>/connected` = 0, the graceful-shutdown half of
// what the Last Will does on a crash (spec §3.1). A normal DISCONNECT discards
// the will, so without this a stopped daemon would leave its last level
// retained and every entity available.
func (c *Coordinator) PublishOffline(ctx context.Context) {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	rt := c.ha()
	if rt == nil {
		return
	}
	if err := rt.AnnounceOffline(ctx); err != nil {
		c.deps.Logger.Warn("coordinator.publish_offline_failed", slog.String("err", err.Error()))
	}
}

func (c *Coordinator) pollLoop(ctx context.Context) error {
	for {
		c.pollOnce(ctx)
		c.maybeSweepLegacy(ctx)
		interval := c.deps.Cfg.PollInterval(c.deps.Clock().Hour())
		c.deps.Logger.Debug("coordinator.poll_sleep", slog.Duration("interval", interval))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		case <-c.refresh:
			// Manual refresh (the HA button): poll now and restart the interval.
			c.deps.Logger.Info("coordinator.refresh_polling")
		}
	}
}

func (c *Coordinator) pollOnce(ctx context.Context) {
	c.mu.Lock()
	c.lastPoll = c.deps.Clock()
	c.mu.Unlock()

	data, err := c.deps.Client.GetDevices(ctx)
	if err != nil {
		switch {
		case errors.Is(err, client.ErrScanIgnore):
			// A deliberate skip after a write, not a statement about the
			// cloud: the level stays where the last real answer put it.
			c.deps.Logger.Debug("coordinator.scan_ignored")
			return
		case errors.Is(err, client.ErrRateLimited):
			c.deps.Logger.Warn("coordinator.rate_limited")
		case errors.Is(err, auth.ErrReauthRequired):
			c.deps.Logger.Error("coordinator.reauth_required",
				slog.String("hint", "run `daikin2mqtt-util auth` or use the web UI to re-authorize"))
		default:
			c.deps.Logger.Warn("coordinator.poll_failed", slog.String("err", err.Error()))
		}
		c.noteCloud(ctx, false)
		return
	}

	devices, err := model.ParseDevices(data)
	if err != nil {
		c.deps.Logger.Warn("coordinator.parse_failed", slog.String("err", err.Error()))
		c.noteCloud(ctx, false)
		return
	}
	c.noteCloud(ctx, true)

	c.updateModeCache(devices)
	c.updateOutdoorGroups(devices)
	// Drive the powerful⇄econo save/restore from the cloud snapshot for groups
	// not controlled locally (the Faikin read path owns the locally-active ones).
	c.reconcileEconoSuspendCloud(ctx, devices)
	// The embeddedID cache is now populated; (re)publish any Faikin state that
	// arrived before this (e.g. the retained state at subscribe time).
	if c.deps.Cfg.LocalEnabled() {
		c.flushLocalStates(ctx)
	}
	points := process.ResolveAt(devices, c.deps.Catalog, c.deps.Clock())
	c.noteBounds(points)

	// In local mode, surface settings Faikin provides but the cloud does not
	// expose (their live state arrives via the Faikin read path).
	if c.deps.Cfg.LocalEnabled() {
		points = append(points, c.localOnlyPoints(devices, points)...)
	}
	// The manual cloud-refresh button: a daemon action, not a device value.
	points = append(points, c.refreshPoints(devices)...)
	// The scheduler's per-device status sensors (only when it is enabled).
	points = append(points, c.schedulePoints(devices)...)

	if c.deps.HASS != nil {
		// What this instance polls, and therefore which retained discovery
		// configs it may ever claim as its own (F14). Stated before discovery
		// publishes, because the orphan reconcile that follows the publish is
		// what consumes it — and before the first poll has resolved anything
		// the claim set is empty and nothing is claimed at all.
		c.deps.HASS.ClaimDevices(c.claimedDeviceSegments(devices))
		infos := deviceInfos(devices)
		c.applyFaikinConfigURLs(infos)
		c.maybePublishDiscovery(ctx, points, infos, climateInfos(devices))
		// Each entity's data_source (cloud vs local Faikin), published every
		// poll rather than only when the discovery signature moves.
		//
		// It used to run inside maybePublishDiscovery, after the `changed`
		// gate. But data_source is a function of which PATH serves a device,
		// and switching paths does not change the point set — so it does not
		// change the signature, so the attribute was never republished. It
		// could be stale for as long as the point set was stable, which is
		// normally forever. That is F13 of the ADR 0070 phase 8 measurement.
		//
		// Latent rather than live today, because localActiveFor is a function
		// of the static LOCAL_DEVICE_MAP; it goes live the moment a
		// fall-back-to-Faikin-on-timeout behaviour is added. The documents are
		// retained and byte-identical between polls, so republishing them
		// costs one retained write per entity per poll and moves no byte a
		// subscriber sees.
		c.publishDataSources(ctx, points)
	}

	published, written := 0, 0
	for i := range points {
		p := points[i]
		// Buttons are stateless (command-only), so there is nothing to publish.
		if p.Entry.Platform == "button" {
			continue
		}
		// In local mode the Faikin path owns these per-unit topics for mapped
		// devices; skip them here to avoid redundant (and slower) cloud writes.
		if localTopics[p.Topic] && c.localActiveFor(p.DeviceID) {
			continue
		}
		topic := c.topicRoot.Slot(p.DeviceID, p.EmbeddedID, p.Topic).State()
		w, ok := c.publishState(ctx, topic, c.statusValue(p.Topic, p.Value))
		if !ok {
			continue
		}
		published++
		if w {
			written++
		}
	}
	c.publishHVACModes(ctx, points)
	c.publishClimateAux(ctx, devices)
	c.publishCloudOnline(ctx, devices)
	// points is what this poll had to say; written is what actually reached the
	// broker. The gap is the state plane's dedup gate, and a steady-state
	// installation should show written far below points — a value that has not
	// moved costs one comparison instead of one retained write and one Home
	// Assistant state evaluation, forever.
	c.deps.Logger.Info("coordinator.published",
		slog.Int("devices", len(devices)), slog.Int("points", published), slog.Int("written", written))

	// The device caches (climateEmbedded, modeCache) are now populated, so a
	// schedule block that could not be applied before can be applied now. The
	// engine re-evaluates idempotently, so this costs nothing when nothing is due.
	if eng := c.scheduleEngine(); eng != nil {
		eng.Wake()
	}

	c.mu.Lock()
	c.lastDevices = c.claimedDeviceSegments(devices)
	c.mu.Unlock()
}

// climateState accumulates the inputs to the combined HA hvac mode.
type climateState struct {
	deviceID, embeddedID string
	power, mode          string
	hasMode              bool
}

// publishHVACModes publishes the synthetic combined hvac-mode state for each
// climateControl management point (computed from onOffMode + operationMode),
// backing the combined HA climate entity.
func (c *Coordinator) publishHVACModes(ctx context.Context, points []process.Point) {
	groups := map[string]*climateState{}
	var order []string
	for i := range points {
		p := points[i]
		if p.MPType != "climateControl" {
			continue
		}
		key := p.DeviceID + "|" + p.EmbeddedID
		g := groups[key]
		if g == nil {
			g = &climateState{deviceID: p.DeviceID, embeddedID: p.EmbeddedID}
			groups[key] = g
			order = append(order, key)
		}
		switch p.Topic {
		case "power":
			if s, ok := p.Value.(string); ok {
				g.power = s
			}
		case "operation_mode":
			if s, ok := p.Value.(string); ok {
				g.mode = s
				g.hasMode = true
			}
		}
	}
	for _, key := range order {
		g := groups[key]
		if !g.hasMode {
			continue
		}
		topic := c.topicRoot.Slot(g.deviceID, g.embeddedID, hass.HVACModeTopic).State()
		c.publishState(ctx, topic, hass.HVACMode(g.power, g.mode))
	}
}

// statusValue is the `val` of a status item: the value as mqtt-smarthome 2.0
// §5.1 would publish it plain, typed as JSON.
//
// The internal vocabulary stays what the API and Faikin speak — "on"/"off",
// operationMode codes, held strings for the outdoor optimistic hold — and is
// converted here, once, by the catalogue entry of the leaf:
//
//   - a switch or binary sensor becomes a JSON boolean (ADR 0083: power on/off
//     and the binary sensors stop being words);
//   - a number becomes a JSON number, rounded to the entry's precision — the
//     same rounding the plain payload had, which stays a project concern;
//   - an enum stays its token (the API code). The localized label is no longer
//     published; Home Assistant gets it from discovery.
//
// A leaf no catalogue entry backs — the climate's hvac/fan/swing/preset
// tokens, the attributes documents — passes through unchanged.
func (c *Coordinator) statusValue(leaf string, v any) any {
	var entry *catalog.Entry
	if c.deps.Catalog != nil {
		entry, _ = c.deps.Catalog.ByTopic(leaf)
	}
	if entry == nil {
		return v
	}
	switch x := v.(type) {
	case float64:
		return roundTo(x, entry.Precision)
	case string:
		switch entry.Platform {
		case "switch", "binary_sensor":
			if b, err := (publisher.SetValue{Text: x}).Bool(); err == nil {
				return b
			}
		case "number":
			if f, err := strconv.ParseFloat(x, 64); err == nil {
				return roundTo(f, entry.Precision)
			}
		}
	}
	return v
}

// roundTo rounds v to prec decimals through the same formatting the plain
// payload used, so a status object never carries a binary residue the plain
// form did not show.
func roundTo(v float64, prec int) float64 {
	r, err := strconv.ParseFloat(strconv.FormatFloat(v, 'f', prec, 64), 64)
	if err != nil {
		return v
	}
	return r
}

// updateModeCache records each climate point's current operationMode (resolves
// mode-scoped PATCH paths) and onOffMode (mode sync must not touch units that
// are off). For locally-controlled devices the cloud values only bootstrap a
// missing entry: the Faikin read path and the write path feed fresher values,
// and the lagging cloud snapshot must not overwrite them — a stale "on" would
// let the mode sync write to (and thereby wake) a unit just switched off.
func (c *Coordinator) updateModeCache(devices []model.Device) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range devices {
		local := c.localActiveFor(d.ID)
		for _, mp := range d.ManagementPoints {
			ch, ok := mp.Characteristics["operationMode"]
			if !ok {
				continue
			}
			if s, ok := ch.String(); ok {
				key := d.ID + "/" + mp.EmbeddedID
				if _, have := c.modeCache[key]; !have || !local {
					c.modeCache[key] = s
				}
			}
			if s, ok := mp.Characteristics["onOffMode"].String(); ok {
				if _, have := c.powerCache[d.ID]; !have || !local {
					c.powerCache[d.ID] = s == "on"
				}
			}
			// The presence of operationMode marks the climateControl point;
			// remember its embeddedID so the local read path can route
			// Faikin state onto the same per-unit topics.
			c.climateEmbedded[d.ID] = mp.EmbeddedID
		}
	}
}

// reconcileCollectWindow is how long a snapshot subscription listens before it
// judges what is retained. Two seconds, the same value publisher.Sweep defaults
// to, stated once so the hand-rolled pass and the library's report-only one
// cannot answer the question over different windows.
//
// It is copied into each Coordinator at construction rather than read from the
// package: a test that shrinks it — two seconds of real time per sweep
// assertion is two seconds a reviewer pays on every run — would otherwise be
// writing a global that another test's reconcile goroutine is reading, which
// the race detector correctly calls a race.
const reconcileCollectWindow = 2 * time.Second

// publishDataSources publishes a {"data_source": "cloud"|"local"} JSON-attributes
// document for every entity, so Home Assistant shows where each value comes from.
func (c *Coordinator) publishDataSources(ctx context.Context, points []process.Point) {
	climateSeen := map[string]bool{}
	for i := range points {
		p := points[i]
		c.publishAttrs(ctx, c.deps.HASS.AttributesTopic(p), c.dataSource(p.DeviceID, p.Topic))
		if p.MPType != "climateControl" {
			continue
		}
		key := p.DeviceID + "|" + p.EmbeddedID
		if climateSeen[key] {
			continue
		}
		climateSeen[key] = true
		src := "cloud"
		if c.localActiveFor(p.DeviceID) {
			src = "local"
		}
		c.publishAttrs(ctx, c.deps.HASS.ClimateAttributesTopic(p.DeviceID, p.EmbeddedID), src)
	}
}

// publishAttrs publishes a retained data_source attributes item. The document
// is the status object's `val`; discovery hands it to Home Assistant through
// hass.AttributesTemplate.
func (c *Coordinator) publishAttrs(ctx context.Context, topic, source string) {
	c.publishState(ctx, topic, map[string]string{"data_source": source})
}

func (c *Coordinator) subscribeWrites(ctx context.Context) error {
	filter := c.topicRoot.CommandFilter()
	if c.commands == nil {
		return nil
	}
	// Registered before Start, and the router refuses two routes that both
	// match some topic, because a broker sends one copy per matching
	// subscription and the handler would run twice per message. This bridge
	// registers the set route and the instance's maintenance route, which are
	// disjoint by their second level.
	//
	// HandleSet is mqtt-smarthome 2.0 §5.3's normaliser: `{"val": x}` arrives
	// as x, an empty payload never arrives, malformed JSON is logged at warn
	// with topic and payload. The router also drops retained sets.
	if err := c.commands.HandleSet(filter, func(_ context.Context, cmd publisher.Command, v publisher.SetValue) {
		req, ok := c.parseSetTopic(cmd)
		if !ok {
			return
		}
		if v.Structured() {
			// No item of this bridge takes structured parameters.
			c.deps.Logger.Warn("coordinator.write_bad_value",
				slog.String("topic", cmd.Topic), slog.String("payload", string(cmd.Payload)))
			return
		}
		req.payload = v.Text
		select {
		case c.writes <- req:
		default:
			c.deps.Logger.Warn("coordinator.write_queue_full",
				slog.String("topic", cmd.Topic), slog.String("payload", string(cmd.Payload)))
		}
	}); err != nil && !errors.Is(err, publisher.ErrDuplicateRoute) {
		return err
	}
	if c.deps.Instance != nil {
		if err := c.deps.Instance.Register(c.commands); err != nil && !errors.Is(err, publisher.ErrDuplicateRoute) {
			return err
		}
	}
	return c.commands.Start(ctx)
}

// checkCommandDisjoint asserts that nothing this daemon publishes lands inside
// its own command subscription.
//
// The topics are built from the same layout the publish path uses, over the
// planes this daemon actually writes: the connected level, the instance info,
// and the status and attributes items of every slot it knows about plus the
// devices' online items. The router's own CheckDisjoint does the matching, so
// the answer comes from the library's filter semantics rather than from a
// string comparison written here.
func (c *Coordinator) checkCommandDisjoint() error {
	if c.commands == nil || c.deps.Catalog == nil {
		return nil
	}
	topics := []string{c.topicRoot.Connected(), c.topicRoot.Info(), c.topicRoot.Online("probe-device")}
	for _, s := range c.knownSlots() {
		topics = append(topics, s.State(), s.Attributes())
	}
	if err := c.commands.CheckDisjoint(topics...); err != nil {
		return fmt.Errorf("coordinator: what this daemon publishes is not disjoint from what it subscribes: %w", err)
	}
	return nil
}

// claimedDeviceSegments is the set of state-plane device segments this instance
// writes, and therefore the set [hass.Discovery.IsOwnConfig] and
// [hass.Discovery.BundleIsOwnConfig] take ownership from: the ONECTA device ids
// this poll resolved, and nothing else.
//
// The scheduler's reserved segment is deliberately NOT in it, and that omission
// is the whole of F-A.
//
// [layout.SchedulerDeviceID] is a compile-time literal ("scheduler"), and the
// scheduler's Home Assistant device node id is likewise a constant
// ("daikin_scheduler"). So two instances that both run a scheduler — with
// entirely DISJOINT schedule ids, on disjoint ONECTA accounts — publish their
// schedule switches to the same device-document topic and under the same
// `<name>/status/scheduler/…` item. Claiming that segment made every one of
// those topics resolve as "ours", so a sibling's live schedule switches passed
// both payload predicates: its document became this instance's tombstone
// prior state, its switches were marked removed, and its per-entity configs
// were retracted. That is the hole F18 closed for the other 262 configs, and it
// stayed open for these two until it was closed here.
//
// Not claiming it is byte-neutral — the set is read by the two ownership
// predicates and by nothing that publishes — and it costs exactly one thing:
// the sweep no longer retracts a DELETED schedule's retained per-entity config,
// and the broker read-back no longer tombstones a schedule component. The
// in-process memo still does (see [Coordinator.recordPublished]), which covers
// deleting a schedule through the daemon's own web UI — the way schedules are
// actually deleted. A schedule removed while the daemon is stopped leaves a
// phantom switch. Leaving an orphan is recoverable; deleting a sibling's live
// entities is not.
//
// What remains open is F14 proper: two instances on the SAME ONECTA account
// with different LOCAL_MODE or characteristics.yaml claim the same device ids
// legitimately, and no predicate can separate them — a shrunken component set
// is indistinguishable from this instance's own configuration change. That
// needs an instance identifier (step 3(c)) and is pinned rather than fixed; see
// TestTwoInstancesOnOneAccountStillOverwriteEachOther.
func (c *Coordinator) claimedDeviceSegments(devices []model.Device) []string {
	out := make([]string, 0, len(devices))
	for i := range devices {
		out = append(out, devices[i].ID)
	}
	return out
}

// syntheticTopics are the leaf segments this daemon publishes that no catalogue
// entry backs: the composite climate's five slots, the refresh button and the
// scheduler's four sensors.
func syntheticTopics() []string {
	return []string{
		hass.HVACModeTopic, hass.FanModeTopic, hass.SwingModeTopic,
		hass.SwingHModeTopic, hass.PresetModeTopic, hass.RefreshTopic,
		ScheduleStateTopic, ScheduleNextTopic,
		OutdoorScheduleStateTopic, OutdoorScheduleNextTopic,
	}
}

// knownSlots is every slot this daemon can publish to, from the catalogue and
// the synthetic points, for one representative device/management point plus
// whatever the last poll resolved. It exists for checkCommandDisjoint: the
// question is structural — all these topics share the four-level shape the
// command filter wildcards — so a representative set answers it exactly.
func (c *Coordinator) knownSlots() []layout.Slot {
	var out []layout.Slot
	add := func(dev, emb, topic string) { out = append(out, c.topicRoot.Slot(dev, emb, topic)) }
	const probeDev, probeEmb = "probe-device", "climateControl"
	entries := c.deps.Catalog.Entries()
	for i := range entries {
		add(probeDev, probeEmb, entries[i].Topic)
	}
	for _, t := range syntheticTopics() {
		add(probeDev, probeEmb, t)
	}
	out = append(out, c.topicRoot.Climate(probeDev, probeEmb), c.topicRoot.Schedule("probe-schedule"))
	c.mu.Lock()
	for dev, emb := range c.climateEmbedded {
		add(dev, emb, "power")
	}
	c.mu.Unlock()
	return out
}

// parseSetTopic extracts the device/embedded/topic from a set item: the three
// `+` levels of "<name>/set/+/+/+", in order.
func (c *Coordinator) parseSetTopic(cmd publisher.Command) (writeReq, bool) {
	if len(cmd.Wildcards) != 3 {
		return writeReq{}, false
	}
	return writeReq{deviceID: cmd.Wildcards[0], embeddedID: cmd.Wildcards[1], topic: cmd.Wildcards[2]}, true
}

// setTopic is the set item a request addresses, for the warn line spec §3.3
// asks of a rejected or failed request. A scheduled write is logged under the
// item it would have arrived on, so the two kinds read the same.
func (c *Coordinator) setTopic(req writeReq) string {
	return c.topicRoot.Slot(req.deviceID, req.embeddedID, req.topic).Command()
}

// rejectWrite logs a rejected or failed set at warn with its topic and payload
// (spec §3.3), plus whatever the caller adds.
func (c *Coordinator) rejectWrite(event string, req writeReq, attrs ...any) {
	c.deps.Logger.Warn(event, append([]any{
		slog.String("topic", c.setTopic(req)), slog.String("payload", req.payload),
	}, attrs...)...)
}

func (c *Coordinator) drainWrites(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case req := <-c.writes:
			c.handleWrite(ctx, req)
		}
	}
}

func (c *Coordinator) handleWrite(ctx context.Context, req writeReq) {
	// The reserved "scheduler" item carries the daemon's own schedule
	// switches, not a Daikin device. Its topics fit the same set filter, so
	// they arrive here and are branched off before any catalog lookup.
	if req.deviceID == schedule.SchedulerDeviceID {
		c.handleSchedulerWrite(req)
		return
	}

	// Synthetic climate-entity topics map to onOffMode/operationMode/fanControl/
	// powerfulMode rather than a single catalog characteristic.
	switch req.topic {
	case hass.RefreshTopic:
		// An action item: any non-empty payload fires it. It writes nothing to
		// the device — it re-reads the cloud.
		c.requestRefresh()
		return
	case hass.HVACModeTopic:
		c.handleHVACModeWrite(ctx, req)
		return
	case hass.FanModeTopic:
		c.handleFanModeWrite(ctx, req)
		return
	case hass.SwingModeTopic:
		c.handleSwingWrite(ctx, req, "vertical")
		return
	case hass.SwingHModeTopic:
		c.handleSwingWrite(ctx, req, "horizontal")
		return
	case hass.PresetModeTopic:
		c.handlePresetWrite(ctx, req)
		return
	}

	entry, ok := c.deps.Catalog.ByTopic(req.topic)
	if !ok {
		c.rejectWrite("coordinator.write_unknown_topic", req)
		return
	}
	if !entry.Settable {
		c.rejectWrite("coordinator.write_not_settable", req)
		return
	}

	value, ok := c.coerceWriteValue(entry, req)
	if !ok {
		c.rejectWrite("coordinator.write_bad_value", req)
		return
	}

	path := entry.Path
	if strings.Contains(path, "{mode}") {
		mode, _ := c.cachedMode(req.deviceID, req.embeddedID)
		if mode == "" {
			c.rejectWrite("coordinator.write_no_mode", req)
			return
		}
		path = strings.ReplaceAll(path, "{mode}", mode)
	}

	if err := c.setCharacteristic(ctx, req.deviceID, req.embeddedID, entry.Match.Characteristic, value, path); err != nil {
		c.rejectWrite("coordinator.patch_failed", req, slog.String("err", err.Error()))
		return
	}
	c.deps.Logger.Info("coordinator.patched",
		slog.String("device", req.deviceID), slog.String("characteristic", entry.Match.Characteristic),
		slog.String("value", req.payload))

	// Outdoor-shared settings apply to every indoor unit of the outdoor unit.
	if entry.Scope == "outdoor" && c.deps.Cfg.OutdoorAggregateEnabled() {
		c.fanOutToGroup(ctx, req.deviceID, entry.Match.Characteristic, value, path)
		// Reflect the change on every member and hold it until a Faikin status
		// confirms it, so the sparse/lagging status from the idle indoor units
		// cannot snap the toggle back before the active unit reports the new
		// value. This runs only AFTER the write above succeeded — the Faikin
		// command was accepted — which is spec §3.3's "the adapter's own state
		// after the action succeeded", never an echo of the request.
		if c.localActiveFor(req.deviceID) {
			held := heldString(value)
			c.holdOutdoor(req.deviceID, req.topic, held)
			c.publishOptimistic(ctx, req.deviceID, req.topic, held)
		}
	}
	// Mutually-exclusive partners (powerful ⇄ econo) are cleared on enable.
	c.enforceMutualExclusive(ctx, req.deviceID, req.embeddedID, entry.Match.Characteristic, value)
}

// heldString is a written value in the vocabulary the outdoor hold compares
// against the Faikin aggregate: "on"/"off" for a switch, the plain number for
// a number.
func heldString(v any) string {
	switch x := v.(type) {
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

// handleHVACModeWrite applies an HA climate hvac-mode command: "off" turns
// the unit off; any other mode turns it on and sets the mapped operationMode.
// The token is matched without regard to case (spec §5.3).
func (c *Coordinator) handleHVACModeWrite(ctx context.Context, req writeReq) {
	ha := strings.ToLower(strings.TrimSpace(req.payload))
	patch := func(characteristic string, value any) bool {
		if err := c.setCharacteristic(ctx, req.deviceID, req.embeddedID, characteristic, value, ""); err != nil {
			c.rejectWrite("coordinator.patch_failed", req,
				slog.String("characteristic", characteristic), slog.String("err", err.Error()))
			return false
		}
		return true
	}

	if ha == "off" {
		if patch("onOffMode", "off") {
			c.deps.Logger.Info("coordinator.patched",
				slog.String("device", req.deviceID), slog.String("hvac_mode", "off"))
		}
		return
	}

	daikinMode, ok := hass.DaikinModeForHA(ha)
	if !ok {
		c.rejectWrite("coordinator.write_bad_hvac_mode", req)
		return
	}
	if !patch("onOffMode", "on") {
		return
	}
	if patch("operationMode", daikinMode) {
		c.deps.Logger.Info("coordinator.patched",
			slog.String("device", req.deviceID), slog.String("hvac_mode", ha),
			slog.String("operationMode", daikinMode))
		// Keep the whole outdoor unit on one mode (multi-split constraint).
		c.syncModeToGroup(ctx, req.deviceID, daikinMode)
	}
}

// coerceWriteValue maps a set value to the cloud value type, with the
// conversions mqtt-smarthome 2.0 §5.3 asks for:
//
//   - a switch reads true/false, 1/0, on/off, yes/no in any case and becomes
//     the API's "on"/"off";
//   - a select reads its token (the API code) in any case, and keeps
//     accepting a label in either language, which is what this bridge's
//     selects carried on the wire before 0.14;
//   - a number is rounded to the point's live step and clamped to its live
//     range when the last poll reported one;
//   - anything else passes through.
//
// ok is false for a value that does not convert; the caller logs it at warn.
func (c *Coordinator) coerceWriteValue(entry *catalog.Entry, req writeReq) (any, bool) {
	v := publisher.SetValue{Text: strings.TrimSpace(req.payload)}
	switch entry.Platform {
	case "number":
		b := c.numberBounds(req)
		f, err := v.Number(deref(b.min, math.Inf(-1)), deref(b.max, math.Inf(1)), deref(b.step, 0))
		if err != nil {
			return nil, false
		}
		return f, true
	case "switch":
		on, err := v.Bool()
		if err != nil {
			return nil, false
		}
		return onOff(on), true
	case "select":
		code, err := v.Enum(entryEnum(entry), true)
		if err != nil {
			return nil, false
		}
		return code, true
	default:
		return v.Text, true
	}
}

// entryEnum is a catalogue entry's values as a model.Enum, codes in declaration
// order and both languages' labels, which is what SetValue.Enum matches.
func entryEnum(entry *catalog.Entry) *hamodel.Enum {
	e := &hamodel.Enum{Labels: make(map[string]hamodel.Localized, len(entry.Values))}
	for _, v := range entry.Values {
		e.Codes = append(e.Codes, v.Value)
		e.Labels[v.Value] = hamodel.Localized{Default: v.Label, Lang: map[string]string{"de": v.LabelDE}}
	}
	return e
}

// numberBounds returns the live range the last poll reported for the
// request's point, or none.
func (c *Coordinator) numberBounds(req writeReq) numberBounds {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bounds[req.deviceID+"|"+req.embeddedID+"|"+req.topic]
}

// noteBounds records the live min/max/step of every number point, for the
// clamping in [Coordinator.coerceWriteValue].
func (c *Coordinator) noteBounds(points []process.Point) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range points {
		p := &points[i]
		if p.Entry.Platform != "number" || (p.Min == nil && p.Max == nil && p.Step == nil) {
			continue
		}
		c.bounds[p.DeviceID+"|"+p.EmbeddedID+"|"+p.Topic] = numberBounds{min: p.Min, max: p.Max, step: p.Step}
	}
}

func deref(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

// deviceInfos builds rich HA device metadata per device from its management
// points: the friendly name + indoor unit details for the main device, and
// gateway / outdoor unit details for the nested sub-devices.
func deviceInfos(devices []model.Device) map[string]hass.DeviceInfo {
	out := make(map[string]hass.DeviceInfo, len(devices))
	for _, d := range devices {
		info := hass.DeviceInfo{ModelID: d.Model}
		str := func(mp model.ManagementPoint, name string) string {
			if ch, ok := mp.Characteristics[name]; ok {
				if s, ok := ch.String(); ok {
					return s
				}
			}
			return ""
		}
		for _, mp := range d.ManagementPoints {
			switch mp.Type {
			case "climateControl", "domesticHotWaterTank":
				if n := str(mp, "name"); n != "" && info.Name == "" {
					info.Name = n
				}
			case "indoorUnit", "indoorUnitHydro":
				if v := str(mp, "modelInfo"); v != "" {
					info.Model = v
				}
				if v := str(mp, "serialNumber"); v != "" {
					info.SerialNumber = v
				}
				if v := str(mp, "softwareVersion"); v != "" {
					info.SWVersion = v
				}
			case "gateway":
				info.Gateway = &hass.SubDevice{
					Model:        str(mp, "modelInfo"),
					SWVersion:    str(mp, "firmwareVersion"),
					SerialNumber: str(mp, "serialNumber"),
					MAC:          str(mp, "macAddress"),
				}
			case "outdoorUnit":
				info.Outdoor = &hass.SubDevice{
					Model:        str(mp, "modelInfo"),
					SWVersion:    str(mp, "softwareVersion"),
					SerialNumber: str(mp, "serialNumber"),
				}
			}
		}
		out[d.ID] = info
	}
	return out
}

// discoverySignature is a cheap fingerprint of the published entity set.
func discoverySignature(points []process.Point) string {
	var b strings.Builder
	for i := range points {
		p := &points[i]
		b.WriteString(p.DeviceID)
		b.WriteByte('/')
		b.WriteString(p.Topic)
		b.WriteByte(';')
	}
	return b.String()
}
