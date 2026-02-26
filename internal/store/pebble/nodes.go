package pebble

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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

// UpsertNodesByHostPortBatch upserts node metadata by dedup key (host,port) for a batch.
// It commits once with pebble.Sync to reduce fsync overhead for large refreshes.
func (d *DB) UpsertNodesByHostPortBatch(ctx context.Context, in []store.UpsertNodeInput) error {
	if d == nil || d.db == nil {
		return errors.New("pebble: db not initialized")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if len(in) == 0 {
		return nil
	}

	updates := make(map[string]store.Node, len(in))
	now := time.Now().UTC()

	for _, item := range in {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		item.URI = strings.TrimSpace(item.URI)
		item.Name = strings.TrimSpace(item.Name)
		if item.URI == "" {
			return errors.New("uri is empty")
		}

		host, port, protocol, err := store.HostPortFromURI(item.URI)
		if err != nil {
			return err
		}
		if item.Name == "" {
			item.Name = store.NameFromURI(item.URI)
		}
		keyStr := string(keyNode(host, port))
		if existing, ok := updates[keyStr]; ok {
			existing.URI = item.URI
			if item.Name != "" {
				existing.Name = item.Name
			}
			existing.Protocol = protocol
			existing.UpdatedAt = now
			updates[keyStr] = existing
			continue
		}

		var out store.Node
		k := []byte(keyStr)
		if val, closer, gerr := d.db.Get(k); gerr == nil {
			n, uerr := unmarshalNode(val)
			_ = closer.Close()
			if uerr == nil {
				out = n
				out.URI = item.URI
				if item.Name != "" {
					out.Name = item.Name
				}
				out.Protocol = protocol
				out.UpdatedAt = now
				updates[keyStr] = out
				continue
			}
		}

		out = store.Node{
			ID:        0,
			Host:      host,
			Port:      port,
			URI:       item.URI,
			Name:      item.Name,
			Protocol:  protocol,
			CreatedAt: now,
			UpdatedAt: now,
		}
		updates[keyStr] = out
	}

	batch := d.db.NewBatch()
	defer batch.Close()
	for k, node := range updates {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		b, err := marshalNode(node)
		if err != nil {
			return err
		}
		if err := batch.Set([]byte(k), b, nil); err != nil {
			return fmt.Errorf("pebble batch set node: %w", err)
		}
	}
	if err := batch.Commit(pebblepkg.Sync); err != nil {
		return fmt.Errorf("pebble batch commit nodes: %w", err)
	}
	return nil
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

func (d *DB) ListNodes(ctx context.Context) ([]store.Node, error) {
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
		out = append(out, n)
	}
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("pebble iter nodes: %w", err)
	}

	// Stable output: most recently updated first.
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
				Host:         host,
				Port:         port,
				IsDamaged:    true,
				DamageReason: reason,
				CreatedAt:    now,
				UpdatedAt:    now,
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

	// Heuristic damage propagation:
	// if a node is marked damaged, also mark nodes whose URI signatures are identical
	// after ignoring name/host/ip related fields.
	_ = d.markDamagedByHeuristic(ctx, n.URI, reason, host, port)
	return nil
}

func (d *DB) markDamagedByHeuristic(ctx context.Context, rawURI, reason, sourceHost string, sourcePort int) error {
	if d == nil || d.db == nil {
		return errors.New("pebble: db not initialized")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	targetSig, ok := damagedSignature(rawURI)
	if !ok {
		return nil
	}

	iter, err := d.db.NewIter(&pebblepkg.IterOptions{
		LowerBound: keyNodePrefixBytes(),
		UpperBound: append([]byte(keyNodePrefix), 0xFF),
	})
	if err != nil {
		return fmt.Errorf("pebble iter: %w", err)
	}
	defer iter.Close()

	now := time.Now().UTC()
	batch := d.db.NewBatch()
	defer batch.Close()
	updated := 0

	for iter.First(); iter.Valid(); iter.Next() {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		n, uerr := unmarshalNode(iter.Value())
		if uerr != nil {
			continue
		}

		// Skip the originally marked node; it is already persisted.
		if strings.EqualFold(strings.TrimSpace(n.Host), strings.TrimSpace(sourceHost)) && n.Port == sourcePort {
			continue
		}

		sig, ok := damagedSignature(n.URI)
		if !ok || sig != targetSig {
			continue
		}
		if n.IsDamaged {
			continue
		}

		n.IsDamaged = true
		n.DamageReason = reason
		n.UpdatedAt = now
		b, merr := marshalNode(n)
		if merr != nil {
			return merr
		}
		if err := batch.Set(keyNode(n.Host, n.Port), b, nil); err != nil {
			return fmt.Errorf("pebble batch set node: %w", err)
		}
		updated++
	}
	if err := iter.Error(); err != nil {
		return fmt.Errorf("pebble iter nodes: %w", err)
	}
	if updated == 0 {
		return nil
	}
	if err := batch.Commit(pebblepkg.Sync); err != nil {
		return fmt.Errorf("pebble batch commit nodes: %w", err)
	}
	return nil
}

func damagedSignature(rawURI string) (string, bool) {
	rawURI = strings.TrimSpace(rawURI)
	if rawURI == "" {
		return "", false
	}

	u, err := url.Parse(rawURI)
	if err != nil || strings.TrimSpace(u.Scheme) == "" {
		return "", false
	}

	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	user := ""
	if u.User != nil {
		user = u.User.String()
	}
	port := u.Port()
	path := u.EscapedPath()

	// Ignore query keys that commonly encode server host/name identity.
	drop := map[string]struct{}{
		"host":       {},
		"sni":        {},
		"servername": {},
		"peer":       {},
		"add":        {},
		"ps":         {}, // name/remark
	}
	q := u.Query()
	parts := make([]string, 0, len(q))
	for k, vals := range q {
		if _, skip := drop[strings.ToLower(strings.TrimSpace(k))]; skip {
			continue
		}
		sort.Strings(vals)
		parts = append(parts, k+"="+strings.Join(vals, ","))
	}
	sort.Strings(parts)
	querySig := strings.Join(parts, "&")

	// Exclude hostname/IP and fragment(name) on purpose.
	// Keep protocol, auth/userinfo, port, path and filtered query as heuristic identity.
	return scheme + "|" + user + "|" + port + "|" + path + "|" + querySig, true
}
