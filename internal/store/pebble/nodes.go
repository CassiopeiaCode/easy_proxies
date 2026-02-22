package pebble

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"easy_proxies/internal/store"

	pebblepkg "github.com/cockroachdb/pebble"
)

// UpsertNodeByHostPort upserts node metadata by dedup key (host,port) extracted from URI.
func (d *DB) UpsertNodeByHostPort(ctx context.Context, in store.UpsertNodeInput) (store.Node, error) {
	if d == nil || d.db == nil {
		return store.Node{}, errors.New("pebble: db not initialized")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return store.Node{}, err
		}
	}

	in.URI = strings.TrimSpace(in.URI)
	in.Name = strings.TrimSpace(in.Name)
	if in.URI == "" {
		return store.Node{}, errors.New("uri is empty")
	}

	host, port, protocol, err := store.HostPortFromURI(in.URI)
	if err != nil {
		return store.Node{}, err
	}
	if in.Name == "" {
		in.Name = store.NameFromURI(in.URI)
	}

	k := keyNode(host, port)

	// Read existing record (if any)
	var existing *store.Node
	if val, closer, gerr := d.db.Get(k); gerr == nil {
		n, uerr := unmarshalNode(val)
		_ = closer.Close()
		if uerr == nil {
			existing = &n
		}
	}

	now := time.Now().UTC()
	var out store.Node
	if existing != nil {
		out = *existing
		out.URI = in.URI
		if in.Name != "" {
			out.Name = in.Name
		}
		out.Protocol = protocol
		out.UpdatedAt = now
	} else {
		out = store.Node{
			ID:        0,
			Host:      host,
			Port:      port,
			URI:       in.URI,
			Name:      in.Name,
			Protocol:  protocol,
			CreatedAt: now,
			UpdatedAt: now,
		}
	}

	b, merr := marshalNode(out)
	if merr != nil {
		return store.Node{}, merr
	}
	if err := d.db.Set(k, b, pebblepkg.Sync); err != nil {
		return store.Node{}, fmt.Errorf("pebble set node: %w", err)
	}
	return out, nil
}

func (d *DB) DeleteNodeByURI(ctx context.Context, uri string) error {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return errors.New("uri is empty")
	}
	host, port, _, err := store.HostPortFromURI(uri)
	if err != nil {
		return err
	}
	return d.DeleteNodeByHostPort(ctx, host, port)
}

func (d *DB) DeleteNodeByHostPort(ctx context.Context, host string, port int) error {
	if d == nil || d.db == nil {
		return errors.New("pebble: db not initialized")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	host = strings.TrimSpace(host)
	if host == "" || port <= 0 {
		return errors.New("host/port invalid")
	}
	k := keyNode(host, port)
	if err := d.db.Delete(k, pebblepkg.Sync); err != nil {
		return fmt.Errorf("pebble delete node: %w", err)
	}
	return nil
}

func (d *DB) ListActiveNodes(ctx context.Context) ([]store.Node, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("pebble: db not initialized")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	iter, err := d.db.NewIter(&pebblepkg.IterOptions{
		LowerBound: keyNodePrefixBytes(),
		UpperBound: append([]byte(keyNodePrefix), 0xFF),
	})
	if err != nil {
		return nil, fmt.Errorf("pebble iter: %w", err)
	}
	defer iter.Close()

	out := make([]store.Node, 0)
	for iter.First(); iter.Valid(); iter.Next() {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		n, uerr := unmarshalNode(iter.Value())
		if uerr != nil {
			continue
		}
		if !n.IsDamaged {
			out = append(out, n)
		}
	}
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("pebble iter nodes: %w", err)
	}

	// Stable output: most recently updated first (similar to SQLite ORDER BY updated_at DESC)
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

func (d *DB) IsNodeDamaged(ctx context.Context, host string, port int) (bool, error) {
	if d == nil || d.db == nil {
		return false, errors.New("pebble: db not initialized")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
	host = strings.TrimSpace(host)
	if host == "" || port <= 0 {
		return false, errors.New("host/port invalid")
	}

	k := keyNode(host, port)
	val, closer, err := d.db.Get(k)
	if err != nil {
		if errors.Is(err, pebblepkg.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("pebble get node: %w", err)
	}
	defer closer.Close()

	n, uerr := unmarshalNode(val)
	if uerr != nil {
		return false, uerr
	}
	return n.IsDamaged, nil
}

func (d *DB) MarkNodeDamaged(ctx context.Context, host string, port int, reason string) error {
	if d == nil || d.db == nil {
		return errors.New("pebble: db not initialized")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	host = strings.TrimSpace(host)
	reason = strings.TrimSpace(reason)
	if host == "" || port <= 0 {
		return errors.New("host/port invalid")
	}

	k := keyNode(host, port)
	val, closer, err := d.db.Get(k)
	if err != nil {
		if errors.Is(err, pebblepkg.ErrNotFound) {
			// If the node wasn't inserted yet, create a minimal record.
			now := time.Now().UTC()
			n := store.Node{
				Host:        host,
				Port:        port,
				IsDamaged:   true,
				DamageReason: reason,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			b, merr := marshalNode(n)
			if merr != nil {
				return merr
			}
			return d.db.Set(k, b, pebblepkg.Sync)
		}
		return fmt.Errorf("pebble get node: %w", err)
	}
	n, uerr := unmarshalNode(val)
	_ = closer.Close()
	if uerr != nil {
		return uerr
	}

	n.IsDamaged = true
	n.DamageReason = reason
	n.UpdatedAt = time.Now().UTC()
	b, merr := marshalNode(n)
	if merr != nil {
		return merr
	}
	if err := d.db.Set(k, b, pebblepkg.Sync); err != nil {
		return fmt.Errorf("pebble set node: %w", err)
	}
	return nil
}
