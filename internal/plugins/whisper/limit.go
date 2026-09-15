package whisper

import (
	"sync"
	"time"
)

// The ceilings. Conversations have to work, so the per-member gap is a light
// slowmode rather than a cooldown; what the caps bound is a flood.
//
// Three keys, each for a different reason. The member cap is the abuse
// control and does not scale with anything: one restricted account gets the
// same few dozen an hour on a server of fifty or of fifty thousand. The
// channel cap sits under Discord's own 30/min per-webhook limit, so a hot
// channel gets a polite refusal from here rather than 429s that would open
// discordguard's breaker for the whole guild. The guild cap scales with
// membership (guildHourly), since the number that makes sense for a
// two-hundred member server would gag a two-thousand member one, and it has
// its own discordguard bucket (webhook.whisper) so none of this touches
// aimod's rewrite budget.
const (
	userGap        = 3 * time.Second
	userHourly     = 60
	channelMinute  = 25
	guildPerMember = 1
	guildFloor     = 200
)

// guildHourly is the guild's hourly whisper cap for a given member count:
// one per member per hour, never under guildFloor. An unknown count (a
// guild the state cache has not seen) gets the floor, which fails toward
// the smaller number.
func guildHourly(members int) int {
	return max(guildFloor, members*guildPerMember)
}

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
func (l *limiter) allow(key string, now time.Time, window, gap time.Duration, max int) bool {
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
