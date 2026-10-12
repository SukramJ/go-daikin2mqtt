// SPDX-License-Identifier: MIT
// Copyright (C) 2026 go-daikin2mqtt authors.

package coordinator

import (
	"testing"

	"github.com/SukramJ/go-hamqtt/discovery"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/hass"
	"github.com/SukramJ/go-daikin2mqtt/internal/version"
)

// TestOneBadComponentDoesNotWithholdTheDocument pins the containment gate.
//
// Home Assistant refuses a device document whole only over device/origin, the
// shared availability and a component's platform/unique_id; any other bad
// component costs just that entity. The gate therefore publishes the rest of
// the device and withholds only the offender, instead of refusing the batch.
func TestOneBadComponentDoesNotWithholdTheDocument(t *testing.T) {
	t.Parallel()
	br := newRetainedBroker()
	c := migrationCoordinator(t, br)

	in := buildHamqttInputs(t, surfaceScenarios()[0])
	bundles, err := in.disc.RenderBundles(config.DefaultHASSBaseTopic, version.Version, in.points, in.infos, in.climateInfos)
	if err != nil || len(bundles) == 0 {
		t.Fatalf("render: %v (%d bundles)", err, len(bundles))
	}
	good := bundles[0]
	bad := hass.Bundle{Topic: good.Topic, Bundle: cloneBundle(good.Bundle)}
	bad.Bundle.Components = map[string]discovery.Component{}
	var victim string
	for k, comp := range good.Bundle.Components {
		if victim == "" && comp.Platform == "sensor" {
			victim = k
			comp.DeviceClass = "garage" // a cover class, not a sensor one
		}
		bad.Bundle.Components[k] = comp
	}
	if victim == "" {
		t.Fatal("scenario has no sensor component to corrupt")
	}
	ct := discovery.Contain(bad.Bundle, discovery.ContainOptions{})
	if _, ok := ct.Withheld[victim]; !ok || !ct.Publishable() {
		t.Fatalf("setup: expected %q withheld and the rest publishable (withheld=%v)", victim, ct.Withheld)
	}
	if !c.bundleValidates(bad) {
		t.Error("one bad component withheld the whole device document")
	}
}
