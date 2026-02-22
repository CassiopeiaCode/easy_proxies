package pebble

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Keyspace prefixes.
const (
	keyNodePrefix     = "node/"      // node/<host>/<port>
	keyHealthHourPref = "hc_hour/"   // hc_hour/<YYYYMMDDHH>/<host>/<port>
)

func keyNode(host string, port int) []byte {
	host = strings.TrimSpace(host)
	return []byte(fmt.Sprintf("%s%s/%d", keyNodePrefix, host, port))
}

func keyNodePrefixBytes() []byte { return []byte(keyNodePrefix) }

func parseNodeKey(key []byte) (host string, port int, ok bool) {
	if !strings.HasPrefix(string(key), keyNodePrefix) {
		return "", 0, false
	}
	s := strings.TrimPrefix(string(key), keyNodePrefix)
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return "", 0, false
	}
	host = parts[0]
	p, err := strconv.Atoi(parts[1])
	if err != nil || p <= 0 || p > 65535 {
		return "", 0, false
	}
	return host, p, true
}

func hourKey(ts time.Time) string {
	// store UTC truncated hour as YYYYMMDDHH for easy prefix scan.
	return ts.UTC().Truncate(time.Hour).Format("2006010215")
}

func keyHealthHour(hour time.Time, host string, port int) []byte {
	host = strings.TrimSpace(host)
	return []byte(fmt.Sprintf("%s%s/%s/%d", keyHealthHourPref, hourKey(hour), host, port))
}

func keyHealthHourPrefix(hour time.Time) []byte {
	return []byte(keyHealthHourPref + hourKey(hour) + "/")
}

func keyHealthHourPrefixAll() []byte { return []byte(keyHealthHourPref) }

// encodeInt64 encodes int64 into big endian so lexical order matches numeric order.
func encodeInt64(v int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v)^0x8000000000000000)
	return b
}
