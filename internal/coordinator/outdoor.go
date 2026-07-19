// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"sort"

	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
)

// updateOutdoorGroups records each device's outdoor-unit serial so the daemon
// can group the indoor units that share one outdoor unit (a multi-split). The
// serial lives on the device's outdoorUnit management point.
func (c *Coordinator) updateOutdoorGroups(devices []model.Device) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range devices {
		for _, mp := range d.ManagementPoints {
			if mp.Type != "outdoorUnit" {
				continue
			}
			if ch, ok := mp.Characteristics["serialNumber"]; ok {
				if s, ok := ch.String(); ok && s != "" {
					c.outdoorSerial[d.ID] = s
				}
			}
		}
	}
}

// groupMembers returns the device IDs sharing deviceID's outdoor unit (sorted,
// including deviceID itself). A device with no known outdoor serial is its own
// singleton group.
func (c *Coordinator) groupMembers(deviceID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	serial, ok := c.outdoorSerial[deviceID]
	if !ok || serial == "" {
		return []string{deviceID}
	}
	var members []string
	for dev, s := range c.outdoorSerial {
		if s == serial {
			members = append(members, dev)
		}
	}
	sort.Strings(members)
	return members
}

// modeFamily buckets an operationMode by the compressor direction it demands:
// heating vs cooling (dry also runs the compressor in cooling). auto and
// fanOnly have no fixed direction — they neither force a sync nor need to be
// synced away.
func modeFamily(mode string) string {
	switch mode {
	case "heating":
		return "heat"
	case "cooling", "dry":
		return "cool"
	}
	return ""
}

// syncModeToGroup resolves a heat/cool conflict on the other indoor units of
// the same outdoor unit, last-write-wins. A standard multi-split cannot cool
// and heat at once: conflicting units drop to standby, so the daemon switches
// them to the just-commanded mode. Only members known to be RUNNING in the
// opposite compressor direction are touched: a unit that is off (or whose
// state is unknown) is never written to — the local Faikin `mode` command
// force-powers a unit on, so a blind sync would switch on the whole house —
// and a running, non-conflicting unit (same family, auto, fanOnly) keeps its
// mode. No-op when mode sync is disabled or the group is a singleton.
// daikinMode is the ONECTA operationMode code (e.g. "cooling").
func (c *Coordinator) syncModeToGroup(ctx context.Context, originDeviceID, daikinMode string) {
	if !c.deps.Cfg.ModeSyncEnabled() {
		return
	}
	family := modeFamily(daikinMode)
	if family == "" {
		return // auto/fanOnly demand no compressor direction → nothing to resolve
	}
	for _, dev := range c.groupMembers(originDeviceID) {
		if dev == originDeviceID {
			continue
		}
		emb, ok := c.climateEmbeddedID(dev)
		if !ok {
			continue
		}
		if on, known := c.powerState(dev); !known || !on {
			c.deps.Logger.Debug("coordinator.mode_sync_skipped",
				slog.String("device", dev), slog.String("reason", "off_or_unknown"))
			continue
		}
		if cur, ok := c.cachedMode(dev, emb); !ok || modeFamily(cur) == "" || modeFamily(cur) == family {
			c.deps.Logger.Debug("coordinator.mode_sync_skipped",
				slog.String("device", dev), slog.String("reason", "no_conflict"))
			continue
		}
		if err := c.setCharacteristic(ctx, dev, emb, "operationMode", daikinMode, ""); err != nil {
			c.deps.Logger.Warn("coordinator.mode_sync_failed",
				slog.String("device", dev), slog.String("err", err.Error()))
			continue
		}
		c.deps.Logger.Info("coordinator.mode_synced",
			slog.String("device", dev), slog.String("operationMode", daikinMode))
	}
}

// powerState returns the last known onOffMode of a device. known is false
// until one of the three feeders (cloud poll, Faikin state, a successful
// write) has seen the device.
func (c *Coordinator) powerState(deviceID string) (on, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	on, known = c.powerCache[deviceID]
	return on, known
}

// cachedMode returns the last known operationMode of a climate point.
func (c *Coordinator) cachedMode(deviceID, embeddedID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.modeCache[deviceID+"/"+embeddedID]
	return m, ok
}

// mutualExclusive maps a just-enabled setting to the partner it must clear. Only
// the econo→powerful direction is a blind clear: powerful is temporary, so it
// needs no save/restore. The powerful→econo direction is handled by
// reconcileEconoSuspend instead, which remembers econo so it can be restored when
// the boost ends (the hardware does not restore it).
var mutualExclusive = map[string]string{
	"econoMode": "powerfulMode",
}

// enforceMutualExclusive clears the partner of a just-enabled mutually-exclusive
// setting. Today this only fires for econo→powerful: turning econo on clears
// powerful. econo is an outdoor-shared setting, so the clear fans out to the
// whole group (a per-unit boost would otherwise re-suspend econo on the next
// read); it also resets the group's suspend state so the explicit econo wins and
// is not undone by a boost still in progress. No-op when disabled, the
// characteristic has no partner, or value is not an "on".
func (c *Coordinator) enforceMutualExclusive(ctx context.Context, deviceID, embeddedID, characteristic string, value any) {
	if !c.deps.Cfg.MutualExclusiveEnforced() {
		return
	}
	partner, ok := mutualExclusive[characteristic]
	if !ok || !truthy(value) {
		return
	}
	if err := c.setCharacteristic(ctx, deviceID, embeddedID, partner, "off", ""); err != nil {
		c.deps.Logger.Warn("coordinator.exclusive_clear_failed",
			slog.String("characteristic", partner), slog.String("err", err.Error()))
		return
	}
	// econo is group-wide, powerful is per indoor unit: clear the boost on every
	// member and forget any pending restore so econo stays the winner.
	c.fanOutToGroup(ctx, deviceID, partner, "off", "")
	group := c.groupKey(deviceID)
	c.mu.Lock()
	c.econoSuspend[group] = econoSuspendState{boosting: false, pending: false}
	c.mu.Unlock()
	c.deps.Logger.Info("coordinator.exclusive_cleared",
		slog.String("set", characteristic), slog.String("cleared", partner))
}

// reconcileEconoSuspend drives the powerful⇄econo save/restore for one outdoor
// group from an observed (anyPowerful, econoOn) snapshot. econo limits the shared
// outdoor compressor, so a boost on any member suspends it group-wide; when the
// last member leaves powerful the coordinator restores the econo state it saw
// before the boost (the hardware does not). It is edge-driven and idempotent: the
// flag read-modify-write is fully under c.mu (the only mutator, so concurrent
// cloud-poll and Faikin-callback goroutines serialize to exactly one suspend per
// group-on edge and one restore per group-off edge), while the resulting writes
// run outside the lock. Calling it on a non-edge (e.g. the restore's own econo
// write read back later) does nothing, so it never loops.
func (c *Coordinator) reconcileEconoSuspend(ctx context.Context, deviceID string, anyPowerful, econoOn bool) {
	if !c.deps.Cfg.MutualExclusiveEnforced() {
		return
	}
	group := c.groupKey(deviceID) // locks c.mu; must be before the Lock below (mu is not reentrant)

	const (
		actionNone = iota
		actionSuspend
		actionRestore
	)
	c.mu.Lock()
	prev := c.econoSuspend[group]
	next := prev
	next.boosting = anyPowerful
	action := actionNone
	switch {
	case anyPowerful && !prev.boosting: // group boost OFF→ON
		if econoOn {
			next.pending = true
			action = actionSuspend
		} else {
			next.pending = false // nothing to restore later
		}
	case !anyPowerful && prev.boosting: // group boost ON→OFF
		if prev.pending {
			action = actionRestore
		}
		next.pending = false
	}
	c.econoSuspend[group] = next
	c.mu.Unlock()

	switch action {
	case actionSuspend:
		c.deps.Logger.Info("coordinator.econo_suspended", slog.String("group", group))
		c.setGroupEcono(ctx, deviceID, "off")
	case actionRestore:
		c.deps.Logger.Info("coordinator.econo_restored", slog.String("group", group))
		c.setGroupEcono(ctx, deviceID, "on")
	}
}

// reconcileEconoSuspendCloud drives the powerful⇄econo save/restore from a cloud
// poll snapshot. It aggregates powerful/econo per outdoor group (OR across the
// indoor units' management points; only climateControl carries these keys) and
// reconciles each group that is not controlled locally — the Faikin read path
// owns the locally-active groups, whose cloud values lag. In pure-cloud mode it
// drives every group.
func (c *Coordinator) reconcileEconoSuspendCloud(ctx context.Context, devices []model.Device) {
	if !c.deps.Cfg.MutualExclusiveEnforced() {
		return
	}
	type groupAgg struct {
		rep             string
		powerful, econo bool
	}
	groups := map[string]*groupAgg{}
	for _, d := range devices {
		pw, ec := false, false
		for _, mp := range d.ManagementPoints {
			if s, ok := mp.Characteristics["powerfulMode"].String(); ok && s == "on" {
				pw = true
			}
			if s, ok := mp.Characteristics["econoMode"].String(); ok && s == "on" {
				ec = true
			}
		}
		k := c.groupKey(d.ID)
		a := groups[k]
		if a == nil {
			a = &groupAgg{rep: d.ID}
			groups[k] = a
		}
		a.powerful = a.powerful || pw
		a.econo = a.econo || ec
	}
	for _, a := range groups {
		if c.localActiveFor(a.rep) {
			continue // local read path owns this group; cloud values are stale
		}
		c.reconcileEconoSuspend(ctx, a.rep, a.powerful, a.econo)
	}
}

// setGroupEcono writes econo to every indoor unit of the device's outdoor group
// (origin first, then fan-out to the rest) and, in local mode, holds the value
// and reflects it optimistically so the single outdoor-unit econo entity does not
// flicker before the sparse Faikin status confirms it. Mirrors the optimistic
// path of the generic scope:outdoor write in handleWrite.
func (c *Coordinator) setGroupEcono(ctx context.Context, deviceID, value string) {
	if emb, ok := c.climateEmbeddedID(deviceID); ok {
		if err := c.setCharacteristic(ctx, deviceID, emb, "econoMode", value, ""); err != nil {
			c.deps.Logger.Warn("coordinator.econo_set_failed",
				slog.String("device", deviceID), slog.String("err", err.Error()))
		}
	}
	c.fanOutToGroup(ctx, deviceID, "econoMode", value, "")
	if c.localActiveFor(deviceID) {
		c.holdOutdoor(deviceID, "econo_mode", value)
		c.publishOptimistic(ctx, deviceID, "econo_mode", value)
	}
}

// fanOutToGroup applies an outdoor-shared characteristic to every indoor unit of
// the same outdoor unit (the setting is physically one knob on the outdoor
// unit, exposed per indoor unit). Used for scope:outdoor catalog entries.
func (c *Coordinator) fanOutToGroup(ctx context.Context, originDeviceID, characteristic string, value any, path string) {
	for _, dev := range c.groupMembers(originDeviceID) {
		if dev == originDeviceID {
			continue
		}
		emb, ok := c.climateEmbeddedID(dev)
		if !ok {
			continue
		}
		if err := c.setCharacteristic(ctx, dev, emb, characteristic, value, path); err != nil {
			c.deps.Logger.Warn("coordinator.fanout_failed",
				slog.String("device", dev), slog.String("characteristic", characteristic),
				slog.String("err", err.Error()))
		}
	}
}
