package pebble

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"easy_proxies/internal/store"

	pebblepkg "github.com/cockroachdb/pebble"
)

func parseHealthHourKey(key []byte) (hour time.Time, host string, port int, ok bool) {
	s := string(key)
	if !strings.HasPrefix(s, keyHealthHourPref) {
		return time.Time{}, "", 0, false
	}
	rest := strings.TrimPrefix(s, keyHealthHourPref)
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return time.Time{}, "", 0, false
	}
	hourStr := parts[0]
	host = parts[1]
	p, err := strconv.Atoi(parts[2])
	if err != nil || p <= 0 || p > 65535 {
		return time.Time{}, "", 0, false
	}

	// hourStr: YYYYMMDDHH in UTC
	t, err := time.ParseInLocation("2006010215", hourStr, time.UTC)
	if err != nil {
		return time.Time{}, "", 0, false
	}
	return t, host, p, true
}

func (d *DB) RecordHealthCheck(ctx context.Context, u store.HealthCheckUpdate) error {
	if d == nil || d.db == nil {
		return errors.New("pebble: db not initialized")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	u.Host = strings.TrimSpace(u.Host)
	u.ErrText = strings.TrimSpace(u.ErrText)
	if u.Host == "" || u.Port <= 0 {
		return errors.New("host/port invalid")
	}

	now := time.Now().UTC()
	hour := now.Truncate(time.Hour)

	// Load node record (optional; if missing, we create a minimal record)
	kNode := keyNode(u.Host, u.Port)
	var node store.Node
	haveNode := false
	if val, closer, err := d.db.Get(kNode); err == nil {
		n, uerr := unmarshalNode(val)
		_ = closer.Close()
		if uerr == nil {
			node = n
			haveNode = true
		}
	}
	if !haveNode {
		node = store.Node{
			Host:      u.Host,
			Port:      u.Port,
			CreatedAt: now,
			UpdatedAt: now,
		}
	}

	// Update node counters
	node.HealthCheckCount++
	tCheck := now
	node.LastCheckAt = &tCheck
	if u.Success {
		node.HealthCheckSuccessCount++
		tOK := now
		node.LastSuccessAt = &tOK
		if u.LatencyMs != nil {
			v := *u.LatencyMs
			if v < 0 {
				v = 0
			}
			node.LastLatencyMs = &v
		}
	}
	node.UpdatedAt = now

	// Load hourly aggregate
	kHour := keyHealthHour(hour, u.Host, u.Port)
	agg := healthHourRecord{}
	if val, closer, err := d.db.Get(kHour); err == nil {
		r, uerr := unmarshalHealthHour(val)
		_ = closer.Close()
		if uerr == nil {
			agg = r
		}
	}
	if u.Success {
		agg.SuccessCount++
		if u.LatencyMs != nil && *u.LatencyMs > 0 {
			agg.LatencySumMs += *u.LatencyMs
			agg.LatencyCount++
		}
	} else {
		agg.FailCount++
	}
	agg.UpdatedAtUnix = now.Unix()

	bNode, err := marshalNode(node)
	if err != nil {
		return err
	}
	bAgg, err := marshalHealthHour(agg)
	if err != nil {
		return err
	}

	batch := d.db.NewBatch()
	defer batch.Close()
	if err := batch.Set(kNode, bNode, nil); err != nil {
		return fmt.Errorf("pebble batch set node: %w", err)
	}
	if err := batch.Set(kHour, bAgg, nil); err != nil {
		return fmt.Errorf("pebble batch set health hour: %w", err)
	}
	if err := batch.Commit(pebblepkg.Sync); err != nil {
		return fmt.Errorf("pebble batch commit: %w", err)
	}
	return nil
}

func (d *DB) QueryNodeRatesSince(ctx context.Context, since time.Time) ([]store.NodeRate, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("pebble: db not initialized")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	since = since.UTC().Truncate(time.Hour)
	lower := []byte(fmt.Sprintf("%s%s/", keyHealthHourPref, hourKey(since)))
	upper := append([]byte(keyHealthHourPref), 0xFF)

	iter, err := d.db.NewIter(&pebblepkg.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, fmt.Errorf("pebble iter health: %w", err)
	}
	defer iter.Close()

	type acc struct{ s, f int64 }
	m := make(map[string]acc)

	for iter.First(); iter.Valid(); iter.Next() {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}

		_, host, port, ok := parseHealthHourKey(iter.Key())
		if !ok {
			continue
		}
		agg, uerr := unmarshalHealthHour(iter.Value())
		if uerr != nil {
			continue
		}
		k := fmt.Sprintf("%s:%d", host, port)
		prev := m[k]
		prev.s += agg.SuccessCount
		prev.f += agg.FailCount
		m[k] = prev
	}
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("pebble iter health: %w", err)
	}

	out := make([]store.NodeRate, 0, len(m))
	for k, v := range m {
		parts := strings.Split(k, ":")
		if len(parts) != 2 {
			continue
		}
		p, _ := strconv.Atoi(parts[1])
		r := store.NodeRate{Host: parts[0], Port: p, SuccessCount: v.s, FailCount: v.f}
		total := r.SuccessCount + r.FailCount
		if total > 0 {
			r.Rate = float64(r.SuccessCount) / float64(total)
		}
		out = append(out, r)
	}

	// Deterministic order
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host == out[j].Host {
			return out[i].Port < out[j].Port
		}
		return out[i].Host < out[j].Host
	})
	return out, nil
}

func (d *DB) CleanupHealthStatsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	if d == nil || d.db == nil {
		return 0, errors.New("pebble: db not initialized")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
	}

	cutoff = cutoff.UTC().Truncate(time.Hour)
	upper := []byte(fmt.Sprintf("%s%s/", keyHealthHourPref, hourKey(cutoff)))
	lower := keyHealthHourPrefixAll()

	iter, err := d.db.NewIter(&pebblepkg.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return 0, fmt.Errorf("pebble iter cleanup: %w", err)
	}
	defer iter.Close()

	batch := d.db.NewBatch()
	defer batch.Close()

	var deleted int64
	for iter.First(); iter.Valid(); iter.Next() {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return deleted, err
			}
		}
		k := append([]byte(nil), iter.Key()...)
		if err := batch.Delete(k, nil); err != nil {
			return deleted, fmt.Errorf("pebble batch delete: %w", err)
		}
		deleted++
	}
	if err := iter.Error(); err != nil {
		return deleted, fmt.Errorf("pebble iter cleanup: %w", err)
	}

	if deleted == 0 {
		return 0, nil
	}
	if err := batch.Commit(pebblepkg.Sync); err != nil {
		return deleted, fmt.Errorf("pebble batch commit cleanup: %w", err)
	}
	return deleted, nil
}
