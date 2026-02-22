package pebble

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"easy_proxies/internal/store"

	pebblepkg "github.com/cockroachdb/pebble"
)

// Options controls Pebble tuning parameters.
// Keep this minimal at first; add fields as needed.
//
// Directory layout:
// - Dir: main Pebble DB directory (must be persistent)
//
// Note: Pebble uses a directory, unlike SQLite's single file.
type Options struct {
	Dir string
}

// DB implements [`store.Store`](../store.go:1) using Pebble.
//
// This is an initial skeleton to allow the project to compile while migration is in progress.
// Methods will be implemented iteratively.
type DB struct {
	db  *pebblepkg.DB
	dir string
}

var _ store.Store = (*DB)(nil)

// Open opens (or creates) a Pebble DB in the provided directory.
func Open(ctx context.Context, opt Options) (*DB, error) {
	_ = ctx
	if strings.TrimSpace(opt.Dir) == "" {
		return nil, errors.New("pebble dir is empty")
	}
	dir := filepath.Clean(opt.Dir)
	pdb, err := pebblepkg.Open(dir, &pebblepkg.Options{})
	if err != nil {
		return nil, fmt.Errorf("open pebble: %w", err)
	}
	return &DB{db: pdb, dir: dir}, nil
}

func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

// Health methods are implemented in health.go
