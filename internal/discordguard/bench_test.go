package discordguard

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
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
	guilds := make([]string, 64)
	for i := range guilds {
		guilds[i] = strconv.Itoa(i)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		now, i := time.Now(), 0
		for pb.Next() {
			i++
			_ = g.allowRate(guilds[i%len(guilds)], opMessageSend, now)
			now = now.Add(time.Minute)
		}
	})
}

// With no lock, the cap holds by compare-and-swap alone: a burst from many
// goroutines at one instant gets exactly the cap, never one more.
func TestCapHoldsUnderConcurrency(t *testing.T) {
	g := New(nil, fakeGate{}, testLogger(), false)
	now := time.Now()
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 50 {
				err := g.allowRate("g", opRoleCreate, now)
				if err == nil {
					allowed.Add(1)
				} else if !errors.Is(err, ErrRateLimited) {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if got := allowed.Load(); got != int32(opCaps[opRoleCreate]) {
		t.Fatalf("%d allowed, want the cap of %d", got, opCaps[opRoleCreate])
	}
	// One step later, exactly one more.
	later := now.Add(capWindow / time.Duration(opCaps[opRoleCreate]))
	if g.allowRate("g", opRoleCreate, later) != nil || g.allowRate("g", opRoleCreate, later) == nil {
		t.Fatal("one step of refill did not admit exactly one call")
	}
}
