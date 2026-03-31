package pebble

import (
	"encoding/binary"
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

var healthHourBinaryMagic = [4]byte{'H', 'H', 'R', '1'}

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
	b := make([]byte, 4+8*5)
	copy(b[:4], healthHourBinaryMagic[:])
	binary.BigEndian.PutUint64(b[4:12], uint64(r.SuccessCount))
	binary.BigEndian.PutUint64(b[12:20], uint64(r.FailCount))
	binary.BigEndian.PutUint64(b[20:28], uint64(r.LatencySumMs))
	binary.BigEndian.PutUint64(b[28:36], uint64(r.LatencyCount))
	binary.BigEndian.PutUint64(b[36:44], uint64(r.UpdatedAtUnix))
	return b, nil
}

func unmarshalHealthHour(b []byte) (healthHourRecord, error) {
	if len(b) == 44 && string(b[:4]) == string(healthHourBinaryMagic[:]) {
		return healthHourRecord{
			SuccessCount:  int64(binary.BigEndian.Uint64(b[4:12])),
			FailCount:     int64(binary.BigEndian.Uint64(b[12:20])),
			LatencySumMs:  int64(binary.BigEndian.Uint64(b[20:28])),
			LatencyCount:  int64(binary.BigEndian.Uint64(b[28:36])),
			UpdatedAtUnix: int64(binary.BigEndian.Uint64(b[36:44])),
		}, nil
	}

	var r healthHourRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return healthHourRecord{}, fmt.Errorf("decode health hour: %w", err)
	}
	return r, nil
}
