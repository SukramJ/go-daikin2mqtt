// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Env abstracts process environment access so tests can inject a
// hermetic env without touching os.Environ.
type Env interface {
	// LookupEnv mirrors os.LookupEnv.
	LookupEnv(key string) (string, bool)
	// Environ returns all KEY=VALUE pairs the daemon should consider when
	// applying DAIKIN_* overrides. Implementations may filter.
	Environ() []string
}

// OSEnv is the real-process implementation of [Env].
type OSEnv struct{}

// LookupEnv implements Env.
func (OSEnv) LookupEnv(key string) (string, bool) { return os.LookupEnv(key) }

// Environ implements Env.
func (OSEnv) Environ() []string { return os.Environ() }

// Load reads a config from r, applies DAIKIN_* overrides from env, fills
// defaults, and validates the result. Returns a fully usable Config or
// the first error encountered (parse errors short-circuit; validation
// errors are aggregated — see [ValidationError]).
func Load(r io.Reader, env Env) (*Config, error) {
	var raw map[string]any
	if err := yaml.NewDecoder(r).Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			raw = map[string]any{} // empty file is allowed; defaults kick in
		} else {
			return nil, fmt.Errorf("config: parse yaml: %w", err)
		}
	}
	if raw == nil {
		raw = map[string]any{}
	}

	if env != nil {
		applyEnvOverrides(raw, env)
	}

	// Round-trip through yaml.v3 so the typed Config sees the merged view
	// (file + env). Marshal cannot fail on a sanitised dict.
	bs, err := yaml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("config: re-marshal merged config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(bs, &cfg); err != nil {
		return nil, fmt.Errorf("config: decode merged config: %w", err)
	}

	applyDefaults(&cfg)
	if err := Validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadFile is a convenience wrapper around [Load] that opens path itself.
func LoadFile(path string, env Env) (*Config, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return Load(f, env)
}

// Locate walks the standard search order (CWD → XDG/APPDATA → ~/.config)
// and returns the first config.yaml that exists. Returns (path, true) on
// hit, ("", false) when no candidate was found.
func Locate(env Env) (string, bool) {
	if env == nil {
		env = OSEnv{}
	}
	for _, p := range append([]string{}, configCandidates(env, ConfigFile)...) {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	return "", false
}

// ResolveTokenStorePath returns the configured TokenStorePath, or the XDG
// default ($XDG_CONFIG_HOME|~/.config/daikin2mqtt/token-store.json) when
// unset.
func (c *Config) ResolveTokenStorePath(env Env) string {
	if c.TokenStorePath != "" {
		return c.TokenStorePath
	}
	if env == nil {
		env = OSEnv{}
	}
	// Reuse the same directory precedence as config discovery, minus the
	// CWD entry (a token store should live in a stable per-user location).
	cands := configCandidates(env, TokenStoreFile)
	if len(cands) > 1 {
		return cands[1] // first XDG/APPDATA or ~/.config candidate
	}
	return cands[len(cands)-1]
}

// configCandidates returns the ordered lookup paths for file name: CWD
// first, then the platform config dir, then ~/.config.
func configCandidates(env Env, name string) []string {
	candidates := []string{}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, name))
	}
	switch runtime.GOOS {
	case "windows":
		if v, ok := env.LookupEnv("APPDATA"); ok && v != "" {
			candidates = append(candidates, filepath.Join(v, AppDirName, name))
		}
	default:
		if v, ok := env.LookupEnv("XDG_CONFIG_HOME"); ok && v != "" {
			candidates = append(candidates, filepath.Join(v, AppDirName, name))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", AppDirName, name))
	}
	return candidates
}

// applyEnvOverrides walks every DAIKIN_<KEY>=value pair in env and sets
// raw[KEY] = coerced(value). The raw map is mutated in place.
//
// Coercion order:
//
//  1. "true"/"false" (case-insensitive) → bool
//  2. parseable as int → int
//  3. parseable as float → float64
//  4. fallback → string
func applyEnvOverrides(raw map[string]any, env Env) {
	for _, kv := range env.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key, val := kv[:eq], kv[eq+1:]
		if !strings.HasPrefix(key, EnvPrefix) {
			continue
		}
		cfgKey := key[len(EnvPrefix):]
		if cfgKey == "" {
			continue
		}
		raw[cfgKey] = coerceEnvValue(val)
	}
}

// coerceEnvValue applies the bool → int → float → string ladder. Exported
// only via applyEnvOverrides; tested via Load.
func coerceEnvValue(s string) any {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true":
		return true
	case "false":
		return false
	}
	// Atoi parses straight into a platform-width int (no int64→int narrowing,
	// which CodeQL flags as go/incorrect-integer-conversion).
	if i, err := strconv.Atoi(s); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}
