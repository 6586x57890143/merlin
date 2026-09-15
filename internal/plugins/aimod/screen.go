package aimod

import (
	"context"
	"fmt"
	"strings"
)

// Screen judges one piece of text a member is about to have posted in their
// name by this bot. The whisper plugin is the caller, through its own narrow
// interface, wired in cmd/bot/main.go the way Jailer is.
//
// refusal is a sentence for the member; empty means post it. err is
// informational: the model was unavailable and the caller decides whether to
// post on the free rungs alone, which is what /whisper does.
//
// The ladder is HandleMessage's, but the failure directions differ, and that
// is the whole reason this is a separate entry point rather than a synthetic
// MessageCreate. There, nothing exists until a verdict lands, so an
// unavailable model means a message stays up. Here the text does not exist
// yet, merlin is the publisher, and declining costs the member one retry. So:
//
//   - Rung 1 runs unconditionally: gate, mode and key are all irrelevant to a
//     hard slur or a leaked token, and the slur rewrite is thrown away. Nothing
//     this bot posts in somebody's name gets a daft substitution.
//   - The model rungs run only where aimod is switched on and funded; otherwise
//     this returns clean, and the caller's own regex suite is the guard.
//   - A fast hit is confirmed by the deep pass before it refuses anything, the
//     same rule that keeps rung 2 off the delete path. But a confirmed verdict
//     refuses regardless of the guild's per-bucket action or flag mode: those
//     decide what happens to a member's own message, and this is the bot's.
//   - The member opt-out (optout.go) does not apply. Opting out of being judged
//     is one thing; asking merlin to post something is a request, and the
//     screening is the condition of it.
func (p *Plugin) Screen(ctx context.Context, guildID, channelID, authorID, text string) (refusal string, err error) {
	text = strings.TrimSpace(text)
	if _, reason, _, hit := hardHit(text); hit {
		return reason, nil
	}
	if p.gate != nil && !p.gate.PluginEnabled(guildID, p.Name()) {
		return "", nil
	}
	cfg, err := p.store.Config(ctx, guildID)
	if err != nil {
		return "", fmt.Errorf("aimod: load config: %w", err)
	}
	if cfg.Mode == ModeOff || len(enforcedBuckets(cfg)) == 0 {
		return "", nil
	}
	now := p.now()
	// Remembered verdicts first: a repeat of refused text is refused for
	// free, and a repeat of cleared text is cleared for free. Same cache the
	// firehose uses, so a whisper and a message agree about the same words.
	if e, ok := p.dedupe.lookup(guildID, text, now); ok {
		if e.verdict == nil {
			return "", nil
		}
		if EffectiveAction(cfg.BucketActions, e.verdict.bucket) != ActionOff {
			return refuseFor(e.verdict.bucket, e.verdict.deep), nil
		}
	}
	state, err := p.checkBudget(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("aimod: budget check: %w", err)
	}
	if state.Exhausted {
		return "", nil
	}
	// The member's own ceiling, not an outage: over it, the answer is no
	// rather than "post unjudged", or the ceiling would be a way to get a
	// whisper past the model by making enough noise first.
	if !p.meter.allowScan(guildID, authorID, now) {
		return "you are over your scan ceiling for now; try again in a while", nil
	}

	c := candidate{ChannelID: channelID, AuthorID: authorID, Content: text}
	hits, err := p.fastPass(ctx, cfg, state, []candidate{c})
	if err != nil {
		return "", fmt.Errorf("aimod: fast pass: %w", err)
	}
	if len(hits) == 0 {
		p.dedupe.markClean(guildID, text, now)
		return "", nil
	}
	hit := hits[0]
	if EffectiveAction(cfg.BucketActions, hit.Bucket) == ActionOff {
		return "", nil
	}
	if allowed, _ := p.meter.allowDeep(guildID, authorID, now); !allowed {
		return "you are over your scan ceiling for now; try again in a while", nil
	}
	v, err := p.deepPass(ctx, cfg, state, hit.Bucket, c, nil, "", false)
	if err != nil {
		return "", fmt.Errorf("aimod: deep pass: %w", err)
	}
	if !v.Violation || v.Confidence < actThreshold {
		p.dedupe.markCleanFor(guildID, authorID, text, now)
		return "", nil
	}
	p.dedupe.remember(guildID, text, now, &cachedVerdict{
		bucket: hit.Bucket, action: EffectiveAction(cfg.BucketActions, hit.Bucket), deep: v,
	})
	return refuseFor(hit.Bucket, v), nil
}

func refuseFor(bucket Bucket, v deepVerdict) string {
	v.Bucket = bucket
	return strings.ReplaceAll(string(bucket), "_", " ") + ": " + reasonOrPolicy(v)
}
