package pebble

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()

	db, err := Open(context.Background(), Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	return db
}

func TestUnmarshalHealthHourSupportsLegacyJSON(t *testing.T) {
	legacy, err := json.Marshal(healthHourRecord{
		SuccessCount:  3,
		FailCount:     1,
		LatencySumMs:  120,
		LatencyCount:  2,
		UpdatedAtUnix: 123,
	})
	if err != nil {
		t.Fatalf("marshal legacy json: %v", err)
	}

	got, err := unmarshalHealthHour(legacy)
	if err != nil {
		t.Fatalf("unmarshal health hour: %v", err)
	}
	if got.SuccessCount != 3 || got.FailCount != 1 || got.LatencySumMs != 120 || got.LatencyCount != 2 || got.UpdatedAtUnix != 123 {
		t.Fatalf("unexpected decoded record: %+v", got)
	}
}

func TestQueryNodeRatesSinceSupportsMixedHealthHourFormats(t *testing.T) {
	db := openTestDB(t)
	hour := time.Now().UTC().Truncate(time.Hour)

	legacyValue, err := json.Marshal(healthHourRecord{
		SuccessCount:  2,
		FailCount:     1,
		LatencySumMs:  0,
		LatencyCount:  0,
		UpdatedAtUnix: hour.Unix(),
	})
	if err != nil {
		t.Fatalf("marshal legacy json: %v", err)
	}
	if err := db.db.Set(keyHealthHour(hour, "legacy.example", 443), legacyValue, nil); err != nil {
		t.Fatalf("set legacy record: %v", err)
	}

	modernValue, err := marshalHealthHour(healthHourRecord{
		SuccessCount:  5,
		FailCount:     0,
		LatencySumMs:  0,
		LatencyCount:  0,
		UpdatedAtUnix: hour.Unix(),
	})
	if err != nil {
		t.Fatalf("marshal binary record: %v", err)
	}
	if err := db.db.Set(keyHealthHour(hour, "modern.example", 8443), modernValue, nil); err != nil {
		t.Fatalf("set binary record: %v", err)
	}

	rates, err := db.QueryNodeRatesSince(context.Background(), hour.Add(-time.Minute))
	if err != nil {
		t.Fatalf("query node rates: %v", err)
	}
	if len(rates) != 2 {
		t.Fatalf("expected 2 rates, got %d", len(rates))
	}

	if rates[0].Host != "legacy.example" || rates[0].Port != 443 || rates[0].SuccessCount != 2 || rates[0].FailCount != 1 {
		t.Fatalf("unexpected legacy rate: %+v", rates[0])
	}
	if rates[1].Host != "modern.example" || rates[1].Port != 8443 || rates[1].SuccessCount != 5 || rates[1].FailCount != 0 {
		t.Fatalf("unexpected modern rate: %+v", rates[1])
	}
}
