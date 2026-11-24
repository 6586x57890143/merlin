package aimod

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
)

// Reposting a member's message for another plugin (dragoman's translations),
// without opening a way round this one.
//
// A plugin that deleted a message and put a copy up by webhook would be a
// moderation bypass. The verdict on a message takes seconds, webhooks are
// never scanned, and enforce reads a missing message as one somebody already
// removed: so the copy of a violation would stay up, unsanctioned. Three
// things close that, and they are why the repost lives here:
//
//   - the swap holds the same per-message lock enforce does, and reads the
//     live message first, so a message already removed is never put back;
//   - the move is remembered, so a verdict still in flight on the original
//     lands on the copy (enforce, clearPendingFlags);
//   - the new words are scanned as the author's own message, and where they
//     are a softened version, the original goes to the deep pass, so softening
//     a violation never hides it.

// moveTTL is how long a move is followed. Past meterWindow, the furthest
// back clearPendingFlags reaches for a message.
const moveTTL = 30 * time.Minute

// move is one repost. done (acted on, nothing left to do) and answered (a
// verdict on the original has been carried out) are guarded by the repost's
// message lock, the rest never changes.
type move struct {
	guildID          string
	original, repost string
	keep             bool
	softened         bool
	at               time.Time
	done, answered   bool
}

func (p *Plugin) noteMove(m *move) {
	p.movesMu.Lock()
	defer p.movesMu.Unlock()
	if p.moves == nil {
		p.moves = make(map[string]*move)
	}
	for id, old := range p.moves {
		if m.at.Sub(old.at) > moveTTL {
			delete(p.moves, id)
		}
	}
	p.moves[m.original], p.moves[m.repost] = m, m
}

// moved is the repost messageID was part of, on either side, or nil.
func (p *Plugin) moved(messageID string) *move {
	p.movesMu.Lock()
	defer p.movesMu.Unlock()
	return p.moves[messageID]
}

func (p *Plugin) forgetMoves(guildID string) {
	p.movesMu.Lock()
	defer p.movesMu.Unlock()
	for id, m := range p.moves {
		if m.guildID == guildID {
			delete(p.moves, id)
		}
	}
}

// follow points c at the repost, and at a kept original beside it.
func (m *move) follow(c candidate) candidate {
	c.MessageID = m.repost
	if m.keep {
		c.AlsoDelete = m.original
	}
	return c
}

// Reasons a repost did not happen that are no failure of anything; see
// unavailableError for the contract.
var (
	errRepostGone    error = unavailableError{"aimod: the message was deleted or edited before it could be reposted"}
	errRepostRefused error = unavailableError{"aimod: the new text is not publishable"}
)

// hintOriginal sends a message somebody translating it called harmful to the
// deep pass, unless this guild wouldn't have scanned it at all.
func (p *Plugin) hintOriginal(cfg Config, r core.Repost) {
	bucket := Bucket(r.Policy)
	m := message{
		ID: r.MessageID, GuildID: r.GuildID, ChannelID: r.ChannelID, AuthorID: r.AuthorID,
		RoleIDs: r.RoleIDs, Content: r.Original, ReplyTo: r.ReplyTo,
	}
	if !p.scanning || !known(bucket) || EffectiveAction(cfg.BucketActions, bucket) == ActionOff ||
		p.shouldSkip(cfg, m, p.now()) != skipNone || scanExempt(cfg, r.AuthorID, r.Original) {
		return
	}
	c := candidate{MessageID: m.ID, ChannelID: m.ChannelID, AuthorID: m.AuthorID, Content: m.Content, ReplyTo: replyContext(m)}
	p.spawn(func(bg context.Context) {
		p.confirmHint(bg, cfg, c, &cachedVerdict{bucket: bucket, deep: deepVerdict{Confidence: actThreshold}})
	})
}

// Repost replaces one message with r.Text under its author's name, and
// returns the new message's ID. Any error means the original is untouched.
//
// Posted before the original is deleted, the reverse of rewriteMessage:
// nothing here is a violation that has to come down, so what must not happen
// is a member's words vanishing because a webhook failed.
func (p *Plugin) Repost(ctx context.Context, r core.Repost) (string, error) {
	if p.gate != nil && !p.gate.PluginEnabled(r.GuildID, p.Name()) {
		return "", ErrNoKey
	}
	cfg, err := p.store.Config(ctx, r.GuildID)
	if err != nil {
		return "", fmt.Errorf("aimod: read config: %w", err)
	}
	// A hint on the original survives whatever happens to the repost.
	if r.Policy != "" {
		defer p.hintOriginal(cfg, r)
	}
	// Nothing in this area has a harmless version, as it has no rewrite that
	// is a joke: it is moderation's to take down, not ours to reword.
	// And an original that stays up beneath a softened copy is not softened.
	if Bucket(r.Policy) == BucketChildSafety || (r.Policy != "" && r.Keep) {
		return "", errRepostRefused
	}
	text := strings.TrimSpace(r.Text)
	if out, hit := redactWords(cfg.WordList, text); hit {
		text = strings.TrimSpace(out)
	}
	text, swapped := redactSlurs(text)
	// What is left of rung 1 is tokens and phishing links, which have no
	// harmless version.
	if _, _, _, hit := hardHit(text); hit || text == "" || utf8.RuneCountInString(text+r.Marker) > maxContentLen {
		return "", errRepostRefused
	}

	defer p.lockMessage(r.MessageID)()
	ops := p.ops(r.GuildID)
	live, err := ops.Message(r.ChannelID, r.MessageID)
	if core.IsUnknownResource(err) || (err == nil && live.Content != r.Original) {
		return "", errRepostGone
	}
	if err != nil {
		return "", fmt.Errorf("aimod: read message: %w", err)
	}
	hook, err := p.resolveWebhook(ctx, r.GuildID, r.ChannelID)
	if err != nil {
		return "", fmt.Errorf("aimod: resolve webhook: %w", err)
	}
	author, err := ops.Member(r.GuildID, r.AuthorID)
	if err != nil {
		return "", fmt.Errorf("aimod: fetch author: %w", err)
	}
	id, err := ops.PostAs(hook, discord.WebhookMessageCreate{
		Content: text + r.Marker, Username: displayName(author), AvatarURL: author.AvatarURL,
	}, true)
	if err != nil {
		p.forgetWebhook(r.ChannelID)
		return "", fmt.Errorf("aimod: repost: %w", err)
	}
	if !r.Keep {
		if err := ops.DeleteMessage(r.ChannelID, r.MessageID); err != nil {
			// The copy comes back down, whichever way the delete failed: a
			// message already gone was taken down by its author or a mod.
			if derr := ops.DeleteMessage(r.ChannelID, id); derr != nil {
				p.log.Error("aimod: take back a repost whose original stayed", "guild", r.GuildID,
					"channel", r.ChannelID, "message", id, "err", derr)
			}
			if core.IsUnknownResource(err) {
				return "", errRepostGone
			}
			return "", fmt.Errorf("aimod: delete original: %w", err)
		}
	}
	p.noteMove(&move{guildID: r.GuildID, original: r.MessageID, repost: id, keep: r.Keep,
		softened: swapped || r.Policy != "", at: p.now()})
	p.handle(message{
		ID: id, GuildID: r.GuildID, ChannelID: r.ChannelID, AuthorID: r.AuthorID,
		RoleIDs: r.RoleIDs, Content: text, ReplyTo: r.ReplyTo,
	})
	return id, nil
}

// Policies is one line per policy area this guild enforces, for a prompt
// that has to know what counts as harmful here.
func (p *Plugin) Policies(ctx context.Context, guildID string) string {
	cfg, err := p.store.Config(ctx, guildID)
	if err != nil {
		cfg = defaultConfig(guildID)
	}
	var b strings.Builder
	for _, bucket := range enforcedBuckets(cfg) {
		if pol, ok := p.policies[bucket]; ok {
			fmt.Fprintf(&b, "- %s: %s\n", bucket, pol.Short)
		}
	}
	return b.String()
}

// Context is the few messages before messageID, each labelled with a letter
// for its speaker, and the letter authorID got. No IDs and no names.
func (p *Plugin) Context(guildID, channelID, messageID, authorID string) (lines []string, self string) {
	return p.recentContext(guildID, candidate{ChannelID: channelID, MessageID: messageID, AuthorID: authorID})
}

// Budget is what the guild has spent on models today and its daily cap. ok is
// false where there is no key to spend with, or no reading it.
func (p *Plugin) Budget(ctx context.Context, guildID string) (spent, limit float64, ok bool) {
	if p.gate != nil && !p.gate.PluginEnabled(guildID, p.Name()) {
		return 0, 0, false
	}
	cfg, err := p.store.Config(ctx, guildID)
	if err != nil {
		return 0, 0, false
	}
	state, err := p.checkBudget(ctx, cfg)
	return state.Spent, state.Budget, err == nil && state.APIKey != ""
}

// EvidenceHours is how long the guild keeps message text; zero, or an
// unreadable config, is none.
func (p *Plugin) EvidenceHours(ctx context.Context, guildID string) int {
	cfg, err := p.store.Config(ctx, guildID)
	if err != nil {
		return 0
	}
	return cfg.EvidenceHours
}
