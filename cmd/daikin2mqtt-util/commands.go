// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/SukramJ/go-daikin2mqtt/internal/catalog"
	"github.com/SukramJ/go-daikin2mqtt/internal/daikin/model"
)

// newFlags returns a flag set with the shared --config flag registered.
func newFlags(name string) (fs *flag.FlagSet, configPath *string) {
	fs = flag.NewFlagSet(name, flag.ExitOnError)
	configPath = fs.String("config", "", "path to config.yaml (default: search standard locations)")
	return fs, configPath
}

// splitPositionals separates leading positional arguments from trailing
// flags. Go's flag package stops at the first non-flag token, so commands
// with positional arguments must parse flags from the part *after* the
// positionals — otherwise flags placed after them are silently ignored.
func splitPositionals(args []string) (pos, rest []string) {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			return args[:i], args[i:]
		}
	}
	return args, nil
}

// runAuth performs the interactive OAuth2 authorization flow and writes the
// token store.
func runAuth(args []string) error {
	fs, configPath := newFlags("auth")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	ts := tokenSource(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tok, err := authConfig(cfg).Authorize(ctx, nil, cfg.OAuthCallbackBind, func(authURL string) error {
		fmt.Println("Open this URL in your browser to authorize:")
		fmt.Println()
		fmt.Println("  " + authURL)
		fmt.Println()
		openBrowser(authURL)
		return nil
	})
	if err != nil {
		return err
	}
	if err := ts.SetToken(tok); err != nil {
		return err
	}
	fmt.Println("Authorized. Token store written to", cfg.ResolveTokenStorePath(nil))
	return nil
}

// runDevices fetches and prints the gateway devices.
func runDevices(args []string) error {
	fs, configPath := newFlags("devices")
	raw := fs.Bool("raw", false, "print the raw JSON response")
	mock := fs.String("mock", "", "use the ONECTA mock endpoint with this X-Mocking-Example-Id")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	data, err := buildClient(cfg, *mock).GetDevices(ctx)
	if err != nil {
		return err
	}
	if *raw {
		return printIndentedJSON(data)
	}

	devices, err := model.ParseDevices(data)
	if err != nil {
		return err
	}
	fmt.Printf("%d device(s):\n", len(devices))
	for _, d := range devices {
		fmt.Printf("\n• %s  (model: %s)\n", d.ID, d.Model)
		for _, mp := range d.ManagementPoints {
			fmt.Printf("    - %s [%s, %s]  %d characteristic(s)\n",
				mp.EmbeddedID, mp.Type, mp.Category, len(mp.Characteristics))
		}
	}
	return nil
}

// runPoints prints the management points and characteristics of one device.
func runPoints(args []string) error {
	pos, rest := splitPositionals(args)
	fs, configPath := newFlags("points")
	mock := fs.String("mock", "", "use the ONECTA mock endpoint with this X-Mocking-Example-Id")
	_ = fs.Parse(rest)
	if len(pos) < 1 {
		return fmt.Errorf("usage: points <deviceId> [--config ...] [--mock id]")
	}
	deviceID := pos[0]

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	data, err := buildClient(cfg, *mock).GetDevices(ctx)
	if err != nil {
		return err
	}
	devices, err := model.ParseDevices(data)
	if err != nil {
		return err
	}
	for _, d := range devices {
		if d.ID != deviceID {
			continue
		}
		fmt.Printf("Device %s (model: %s)\n", d.ID, d.Model)
		for _, mp := range d.ManagementPoints {
			fmt.Printf("\n  [%s] %s (%s)\n", mp.Type, mp.EmbeddedID, mp.Category)
			for name, c := range mp.Characteristics {
				printCharacteristic(name, c)
			}
		}
		return nil
	}
	return fmt.Errorf("device %q not found", deviceID)
}

// runSet sends a test PATCH to a characteristic.
func runSet(args []string) error {
	pos, rest := splitPositionals(args)
	fs, configPath := newFlags("set")
	path := fs.String("path", "", "nested PATCH path (optional)")
	_ = fs.Parse(rest)
	if len(pos) < 4 {
		return fmt.Errorf("usage: set <deviceId> <embeddedId> <characteristic> <value> [--path p] [--config ...]")
	}
	deviceID, embeddedID, characteristic, rawValue := pos[0], pos[1], pos[2], pos[3]

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := buildClient(cfg, "").Patch(ctx, deviceID, embeddedID, characteristic, coerceValue(rawValue), *path); err != nil {
		return err
	}
	fmt.Println("OK")
	return nil
}

// runRateLimit performs a GET and prints the resulting rate-limit budget.
func runRateLimit(args []string) error {
	fs, configPath := newFlags("ratelimit")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cl := buildClient(cfg, "")
	_, err = cl.GetDevices(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: request error:", err)
	}
	rl := cl.RateLimit()
	fmt.Printf("minute: %d/%d remaining\n", rl.RemainingMinute, rl.LimitMinute)
	fmt.Printf("day:    %d/%d remaining\n", rl.RemainingDay, rl.LimitDay)
	if rl.RetryAfter > 0 {
		fmt.Printf("retry-after: %ds\n", rl.RetryAfter)
	}
	if !rl.ResetAt.IsZero() {
		fmt.Printf("reset-at: %s\n", rl.ResetAt.Format(time.RFC3339))
	}
	return nil
}

// runCatalogCheck reports live characteristics that the catalog does not
// map, so operators can spot coverage gaps.
func runCatalogCheck(args []string) error {
	pos, rest := splitPositionals(args)
	_ = pos
	fs, configPath := newFlags("catalog-check")
	catalogPath := fs.String("catalog", "characteristics.yaml", "path to the characteristics catalog")
	_ = fs.Parse(rest)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	cat, err := catalog.LoadFile(*catalogPath)
	if err != nil {
		return err
	}

	// referenced maps each management-point type to the set of catalog
	// characteristics defined for it.
	referenced := map[string]map[string]bool{}
	entries := cat.Entries()
	for i := range entries {
		e := &entries[i]
		m := referenced[e.Match.ManagementPointType]
		if m == nil {
			m = map[string]bool{}
			referenced[e.Match.ManagementPointType] = m
		}
		m[e.Match.Characteristic] = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data, err := buildClient(cfg, "").GetDevices(ctx)
	if err != nil {
		return err
	}
	devices, err := model.ParseDevices(data)
	if err != nil {
		return err
	}

	// Collect unmapped (mpType, characteristic) pairs.
	seen := map[string]bool{}
	unmapped := map[string][]string{}
	for _, d := range devices {
		for _, mp := range d.ManagementPoints {
			for name := range mp.Characteristics {
				key := mp.Type + "/" + name
				if seen[key] {
					continue
				}
				seen[key] = true
				if referenced[mp.Type][name] {
					continue
				}
				unmapped[mp.Type] = append(unmapped[mp.Type], name)
			}
		}
	}

	if len(unmapped) == 0 {
		fmt.Println("All live characteristics are mapped by the catalog.")
		return nil
	}
	fmt.Println("Unmapped characteristics (no catalog entry):")
	types := make([]string, 0, len(unmapped))
	for t := range unmapped {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		names := unmapped[t]
		sort.Strings(names)
		fmt.Printf("\n  [%s]\n", t)
		for _, n := range names {
			fmt.Printf("    - %s\n", n)
		}
	}
	return nil
}

// coerceValue parses a CLI value as JSON (number/bool/object) and falls
// back to a plain string (so "on" stays "on", "21.5" becomes a number).
func coerceValue(s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return v
	}
	return s
}

func printIndentedJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		// Not valid JSON — print as-is.
		fmt.Println(string(data))
		return nil //nolint:nilerr // best-effort raw print
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

func printCharacteristic(name string, c model.Characteristic) {
	flags := ""
	if c.Settable {
		flags = " (settable)"
	}
	switch {
	case c.IsObject():
		fmt.Printf("    %-28s = <object>%s\n", name, flags)
	default:
		if s, ok := c.String(); ok {
			fmt.Printf("    %-28s = %q%s\n", name, s, flags)
		} else if f, ok := c.Float(); ok {
			fmt.Printf("    %-28s = %g%s\n", name, f, flags)
		} else if b, ok := c.Bool(); ok {
			fmt.Printf("    %-28s = %t%s\n", name, b, flags)
		} else {
			fmt.Printf("    %-28s = %s%s\n", name, string(c.Raw), flags)
		}
	}
	if len(c.Values) > 0 {
		fmt.Printf("    %-28s   values: %v\n", "", c.Values)
	}
}
