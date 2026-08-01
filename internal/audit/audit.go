// Package audit implements core.AuditWriter: every config/state change gets
// written to an append-only DB table and posted as an embed to the guild's
// #bot-audit-log channel (spec.MD Design Principle 4 — "config changes are
// audited, not silent").
package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/6586x57890143/merlin/internal/config"
)

// Writer implements core.AuditWriter.
type Writer struct {
	pool    *pgxpool.Pool
	session *discordgo.Session
	cfg     *config.Loader
}

func New(pool *pgxpool.Pool, session *discordgo.Session, cfg *config.Loader) *Writer {
	return &Writer{pool: pool, session: session, cfg: cfg}
}

// Record inserts an append-only audit_log row and posts a matching embed to
// the guild's configured AuditLogChannelID. Both the DB write and the
// Discord post must succeed for a nil error — callers should treat a
// failure as "the audit trail is incomplete" and propagate it rather than
// swallow it, per spec's fail-safe-not-fail-silent principle. A retried
// caller may produce a harmless duplicate audit_log row; this is
// acceptable for an append-only trail that's meant to over-record rather
// than under-record.
func (w *Writer) Record(ctx context.Context, guildID, actorID, action, oldValue, newValue string) error {
	if _, err := w.pool.Exec(ctx, `
		INSERT INTO audit_log (guild_id, actor_id, action, old_value, new_value)
		VALUES ($1, $2, $3, $4, $5)
	`, guildID, actorID, action, oldValue, newValue); err != nil {
		return fmt.Errorf("audit: record: %w", err)
	}

	gc, err := w.cfg.Guild(guildID)
	if err != nil {
		return fmt.Errorf("audit: resolve guild config: %w", err)
	}

	embed := &discordgo.MessageEmbed{
		Title:     action,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Actor", Value: actorID, Inline: true},
		},
	}
	if oldValue != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Old", Value: oldValue, Inline: true})
	}
	if newValue != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "New", Value: newValue, Inline: true})
	}

	if _, err := w.session.ChannelMessageSendEmbed(gc.AuditLogChannelID, embed); err != nil {
		return fmt.Errorf("audit: post embed: %w", err)
	}
	return nil
}
