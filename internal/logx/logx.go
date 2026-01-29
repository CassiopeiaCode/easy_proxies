package logx

import (
	"log"
	"runtime"
	"sync"
	"time"
)

const maxPerSecondPerCallsite = 10

type bucket struct {
	mu     sync.Mutex
	window time.Time
	count  int
}

var buckets sync.Map // map[string]*bucket

func allow(callsite string, now time.Time) bool {
	v, _ := buckets.LoadOrStore(callsite, &bucket{})
	b := v.(*bucket)

	win := now.Truncate(time.Second)

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.window.IsZero() || !b.window.Equal(win) {
		b.window = win
		b.count = 0
	}

	if b.count >= maxPerSecondPerCallsite {
		return false
	}
	b.count++
	return true
}

func callsiteKey(skip int) string {
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "unknown:0"
	}
	return file + ":" + itoa(line)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [32]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + (n % 10))
		n /= 10
	}
	return string(buf[i:])
}

func Printf(format string, args ...any) {
	now := time.Now()
	if !allow(callsiteKey(2), now) {
		return
	}
	log.Printf(format, args...)
}

func Println(args ...any) {
	now := time.Now()
	if !allow(callsiteKey(2), now) {
		return
	}
	log.Println(args...)
}

func Print(args ...any) {
	now := time.Now()
	if !allow(callsiteKey(2), now) {
		return
	}
	log.Print(args...)
}