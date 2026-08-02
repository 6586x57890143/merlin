// Package audit implements core.AuditWriter: every config/state change gets
// written to an append-only DB table and posted as an embed to the guild's
// #bird-audit-log channel (spec.MD Design Principle 4 — "config changes are
// audited, not silent").
package audit

import (
	"context"
	"fmt"
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
// Discord post must succeed for a nil error, but the DB row — the actual
// durable audit trail spec.MD Design Principle 4 requires — is written
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
	if oldValue != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Old", Value: oldValue, Inline: true})
	}
	if newValue != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "New", Value: newValue, Inline: true})
	}

	if _, err := w.session.ChannelMessageSendEmbed(channelID, embed); err != nil {
		return fmt.Errorf("audit: post embed: %w", err)
	}
	return nil
}
