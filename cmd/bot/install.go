package main

import (
	"sync"
	"time"
)

// installSettle is how long the install settings wait after the last thing
// that could change them before being recomputed.
const installSettle = 30 * time.Second

// installer runs sync once things have been quiet for settle, however many
// pokes arrive meanwhile: a boot's worth of guilds is one sync, not one each.
type installer struct {
	mu     sync.Mutex
	settle time.Duration
	sync   func()
	timer  *time.Timer
}

func newInstaller(settle time.Duration, sync func()) *installer {
	return &installer{settle: settle, sync: sync}
}

func (i *installer) poke() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.timer != nil {
		i.timer.Stop()
	}
	i.timer = time.AfterFunc(i.settle, i.sync)
}
