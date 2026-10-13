package discordguard

import (
	"strconv"
	"testing"
	"time"
)

// The guard sits in front of every write merlin makes: one rate check and
// one breaker report per call.
func BenchmarkAllowAndRecord(b *testing.B) {
	g := New(nil, fakeGate{}, testLogger(), false)
	now := time.Now()
	b.ReportAllocs()
	for b.Loop() {
		_ = g.allowRate("1459404984943644747", opMessageSend, now)
		g.recordResult("1459404984943644747", now, nil)
		now = now.Add(time.Minute) // keep the bucket refilled
	}
}

func BenchmarkAllowParallel(b *testing.B) {
	g := New(nil, fakeGate{}, testLogger(), false)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		now, i := time.Now(), 0
		for pb.Next() {
			i++
			_ = g.allowRate(strconv.Itoa(i%64), opMessageSend, now)
			now = now.Add(time.Minute)
		}
	})
}
