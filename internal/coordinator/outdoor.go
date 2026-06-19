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

// syncModeToGroup propagates a heat/cool operationMode to the other indoor
// units of the same outdoor unit. A standard multi-split cannot cool and heat
// at once: conflicting units drop to standby, so the daemon keeps the whole
// group on one mode. No-op when mode sync is disabled or the group is a
// singleton. daikinMode is the ONECTA operationMode code (e.g. "cooling").
func (c *Coordinator) syncModeToGroup(ctx context.Context, originDeviceID, daikinMode string) {
	if !c.deps.Cfg.ModeSyncEnabled() {
		return
	}
	for _, dev := range c.groupMembers(originDeviceID) {
		if dev == originDeviceID {
			continue
		}
		emb, ok := c.climateEmbeddedID(dev)
		if !ok {
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

// mutualExclusive pairs settings that the hardware cannot run together; turning
// one on must turn the other off.
var mutualExclusive = map[string]string{
	"powerfulMode": "econoMode",
	"econoMode":    "powerfulMode",
}

// enforceMutualExclusive clears the partner of a just-enabled mutually-exclusive
// setting (powerful ⇄ econo). No-op when disabled, the characteristic has no
// partner, or value is not an "on".
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
	c.deps.Logger.Info("coordinator.exclusive_cleared",
		slog.String("set", characteristic), slog.String("cleared", partner))
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
