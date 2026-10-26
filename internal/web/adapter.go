// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SukramJ/go-bridgeui"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/coordinator"
	"github.com/SukramJ/go-daikin2mqtt/internal/state"
	"github.com/SukramJ/go-daikin2mqtt/internal/version"
)

// The dashboard's entry groups.
const (
	groupClimate    = "climate"
	groupHotWater   = "hotwater"
	groupEnergy     = "energy"
	groupSchedule   = "schedule"
	groupLocal      = "local"
	groupDiagnostic = "diagnostic"
)

// dashboardLayout is the Daikin page: a unit's climate and hot water, its
// energy, its schedule; the diagnostics under System; the cloud tools as
// pages of their own.
func dashboardLayout(cfg *config.Config) bridgeui.Layout {
	l := bridgeui.Layout{
		Brand:   "daikin2mqtt",
		Product: "Daikin ONECTA",
		Icon:    "❄",
		Sections: []bridgeui.Section{
			{ID: "climate", Title: "section.climate", Nav: "nav.climate", Groups: []string{groupClimate, groupHotWater}},
			{ID: "energy", Title: "section.energy", Nav: "nav.energy", Groups: []string{groupEnergy}, Optional: true},
			{ID: "schedule", Title: "section.schedule", Nav: "nav.schedule", Groups: []string{groupSchedule}, Optional: true},
		},
		SystemGroups: []string{groupLocal, groupDiagnostic},
		Links: []bridgeui.Link{
			{Href: "tools.html#sec-auth", Title: "link.auth"},
			{Href: "tools.html#sec-devices", Title: "link.devices"},
		},
	}
	if cfg.ScheduleEnable {
		l.Links = append(l.Links, bridgeui.Link{Href: "tools.html#sec-schedules", Title: "link.schedules"})
	}
	l.Links = append(l.Links, bridgeui.Link{Href: "tools.html#sec-patch", Title: "link.patch"})
	return l
}

// adapter presents the state store and the coordinator's write queue as
// go-bridgeui's contract: every ONECTA device is a device, every published
// point an entry, every settable point an item.
type adapter struct {
	store   *state.Store
	bridge  Bridge
	client  cloudClient
	tokens  tokenSource
	mqttUp  func() bool
	cfg     *config.Config
	de      bool
	version string

	notifier bridgeui.Notifier
	problems bridgeui.ProblemLog
}

func newAdapter(d Deps, cfg *config.Config) *adapter {
	a := &adapter{
		store: d.State, bridge: d.Bridge, client: d.Client, tokens: d.Tokens, mqttUp: d.MQTTUp,
		cfg: cfg, de: strings.EqualFold(cfg.Language, "de"), version: version.Version,
	}
	if a.store == nil {
		a.store = state.New(time.Now())
	}
	if a.mqttUp == nil {
		a.mqttUp = func() bool { return true }
	}
	a.problems.OnAdd = a.notifier.Notify
	a.store.OnChange(a.notifier.Notify)
	return a
}

// controls are the platforms the dashboard writes, as Home Assistant does.
var controls = map[string]bool{"number": true, "select": true, "switch": true, "button": true}

// buttonPress is what a button writes: any non-empty payload fires it.
const buttonPress = "PRESS"

func itemKey(p *state.Point) string { return p.EmbeddedID + "/" + p.Topic }

// group places a point on the page.
func group(p *state.Point) string {
	switch {
	case p.Characteristic == "consumptionData":
		return groupEnergy
	case strings.HasPrefix(p.Characteristic, "daemon") && p.Characteristic != "daemonRefresh":
		return groupSchedule
	case p.Characteristic == "faikinLocal":
		return groupLocal
	case p.Category == "diagnostic", p.MPType == "gateway", p.MPType == "outdoorUnit":
		return groupDiagnostic
	case strings.HasPrefix(p.MPType, "domesticHotWater"):
		return groupHotWater
	}
	return groupClimate
}

func (a *adapter) name(p *state.Point) string {
	if a.de && p.NameDE != "" {
		return p.NameDE
	}
	if p.Name != "" {
		return p.Name
	}
	return p.Topic
}

func (a *adapter) Snapshot() bridgeui.Snapshot {
	devices := a.store.Snapshot()
	out := bridgeui.Snapshot{Devices: make([]bridgeui.Device, 0, len(devices))}
	for i := range devices {
		d := &devices[i]
		dev := bridgeui.Device{
			ID:        d.ID,
			Title:     firstNonEmpty(d.Model, d.ID),
			Subtitle:  d.ID,
			Online:    d.Online,
			UpdatedAt: d.UpdatedAt,
			Entries:   make([]bridgeui.Entry, 0, len(d.Points)),
		}
		for j := range d.Points {
			p := &d.Points[j]
			if p.Platform == "button" {
				continue // an action has no state to show
			}
			dev.Entries = append(dev.Entries, bridgeui.Entry{
				Group: group(&p.Point), Key: itemKey(&p.Point), Name: a.name(&p.Point),
				Unit: p.Unit, Value: p.Value, Tone: tone(&p.Point, p.Value),
			})
		}
		out.Devices = append(out.Devices, dev)
	}
	return out
}

// tone flags a raised error, warning or caution state.
func tone(p *state.Point, v any) string {
	on, ok := v.(bool)
	if !ok || p.Platform != "binary_sensor" || !on {
		return ""
	}
	switch p.Characteristic {
	case "isInErrorState":
		return "err"
	case "isInWarningState", "isInCautionState", "isInModeConflict":
		return "warn"
	}
	return ""
}

func (a *adapter) Items() []bridgeui.Item {
	var out []bridgeui.Item
	for _, d := range a.store.Snapshot() {
		for j := range d.Points {
			p := &d.Points[j].Point
			if !p.Settable || !controls[p.Platform] {
				continue
			}
			it := bridgeui.Item{
				Key: itemKey(p), Device: d.ID, Name: a.name(p), Unit: p.Unit,
				Component: p.Platform, Min: p.Min, Max: p.Max, Step: p.Step,
			}
			if p.Platform == "button" {
				it.Payload = buttonPress
			}
			for _, o := range p.Options {
				label := o.Label
				if a.de && o.LabelDE != "" {
					label = o.LabelDE
				}
				it.Options = append(it.Options, bridgeui.Option{Value: o.Value, Label: firstNonEmpty(label, o.Value)})
			}
			out = append(out, it)
		}
	}
	return out
}

// Write queues the write exactly as an MQTT set on the point's item would
// be queued. It returns once queued: like a set, the outcome shows in the
// published state.
func (a *adapter) Write(_ context.Context, device, key, value string) error {
	emb, topic, ok := strings.Cut(key, "/")
	if !ok {
		return fmt.Errorf("%w: %s", bridgeui.ErrNotWritable, key)
	}
	if !a.store.HasDevice(device) {
		return fmt.Errorf("%w: %s", bridgeui.ErrUnknownDevice, device)
	}
	p, ok := a.store.Point(device, emb, topic)
	if !ok || !p.Settable || !controls[p.Platform] {
		return fmt.Errorf("%w: %s", bridgeui.ErrNotWritable, key)
	}
	if a.bridge == nil {
		return fmt.Errorf("%w: no write path", bridgeui.ErrNotReady)
	}
	if err := a.bridge.Submit(device, emb, topic, value); err != nil {
		a.problems.Add(bridgeui.Problem{Device: device, Event: "web.write_failed", Detail: key + " = " + value + ": " + err.Error()})
		if errors.Is(err, coordinator.ErrWriteQueueFull) {
			return fmt.Errorf("%w: %w", bridgeui.ErrNotReady, err)
		}
		return err
	}
	return nil
}

func (a *adapter) Health() bridgeui.Health {
	var cloudOK, known bool
	var lastPoll time.Time
	if a.bridge != nil {
		cloudOK, known, lastPoll = a.bridge.Upstream()
	}
	mqttUp := a.mqttUp()

	status := "ok"
	switch {
	case !known && lastPoll.IsZero():
		status = "starting"
	case !cloudOK || !mqttUp:
		status = "degraded"
	}

	mqtt := bridgeui.Tile{Label: "health.mqtt", Text: "val.disconnected", Tone: "err", Sub: a.cfg.MQTTServer}
	if mqttUp {
		mqtt.Text, mqtt.Tone = "val.connected", "ok"
	}
	cloud := bridgeui.Tile{Label: "health.cloud", Text: "val.disconnected", Tone: "err"}
	switch {
	case cloudOK:
		cloud.Text, cloud.Tone = "val.connected", "ok"
	case lastPoll.IsZero():
		cloud.Text, cloud.Tone = "val.unknown", ""
	}
	if !lastPoll.IsZero() {
		cloud.Sub = lastPoll.Local().Format("15:04:05")
	}
	tiles := []bridgeui.Tile{mqtt, cloud, a.authTile()}
	if a.cfg.LocalEnabled() {
		tiles = append(tiles, bridgeui.Tile{Label: "health.mode", Text: "val.local"})
	}
	tiles = append(tiles, a.rateLimitTiles()...)
	return bridgeui.Health{
		Status:    status,
		Version:   a.version,
		StartedAt: a.store.StartedAt(),
		Tiles:     tiles,
		Problems:  a.problems.List(),
	}
}

// authTile says whether the token source holds a usable token. The token
// source answers from its cache; only an expired token costs a refresh.
func (a *adapter) authTile() bridgeui.Tile {
	t := bridgeui.Tile{Label: "health.auth", Text: "val.reauth", Tone: "err"}
	if a.tokens == nil {
		t.Text, t.Tone = "val.unknown", ""
		return t
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := a.tokens.Token(ctx); err == nil {
		t.Text, t.Tone = "val.ok", "ok"
	}
	return t
}

// rateLimitTiles show the cloud's daily and per-minute quota as of the last
// response; none before the first.
func (a *adapter) rateLimitTiles() []bridgeui.Tile {
	if a.client == nil {
		return nil
	}
	rl := a.client.RateLimit()
	if rl.Updated.IsZero() {
		return nil
	}
	quota := func(label string, rem, limit int) bridgeui.Tile {
		t := bridgeui.Tile{Label: label, Value: strconv.Itoa(rem) + " / " + strconv.Itoa(limit)}
		switch {
		case limit > 0 && rem == 0:
			t.Tone = "err"
		case limit > 0 && rem*10 < limit:
			t.Tone = "warn"
		}
		return t
	}
	return []bridgeui.Tile{
		quota("health.rateDay", rl.RemainingDay, rl.LimitDay),
		quota("health.rateMinute", rl.RemainingMinute, rl.LimitMinute),
	}
}

func (a *adapter) Config() []bridgeui.Tile {
	c := a.cfg
	onOff := func(label string, on bool) bridgeui.Tile {
		if on {
			return bridgeui.Tile{Label: label, Text: "val.on"}
		}
		return bridgeui.Tile{Label: label, Text: "val.off"}
	}
	hass := onOff("cfg.hass", c.HASSEnable)
	if c.HASSEnable {
		hass.Sub = c.HASSBaseTopic
	}
	auth := bridgeui.Tile{Label: "cfg.webAuth", Text: "val.on"}
	if c.WebUser == "" && c.WebPassword == "" {
		auth = bridgeui.Tile{Label: "cfg.webAuth", Text: "val.off", Tone: "warn"}
	}
	mode := bridgeui.Tile{Label: "cfg.mode", Text: "val.cloud"}
	if c.LocalEnabled() {
		mode = bridgeui.Tile{Label: "cfg.mode", Text: "val.local", Sub: strconv.Itoa(len(c.LocalDeviceMap)) + " Faikin"}
	}
	return []bridgeui.Tile{
		{Label: "cfg.mqtt", Value: firstNonEmpty(c.MQTTServer, "—"), Sub: c.MQTTTopic},
		hass,
		mode,
		{Label: "cfg.poll", Value: c.RefreshDayIntervalDuration().String() + " / " + c.RefreshNightIntervalDuration().String()},
		onOff("cfg.schedule", c.ScheduleEnable),
		auth,
		{Label: "cfg.language", Value: firstNonEmpty(c.Language, "en")},
		onOff("cfg.debug", c.Debug),
	}
}

func (a *adapter) Changes() (events <-chan struct{}, cancel func()) {
	return a.notifier.Subscribe()
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
