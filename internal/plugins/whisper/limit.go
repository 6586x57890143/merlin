package whisper

import (
	"sync"
	"time"
)

// The ceilings. Conversations have to work, so the per-member gap is a light
// slowmode rather than a cooldown; what the hourly caps bound is a flood.
//
// The guild cap sits under discordguard's 300/hour webhook.execute budget
// on purpose. aimod's rewrites share that budget, and a rewrite that has
// already deleted the original and then cannot repost degrades silently to
// a removal. A whisper flood must not be the thing that spends it.
const (
	userGap     = 3 * time.Second
	userHourly  = 60
	guildHourly = 200
	window      = time.Hour
)

// limiter is a sliding window of attempts per key, pruned on use. Attempts
// are counted whether or not the whisper was posted, so a refused whisper
// is not a free retry.
//
// ponytail: unbounded map of keys, pruned only when a key is touched. Fine
// for two guilds; give it a sweep if merlin ever runs in hundreds.
type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newLimiter() *limiter { return &limiter{hits: make(map[string][]time.Time)} }

// allow records an attempt under key and reports whether it is within
// limits: at most max in the last window, and none inside gap.
func (l *limiter) allow(key string, now time.Time, gap time.Duration, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	ok := len(kept) < max && (len(kept) == 0 || now.Sub(kept[len(kept)-1]) >= gap)
	if ok {
		kept = append(kept, now)
	}
	l.hits[key] = kept
	return ok
}
