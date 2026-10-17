// Package audit implements core.AuditWriter: every config or state change is
// one entry in the guild's ledger chain (audit_log, cygnet/ledger) and one
// card in its log channel (spec.MD Design Principle 4, "config changes are
// audited, not silent"). A run of the same action by the same actor shares
// a card that edits itself as it grows, so a jail of five members or a sweep
// of three archives is one card in the channel and five or three rows here.
package audit

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/6586x57890143/cygnet/ledger"

	"github.com/6586x57890143/merlin/internal/core"
)

// channelResolver is the narrow view of guild settings Writer needs,
// implemented by internal/settings.Store.
type channelResolver interface {
	AuditLogChannelID(guildID string) string
}

// Retention for audit_log. The table had none and grew without bound.
//
// A year is deliberately long: this is the moderation trail spec.MD Design
// Principle 4 exists to keep, and the whole point of channel rotation is that
// the *content* is gone: the record of what the bot did, and who told it to,
// is what remains. It is also small (one row per config change or automated
// action), so a year of it is nothing next to the cost of not being able to
// answer "who changed the retention window in March."
const (
	auditRetention     = 365 * 24 * time.Hour
	retentionInterval  = 6 * time.Hour
	retentionOpTimeout = 60 * time.Second
)

// Writer implements core.AuditWriter.
type Writer struct {
	pool     *pgxpool.Pool
	store    *Store
	rest     rest.Rest
	settings channelResolver
	cards    *ledger.Batcher
	now      func() time.Time
}

func New(pool *pgxpool.Pool, r rest.Rest, settings channelResolver) *Writer {
	w := &Writer{pool: pool, store: NewStore(pool), rest: r, settings: settings, now: time.Now}
	w.cards = &ledger.Batcher{Brand: brand, Post: w.post, Edit: w.edit, Now: func() time.Time { return w.now() }}
	return w
}

// Record writes one entry to the guild's chain and shows it in the guild's
// log channel. Both must succeed for a nil error, but the row (the durable
// trail spec.MD Design Principle 4 requires) is written first and
// independently of the card. Callers log a non-nil error and carry on
// rather than fail the action that triggered it: a missing or deleted log
// channel means the live notice was missed, not that the action or its
// record failed. Every call site follows that policy.
func (w *Writer) Record(ctx context.Context, guildID, actorID, action, oldValue, newValue string) error {
	e := toEntry(guildID, actorID, action, oldValue, newValue, w.now().UTC())
	r, err := w.store.appendRaw(ctx, e, oldValue, newValue)
	if err != nil {
		return err
	}
	return w.cards.Add(ctx, r.Entry)
}

// post sends a new card to guildID's log channel and returns
// "<channel>/<message>", which is what edit needs back.
//
// Sent on the raw REST client rather than through discordguard, on purpose.
// The guard refuses writes when a guild is paused or in dry-run, which
// would silence the log at exactly the moment somebody hit the emergency
// stop and most wants to know what the bot is doing, and would guarantee
// the rotation.dryrun and archive.dryrun entries (which exist because the
// guild is in dry-run) were never posted. This is a record of what
// happened, not an action, and it is not the guard's to refuse. So
// AllowedMentions is spelled out as an empty parse list here: the card
// mentions members by design and must ping none of them.
func (w *Writer) post(_ context.Context, guildID string, card ledger.Card) (string, error) {
	channelID := core.ID(w.settings.AuditLogChannelID(guildID))
	if channelID == 0 {
		return "", fmt.Errorf("audit: guild %s has no audit log channel configured", guildID)
	}
	msg, err := w.rest.CreateMessage(channelID, discord.MessageCreate{
		Embeds:          []discord.Embed{card.Embed},
		Components:      rows(card),
		Files:           core.EmbedUploads(card.Embed),
		AllowedMentions: &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}},
	})
	if err != nil {
		return "", fmt.Errorf("audit: post card: %w", err)
	}
	return channelID.String() + "/" + msg.ID.String(), nil
}

// edit replaces the card post returned ref for.
func (w *Writer) edit(_ context.Context, _ string, ref string, card ledger.Card) error {
	ch, msg, _ := strings.Cut(ref, "/")
	components := rows(card)
	if _, err := w.rest.UpdateMessage(core.ID(ch), core.ID(msg), discord.MessageUpdate{
		Embeds:     &[]discord.Embed{card.Embed},
		Components: &components,
		// Replaced, not added to: an edit that only uploaded would pile
		// another copy of each image onto the message every time it grew.
		Files:           core.EmbedUploads(card.Embed),
		Attachments:     &[]discord.AttachmentUpdate{},
		AllowedMentions: &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}},
	}); err != nil {
		return fmt.Errorf("audit: edit card: %w", err)
	}
	return nil
}

// rows is the card's buttons as the message's components, empty when it has
// none (an edit sends it as is, clearing a row the card no longer has).
func rows(card ledger.Card) []discord.LayoutComponent {
	if card.Buttons == nil {
		return []discord.LayoutComponent{}
	}
	return []discord.LayoutComponent{*card.Buttons}
}

// StartRetention prunes audit_log past auditRetention until ctx is
// cancelled, and prunes anything else passed in (the action journal) on the
// same tick.
//
// Housekeeping runs on its own goroutine rather than as a Scheduler job:
// these tables are process-wide, not per guild, and the Scheduler's job keys,
// failure alerting, and persisted last-run state are all built around a guild
// that this work doesn't have. Missing a tick is harmless: the next one
// prunes the same rows.
func (w *Writer) StartRetention(ctx context.Context, log *slog.Logger, extra ...Pruner) {
	go func() {
		ticker := time.NewTicker(retentionInterval)
		defer ticker.Stop()
		for {
			w.pruneOnce(ctx, log, extra)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Pruner is anything with its own age-based retention to enforce,
// implemented by discordguard's action journal.
type Pruner interface {
	PruneBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

func (w *Writer) pruneOnce(ctx context.Context, log *slog.Logger, extra []Pruner) {
	opCtx, cancel := context.WithTimeout(ctx, retentionOpTimeout)
	defer cancel()

	cutoff := time.Now().UTC().Add(-auditRetention)
	tag, err := w.pool.Exec(opCtx, `DELETE FROM audit_log WHERE created_at < $1`, cutoff)
	if err != nil {
		log.Error("audit: retention prune failed", "err", err)
	} else if n := tag.RowsAffected(); n > 0 {
		log.Info("audit: pruned expired audit rows", "rows", n, "older_than", auditRetention)
	}

	for _, p := range extra {
		n, err := p.PruneBefore(opCtx, time.Now().UTC().Add(-journalRetention))
		if err != nil {
			log.Error("audit: journal prune failed", "err", err)
			continue
		}
		if n > 0 {
			log.Info("audit: pruned expired journal rows", "rows", n, "older_than", journalRetention)
		}
	}
}

// journalRetention is much shorter than the audit trail's: the action journal
// is diagnostic ("what did the bot try to do, and what came back") and is
// useful for reconstructing a recent incident, not for answering questions
// about last spring.
const journalRetention = 30 * 24 * time.Hour
