package pebble

import (
	"encoding/json"
	"fmt"

	"easy_proxies/internal/store"
)

type nodeRecord struct {
	Node store.Node `json:"node"`
}

type healthHourRecord struct {
	SuccessCount  int64 `json:"success_count"`
	FailCount     int64 `json:"fail_count"`
	LatencySumMs  int64 `json:"latency_sum_ms"`
	LatencyCount  int64 `json:"latency_count"`
	UpdatedAtUnix int64 `json:"updated_at_unix"`
}

func marshalNode(n store.Node) ([]byte, error) {
	return json.Marshal(nodeRecord{Node: n})
}

func unmarshalNode(b []byte) (store.Node, error) {
	var r nodeRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return store.Node{}, fmt.Errorf("decode node: %w", err)
	}
	return r.Node, nil
}

func marshalHealthHour(r healthHourRecord) ([]byte, error) {
	return json.Marshal(r)
}

func unmarshalHealthHour(b []byte) (healthHourRecord, error) {
	var r healthHourRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return healthHourRecord{}, fmt.Errorf("decode health hour: %w", err)
	}
	return r, nil
}
