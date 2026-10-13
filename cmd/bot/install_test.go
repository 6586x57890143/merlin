package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// A burst of pokes is one sync, once they stop.
func TestInstallerSyncsOnceAfterABurst(t *testing.T) {
	var n atomic.Int32
	i := newInstaller(30*time.Millisecond, func() { n.Add(1) })
	for range 10 {
		i.poke()
		time.Sleep(5 * time.Millisecond)
	}
	if n.Load() != 0 {
		t.Fatal("synced during the burst")
	}
	time.Sleep(100 * time.Millisecond)
	if n.Load() != 1 {
		t.Fatalf("synced %d times, want 1", n.Load())
	}
}
