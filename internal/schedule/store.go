// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Store persists a [Document] as JSON at a fixed path. The file is written
// atomically (temp file + rename) with 0600 permissions, mirroring
// auth.Store — a schedule file is not secret, but it is operator
// configuration that a half-written file would corrupt.
type Store struct {
	path string
}

// NewStore returns a Store backed by path.
func NewStore(path string) *Store { return &Store{path: path} }

// Path returns the backing file path.
func (s *Store) Path() string { return s.path }

// Load reads and validates the persisted document. A missing file is not an
// error: it yields an empty, valid document so a fresh install starts with no
// schedules rather than failing to boot.
func (s *Store) Load() (*Document, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NewDocument(), nil
		}
		return nil, fmt.Errorf("schedule: read store: %w", err)
	}
	var doc Document
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("schedule: decode store %s: %w", s.path, err)
	}
	if doc.Version == 0 {
		doc.Version = SchemaVersion
	}
	if doc.Schedules == nil {
		doc.Schedules = []Schedule{}
	}
	if err := doc.Validate(); err != nil {
		return nil, fmt.Errorf("schedule: invalid store %s: %w", s.path, err)
	}
	return &doc, nil
}

// Save validates and writes the document atomically, creating the parent
// directory when needed. It does not touch Revision — the caller owns that,
// so a save can be rejected on a stale revision before it reaches the disk.
func (s *Store) Save(doc *Document) error {
	if doc == nil {
		return errors.New("schedule: refusing to save nil document")
	}
	if doc.Version == 0 {
		doc.Version = SchemaVersion
	}
	if err := doc.Validate(); err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("schedule: create store dir: %w", err)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("schedule: encode document: %w", err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, ".schedules-*.tmp")
	if err != nil {
		return fmt.Errorf("schedule: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("schedule: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("schedule: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("schedule: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("schedule: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("schedule: replace store: %w", err)
	}
	return nil
}
