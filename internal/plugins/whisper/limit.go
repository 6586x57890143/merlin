package whisper

import (
	"sync"
	"time"
)

// The ceilings. Conversations have to work, so the per-member gap is a light
// slowmode rather than a cooldown; what the caps bound is a flood.
//
// Three keys, each for a different reason. The member cap is the abuse
// control: a floor of userFloor an hour, plus a share of whatever the guild
// has not used of its own hour (userHourly). A flat sixty was reached by
// one person having an ordinary conversation, and what a flood costs is
// other members' share of the guild cap, which is exactly nothing while the
// guild is quiet; as it fills up, everyone drops back to the floor. The
// channel cap sits under Discord's own per-webhook limit (webhookMinute),
// so a hot channel gets a polite refusal from here rather than 429s that
// would open discordguard's breaker for the whole guild. The guild cap
// (guildHourly) has a floor of one webhook's full hour, since every channel
// has its own webhook and there is no reason to allow a guild less than one
// of them, and scales with membership above that, since a number sized for
// a small server gags a large one. It has its own discordguard bucket
// (webhook.whisper) so none of this touches aimod's rewrite budget.
const (
	userGap   = 3 * time.Second
	userFloor = 120
	userShare = 2
	// webhookMinute is Discord's documented rate limit per webhook.
	webhookMinute  = 30
	channelMinute  = webhookMinute - 5
	guildPerMember = 1
	guildFloor     = webhookMinute * 60
)

// userHourly is one member's hourly cap given how much of the guild's hour
// is still unused: never under userFloor, never more than 1/userShare of
// the remainder, so no single account can take the whole guild cap and gag
// everyone else for the hour.
func userHourly(guildRemaining int) int {
	return max(userFloor, guildRemaining/userShare)
}

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
	kept := l.pruneLocked(key, now, window)
	ok := len(kept) < max && (len(kept) == 0 || now.Sub(kept[len(kept)-1]) >= gap)
	if ok {
		kept = append(kept, now)
	}
	l.hits[key] = kept
	return ok
}

// count reports the attempts under key in the last window without
// recording one.
func (l *limiter) count(key string, now time.Time, window time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.pruneLocked(key, now, window)
	l.hits[key] = kept
	return len(kept)
}

func (l *limiter) pruneLocked(key string, now time.Time, window time.Duration) []time.Time {
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	return kept
}
