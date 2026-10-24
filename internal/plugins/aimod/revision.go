package aimod

import (
	"context"
	"hash/fnv"
	"sync"
	"time"
)

// Work in flight outlives the moment it was judged. A deep pass can take
// tens of seconds, and in that time a guild can switch the plugin off,
// move a bucket to flag, or the author can edit the message. Acting on the
// verdict anyway deleted messages a guild had just told merlin to leave
// alone, and reposted a rewrite of words the author had already corrected.
//
// So enforce re-reads policy immediately before touching anything
// (stillActs), discards a verdict on text the author has since edited
// (superseded), and runs one enforcement per message at a time. The edit
// itself is rescanned by HandleMessageEdit and judged on its own words.
//
// What is left is the external race: an edit that lands between this check
// and Discord carrying out the delete. Closing that would need Discord to
// offer a conditional delete, which it does not.

// revisionTTL is how long an edit is remembered. Comfortably past the
// longest a verdict can be in flight (spawn's two minute context).
const revisionTTL = 10 * time.Minute

type revision struct {
	hash uint64
	at   time.Time
}

func contentHash(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// noteRevision records a message's text as of an edit. Only a hash is kept,
// so this holds no copy of anything a member wrote.
func (p *Plugin) noteRevision(messageID, content string) {
	now := p.now()
	p.revisionMu.Lock()
	defer p.revisionMu.Unlock()
	if p.revisions == nil {
		p.revisions = make(map[string]revision)
	}
	for id, r := range p.revisions {
		if now.Sub(r.at) > revisionTTL {
			delete(p.revisions, id)
		}
	}
	p.revisions[messageID] = revision{hash: contentHash(content), at: now}
}

// superseded reports whether the message has been edited away from the text
// c carries, so a verdict on c no longer describes what is in the channel.
func (p *Plugin) superseded(c candidate) bool {
	p.revisionMu.Lock()
	defer p.revisionMu.Unlock()
	r, ok := p.revisions[c.MessageID]
	return ok && r.hash != contentHash(c.Content)
}

// lockMessage serializes enforcement on one message.
func (p *Plugin) lockMessage(messageID string) func() {
	p.actingMu.Lock()
	if p.acting == nil {
		p.acting = make(map[string]*messageLock)
	}
	l, ok := p.acting[messageID]
	if !ok {
		l = &messageLock{}
		p.acting[messageID] = l
	}
	l.n++
	p.actingMu.Unlock()

	l.Lock()
	return func() {
		l.Unlock()
		p.actingMu.Lock()
		if l.n--; l.n == 0 {
			delete(p.acting, messageID)
		}
		p.actingMu.Unlock()
	}
}

type messageLock struct {
	sync.Mutex
	n int // holders and waiters, guarded by Plugin.actingMu
}

// stillActs re-reads the guild's policy just before a message is touched,
// and returns the config and action enforcement should go on with. ok is
// false when the answer is now "do nothing at all".
//
// It only ever moves an action down: a guild that tightened its policy
// meanwhile gets that on the next message, not by retroactively escalating
// this one.
func (p *Plugin) stillActs(ctx context.Context, cfg Config, bucket Bucket, action Action, c candidate) (Config, Action, bool) {
	if p.gate != nil && !p.gate.PluginEnabled(cfg.GuildID, p.Name()) {
		return cfg, action, false
	}
	live, err := p.store.Config(ctx, cfg.GuildID)
	if err != nil {
		// Unknown policy is not permission to delete.
		p.log.Error("aimod: re-read config before acting, taking no action", "guild", cfg.GuildID, "err", err)
		return cfg, action, false
	}
	if live.Mode == ModeOff {
		return live, action, false
	}
	if bucket == BucketWordList {
		if _, hit := redactWords(live.WordList, c.Content); !hit {
			return live, action, false
		}
		return live, action, true
	}
	// Compared with the snapshot the verdict was reached under, so only a
	// change made while it was in flight moves it. The caller's own
	// adjustments (an empty rewrite becoming a removal) stand.
	if now := EffectiveAction(live.BucketActions, bucket); now != EffectiveAction(cfg.BucketActions, bucket) {
		switch now {
		case ActionOff:
			return live, action, false
		case ActionFlag:
			return live, ActionFlag, true
		}
	}
	return live, action, true
}
