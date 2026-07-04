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
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/auth"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/client"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
	"github.com/SukramJ/go-daikin2mqtt/internal/faikin"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/process"
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
}

// Coordinator owns the poll/publish/write loops.
type Coordinator struct {
	deps      Deps
	topicRoot string

	writes chan writeReq

	mu              sync.Mutex
	modeCache       map[string]string            // deviceID/embeddedID -> current operationMode
	climateEmbedded map[string]string            // deviceID -> climateControl embeddedID (for local routing)
	outdoorSerial   map[string]string            // deviceID -> outdoor-unit serial (for multi-split grouping)
	lastLocal       map[string]*faikin.State     // deviceID -> last AC state received from Faikin
	lastEnergy      map[string]energyTotals      // deviceID -> last non-zero per-unit energy (held; see localOutdoorAgg)
	pendingOutdoor  map[string]outdoorHold       // "<group>|<topic>" -> just-written value, held until confirmed
	econoSuspend    map[string]econoSuspendState // outdoor group key -> powerful<->econo save/restore state
	lastDiscSig     string                       // signature of the last published discovery set
	reconcileGate   sync.Mutex                   // try-locked gate so only one orphan reconcile runs at a time
}

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

// New builds a coordinator.
func New(d Deps) *Coordinator {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	return &Coordinator{
		deps:            d,
		topicRoot:       d.Cfg.MQTTTopic,
		writes:          make(chan writeReq, 64),
		modeCache:       map[string]string{},
		climateEmbedded: map[string]string{},
		outdoorSerial:   map[string]string{},
		lastLocal:       map[string]*faikin.State{},
		lastEnergy:      map[string]energyTotals{},
		pendingOutdoor:  map[string]outdoorHold{},
		econoSuspend:    map[string]econoSuspendState{},
	}
}

// Run starts the poll loop and the write drain, and subscribes to /set
// commands. It blocks until ctx is cancelled or a component fails.
func (c *Coordinator) Run(ctx context.Context) error {
	c.PublishOnline(ctx)

	if err := c.subscribeWrites(ctx); err != nil {
		c.deps.Logger.Warn("coordinator.subscribe_failed", slog.String("err", err.Error()))
	}
	c.subscribeLocal(ctx)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return c.pollLoop(gctx) })
	g.Go(func() error { return c.drainWrites(gctx) })
	return g.Wait()
}

// PublishOnline marks the bridge available (retained). Wire this to the MQTT
// lifecycle OnConnect to re-announce after a reconnect.
func (c *Coordinator) PublishOnline(ctx context.Context) {
	topic := c.topicRoot + "/bridge/status"
	if err := c.deps.MQTT.Publish(ctx, topic, []byte("online"), mqtt.QoS0, true); err != nil {
		c.deps.Logger.Warn("coordinator.publish_online_failed", slog.String("err", err.Error()))
	}
}

func (c *Coordinator) pollLoop(ctx context.Context) error {
	for {
		c.pollOnce(ctx)
		interval := c.deps.Cfg.PollInterval(c.deps.Clock().Hour())
		c.deps.Logger.Debug("coordinator.poll_sleep", slog.Duration("interval", interval))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

func (c *Coordinator) pollOnce(ctx context.Context) {
	data, err := c.deps.Client.GetDevices(ctx)
	if err != nil {
		switch {
		case errors.Is(err, client.ErrScanIgnore):
			c.deps.Logger.Debug("coordinator.scan_ignored")
		case errors.Is(err, client.ErrRateLimited):
			c.deps.Logger.Warn("coordinator.rate_limited")
		case errors.Is(err, auth.ErrReauthRequired):
			c.deps.Logger.Error("coordinator.reauth_required",
				slog.String("hint", "run `daikin2mqtt-util auth` or use the web UI to re-authorize"))
		default:
			c.deps.Logger.Warn("coordinator.poll_failed", slog.String("err", err.Error()))
		}
		return
	}

	devices, err := model.ParseDevices(data)
	if err != nil {
		c.deps.Logger.Warn("coordinator.parse_failed", slog.String("err", err.Error()))
		return
	}

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

	// In local mode, surface settings Faikin provides but the cloud does not
	// expose (their live state arrives via the Faikin read path).
	if c.deps.Cfg.LocalEnabled() {
		points = append(points, c.localOnlyPoints(devices, points)...)
	}

	if c.deps.HASS != nil {
		infos := deviceInfos(devices)
		c.applyFaikinConfigURLs(infos)
		c.maybePublishDiscovery(ctx, points, infos, climateInfos(devices, c.deps.Cfg.Language))
	}

	published := 0
	for i := range points {
		p := points[i]
		// In local mode the Faikin path owns these per-unit topics for mapped
		// devices; skip them here to avoid redundant (and slower) cloud writes.
		if localTopics[p.Topic] && c.localActiveFor(p.DeviceID) {
			continue
		}
		topic := fmt.Sprintf("%s/%s/%s/%s/state", c.topicRoot, p.DeviceID, p.EmbeddedID, p.Topic)
		if err := c.deps.MQTT.Publish(ctx, topic, []byte(c.formatValue(p)), mqtt.QoS0, true); err != nil {
			c.deps.Logger.Warn("coordinator.publish_failed",
				slog.String("topic", topic), slog.String("err", err.Error()))
			continue
		}
		published++
	}
	c.publishHVACModes(ctx, points)
	c.publishClimateAux(ctx, devices)
	c.deps.Logger.Info("coordinator.published",
		slog.Int("devices", len(devices)), slog.Int("points", published))
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
		topic := fmt.Sprintf("%s/%s/%s/%s/state", c.topicRoot, g.deviceID, g.embeddedID, hass.HVACModeTopic)
		payload := hass.HVACMode(g.power, g.mode)
		if err := c.deps.MQTT.Publish(ctx, topic, []byte(payload), mqtt.QoS0, true); err != nil {
			c.deps.Logger.Warn("coordinator.publish_hvac_failed",
				slog.String("topic", topic), slog.String("err", err.Error()))
		}
	}
}

// formatValue renders a point's MQTT state payload. For select entities the
// raw API code is mapped to the localized label so the HA dropdown (whose
// options are localized labels) shows the current selection; the write path
// maps the label back to the code.
func (c *Coordinator) formatValue(p process.Point) string {
	if p.Entry.Platform == "select" {
		if s, ok := p.Value.(string); ok {
			return p.Entry.LocalizedLabel(s, c.deps.Cfg.Language)
		}
	}
	return p.Format()
}

// updateModeCache records the current operationMode of each climate point so
// the write path can resolve mode-scoped setpoint PATCH paths.
func (c *Coordinator) updateModeCache(devices []model.Device) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range devices {
		for _, mp := range d.ManagementPoints {
			if ch, ok := mp.Characteristics["operationMode"]; ok {
				if s, ok := ch.String(); ok {
					c.modeCache[d.ID+"/"+mp.EmbeddedID] = s
				}
				// The presence of operationMode marks the climateControl point;
				// remember its embeddedID so the local read path can route
				// Faikin state onto the same per-unit topics.
				c.climateEmbedded[d.ID] = mp.EmbeddedID
			}
		}
	}
}

// maybePublishDiscovery (re)publishes discovery only when the topic set
// changed, since configs are retained.
func (c *Coordinator) maybePublishDiscovery(ctx context.Context, points []process.Point, infos map[string]hass.DeviceInfo, climateInfos map[string]hass.ClimateInfo) {
	sig := discoverySignature(points)
	c.mu.Lock()
	changed := sig != c.lastDiscSig
	c.lastDiscSig = sig
	c.mu.Unlock()
	if !changed {
		return
	}
	published, err := c.deps.HASS.Publish(ctx, points, infos, climateInfos)
	if err != nil {
		c.deps.Logger.Warn("coordinator.discovery_failed", slog.String("err", err.Error()))
		return
	}
	c.deps.Logger.Info("coordinator.discovery_published", slog.Int("entities", len(points)))
	// Publish each entity's data_source (cloud vs local Faikin) alongside the
	// (retained) discovery, so it shows as an entity attribute.
	c.publishDataSources(ctx, points)
	// Clear any of our own retained discovery configs that we no longer publish
	// (entities removed or moved/renamed across versions), so they don't linger
	// as unavailable entities in Home Assistant.
	c.reconcileOrphans(ctx, published)
}

// reconcileOrphans clears this daemon's retained discovery configs that are no
// longer in the published set. It collects the retained configs under the HA
// discovery prefix, then clears ours (IsOwnConfig) that are absent from
// published. Runs asynchronously; a second call while one is in flight is
// skipped (the gate), since discovery changes are infrequent.
func (c *Coordinator) reconcileOrphans(ctx context.Context, published map[string]bool) {
	if c.deps.HASS == nil || !c.reconcileGate.TryLock() {
		return
	}
	go func() {
		defer c.reconcileGate.Unlock()
		filter := c.deps.HASS.ConfigFilter()
		var mu sync.Mutex
		retained := map[string][]byte{}
		if _, err := c.deps.MQTT.Subscribe(ctx, filter, mqtt.QoS0, func(msg *mqtt.Message) {
			mu.Lock()
			retained[msg.Topic] = append([]byte(nil), msg.Payload...)
			mu.Unlock()
		}); err != nil {
			c.deps.Logger.Warn("coordinator.reconcile_subscribe_failed", slog.String("err", err.Error()))
			return
		}
		// Retained configs are delivered right after subscribe; collect briefly.
		select {
		case <-ctx.Done():
			_ = c.deps.MQTT.Unsubscribe(ctx, filter)
			return
		case <-time.After(2 * time.Second):
		}
		_ = c.deps.MQTT.Unsubscribe(ctx, filter)

		mu.Lock()
		cleared := c.clearOrphanConfigs(ctx, retained, published)
		mu.Unlock()
		if cleared > 0 {
			c.deps.Logger.Info("coordinator.discovery_orphans_cleared", slog.Int("count", cleared))
		}
	}()
}

// clearOrphanConfigs clears each retained config that is ours (IsOwnConfig) and
// absent from the published set, returning how many were cleared.
func (c *Coordinator) clearOrphanConfigs(ctx context.Context, retained map[string][]byte, published map[string]bool) int {
	cleared := 0
	for topic, payload := range retained {
		if len(payload) == 0 || published[topic] {
			continue // already cleared, or still a current entity
		}
		if !c.deps.HASS.IsOwnConfig(payload) {
			continue // belongs to another integration — never touch it
		}
		if err := c.deps.MQTT.Publish(ctx, topic, nil, mqtt.QoS0, true); err == nil {
			cleared++
		}
	}
	return cleared
}

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

// publishAttrs publishes a retained data_source attributes document.
func (c *Coordinator) publishAttrs(ctx context.Context, topic, source string) {
	payload := `{"data_source":"` + source + `"}`
	if err := c.deps.MQTT.Publish(ctx, topic, []byte(payload), mqtt.QoS0, true); err != nil {
		c.deps.Logger.Warn("coordinator.attrs_publish_failed",
			slog.String("topic", topic), slog.String("err", err.Error()))
	}
}

func (c *Coordinator) subscribeWrites(ctx context.Context) error {
	filter := c.topicRoot + "/+/+/+/set"
	_, err := c.deps.MQTT.Subscribe(ctx, filter, mqtt.QoS0, func(msg *mqtt.Message) {
		req, ok := c.parseSetTopic(msg.Topic, string(msg.Payload))
		if !ok {
			return
		}
		select {
		case c.writes <- req:
		default:
			c.deps.Logger.Warn("coordinator.write_queue_full", slog.String("topic", msg.Topic))
		}
	})
	return err
}

// parseSetTopic extracts the device/embedded/topic from a /set topic.
func (c *Coordinator) parseSetTopic(topic, payload string) (writeReq, bool) {
	parts := strings.Split(topic, "/")
	// <root>/<deviceId>/<embeddedId>/<topic>/set
	if len(parts) != 5 || parts[0] != c.topicRoot || parts[4] != "set" {
		return writeReq{}, false
	}
	return writeReq{deviceID: parts[1], embeddedID: parts[2], topic: parts[3], payload: payload}, true
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
	// Synthetic climate-entity topics map to onOffMode/operationMode/fanControl/
	// powerfulMode rather than a single catalog characteristic.
	switch req.topic {
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
		c.deps.Logger.Warn("coordinator.write_unknown_topic", slog.String("topic", req.topic))
		return
	}
	if !entry.Settable {
		c.deps.Logger.Warn("coordinator.write_not_settable", slog.String("topic", req.topic))
		return
	}

	value, ok := c.coerceWriteValue(entry, req.payload)
	if !ok {
		c.deps.Logger.Warn("coordinator.write_bad_value",
			slog.String("topic", req.topic), slog.String("payload", req.payload))
		return
	}

	path := entry.Path
	if strings.Contains(path, "{mode}") {
		c.mu.Lock()
		mode := c.modeCache[req.deviceID+"/"+req.embeddedID]
		c.mu.Unlock()
		if mode == "" {
			c.deps.Logger.Warn("coordinator.write_no_mode", slog.String("topic", req.topic))
			return
		}
		path = strings.ReplaceAll(path, "{mode}", mode)
	}

	if err := c.setCharacteristic(ctx, req.deviceID, req.embeddedID, entry.Match.Characteristic, value, path); err != nil {
		c.deps.Logger.Warn("coordinator.patch_failed",
			slog.String("topic", req.topic), slog.String("err", err.Error()))
		return
	}
	c.deps.Logger.Info("coordinator.patched",
		slog.String("device", req.deviceID), slog.String("characteristic", entry.Match.Characteristic),
		slog.String("value", req.payload))

	// Outdoor-shared settings apply to every indoor unit of the outdoor unit.
	if entry.Scope == "outdoor" && c.deps.Cfg.OutdoorAggregateEnabled() {
		c.fanOutToGroup(ctx, req.deviceID, entry.Match.Characteristic, value, path)
		// Reflect the change on every member immediately (optimistic) and hold it
		// until a Faikin status confirms it, so the sparse/lagging status from the
		// idle indoor units cannot snap the toggle back before the active unit
		// reports the new value.
		if c.localActiveFor(req.deviceID) {
			c.holdOutdoor(req.deviceID, req.topic, req.payload)
			c.publishOptimistic(ctx, req.deviceID, req.topic, req.payload)
		}
	}
	// Mutually-exclusive partners (powerful ⇄ econo) are cleared on enable.
	c.enforceMutualExclusive(ctx, req.deviceID, req.embeddedID, entry.Match.Characteristic, value)
}

// handleHVACModeWrite applies an HA climate hvac-mode command: "off" turns
// the unit off; any other mode turns it on and sets the mapped operationMode.
func (c *Coordinator) handleHVACModeWrite(ctx context.Context, req writeReq) {
	ha := strings.TrimSpace(req.payload)
	patch := func(characteristic string, value any) bool {
		if err := c.setCharacteristic(ctx, req.deviceID, req.embeddedID, characteristic, value, ""); err != nil {
			c.deps.Logger.Warn("coordinator.patch_failed",
				slog.String("topic", req.topic), slog.String("characteristic", characteristic),
				slog.String("err", err.Error()))
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
		c.deps.Logger.Warn("coordinator.write_bad_hvac_mode",
			slog.String("topic", req.topic), slog.String("payload", req.payload))
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

// coerceWriteValue maps an MQTT payload to the cloud value type. Selects
// accept either the raw value or a localized label (mapped back via the
// catalog); numbers parse to float; switches/strings pass through.
func (c *Coordinator) coerceWriteValue(entry *catalog.Entry, payload string) (any, bool) {
	switch entry.Platform {
	case "number":
		f, err := strconv.ParseFloat(strings.TrimSpace(payload), 64)
		if err != nil {
			return nil, false
		}
		return f, true
	case "select":
		if v, ok := entry.CodeForLabel(payload); ok {
			return v, true
		}
		return payload, true
	default:
		return payload, true
	}
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
