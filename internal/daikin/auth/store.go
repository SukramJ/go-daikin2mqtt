// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNoToken is returned by [Store.Load] when no token store exists yet.
var ErrNoToken = errors.New("auth: no token store")

// Store persists a single [Token] as JSON at a fixed path. The file is
// written atomically with 0600 permissions so the refresh token is never
// world-readable.
type Store struct {
	path string
}

// NewStore returns a Store backed by path.
func NewStore(path string) *Store { return &Store{path: path} }

// Path returns the backing file path.
func (s *Store) Path() string { return s.path }

// Load reads the persisted token. Returns [ErrNoToken] (wrapped) when the
// file does not exist.
func (s *Store) Load() (*Token, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w at %s", ErrNoToken, s.path)
		}
		return nil, fmt.Errorf("auth: read token store: %w", err)
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("auth: decode token store %s: %w", s.path, err)
	}
	return &t, nil
}

// Save writes the token atomically (temp file + rename) with 0600 perms,
// creating the parent directory if needed.
func (s *Store) Save(t *Token) error {
	if t == nil {
		return errors.New("auth: refusing to save nil token")
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("auth: create token store dir: %w", err)
	}
	b, err := json.MarshalIndent(t, "", "  ") //nolint:gosec // token store is intentionally persisted (0600)
	if err != nil {
		return fmt.Errorf("auth: encode token: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".token-*.tmp")
	if err != nil {
		return fmt.Errorf("auth: create temp token file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("auth: chmod temp token file: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("auth: write temp token file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("auth: sync temp token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("auth: close temp token file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("auth: replace token store: %w", err)
	}
	return nil
}
