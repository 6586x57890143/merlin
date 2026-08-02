// Package audit implements core.AuditWriter: every config/state change gets
// written to an append-only DB table and posted as an embed to the guild's
// #bird-audit-log channel (spec.MD Design Principle 4, "config changes are
// audited, not silent").
package audit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"

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
	session  *discordgo.Session
	settings channelResolver
}

func New(pool *pgxpool.Pool, session *discordgo.Session, settings channelResolver) *Writer {
	return &Writer{pool: pool, session: session, settings: settings}
}

// Record inserts an append-only audit_log row and posts a matching embed to
// the guild's configured AuditLogChannelID. Both the DB write and the
// Discord post must succeed for a nil error, but the DB row (the actual
// durable audit trail spec.MD Design Principle 4 requires) is written
// first and independently of the embed post. Callers should log a non-nil
// error and continue rather than fail the action that triggered it: a
// missing/deleted #bird-audit-log channel means the live notification was
// missed, not that the underlying action (or the audit trail itself) failed.
// See every call site (rotation/execute.go, sweep.go, adminconfig.go,
// rotation/configure.go) for the consistent policy this implies.
func (w *Writer) Record(ctx context.Context, guildID, actorID, action, oldValue, newValue string) error {
	if _, err := w.pool.Exec(ctx, `
		INSERT INTO audit_log (guild_id, actor_id, action, old_value, new_value)
		VALUES ($1, $2, $3, $4, $5)
	`, guildID, actorID, action, oldValue, newValue); err != nil {
		return fmt.Errorf("audit: record: %w", err)
	}

	channelID := w.settings.AuditLogChannelID(guildID)
	if channelID == "" {
		return fmt.Errorf("audit: guild %s has no audit log channel configured", guildID)
	}

	embed := &discordgo.MessageEmbed{
		Title:     action,
		Color:     core.ColorSuccess,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Actor", Value: actorID, Inline: true},
		},
	}
	// Truncated because an embed field over 1024 bytes doesn't get trimmed by
	// Discord, it fails the entire message, so a single long value (a sweep
	// listing many channel IDs, a rotation config with a long sticky) would
	// silently cost the guild its live audit notification for that action.
	// The durable row above is already written in full and is unaffected.
	if oldValue != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Old", Value: core.TruncateEmbedField(oldValue), Inline: true})
	}
	if newValue != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "New", Value: core.TruncateEmbedField(newValue), Inline: true})
	}

	if _, err := w.session.ChannelMessageSendEmbed(channelID, embed); err != nil {
		return fmt.Errorf("audit: post embed: %w", err)
	}
	return nil
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
