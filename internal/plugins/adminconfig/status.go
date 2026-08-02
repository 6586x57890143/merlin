package adminconfig

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/settings"
)

// JobHealth and DBHealth are the narrow views /config status needs of the
// Scheduler and the database, defined here rather than taken from core.Deps
// so this plugin can be tested against tiny fakes — the same seam pattern as
// rotation.SettingsProvider and roles.JailChannelConfig.
type JobHealth interface {
	JobHealth(ctx context.Context, guildID string) (total, failing int, err error)
}

type DBHealth interface {
	Healthy(ctx context.Context) error
}

// handleStatus answers "is Merlin okay?" in one embed, from inside Discord.
//
// Everything it reports was previously only visible by reading container
// logs over SSH: whether the database is reachable, whether any scheduled job
// is wedged, whether someone left the guild paused or in dry-run, and whether
// the channels and roles the guild configured still exist. Those last two
// matter because a deleted audit-log channel doesn't announce itself — the
// audit trail simply stops appearing, and the bot goes on working.
//
// TierMod, not TierAdmin: it reads state and changes nothing, and a mod
// noticing "rotation is wedged" hours before an admin logs in is the entire
// point.
func (p *Plugin) handleStatus(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	gs := p.settings.GuildSettings(i.GuildID)
	var b strings.Builder

	// Database first: if it's down, everything below is suspect, and saying
	// so plainly beats reporting a lot of stale-looking numbers.
	if p.db == nil {
		b.WriteString("**Database** — not wired up\n")
	} else if err := p.db.Healthy(ctx); err != nil {
		fmt.Fprintf(&b, "**Database** — ❌ unreachable: %v\n", err)
	} else {
		b.WriteString("**Database** — ✅ reachable\n")
	}

	if p.jobs == nil {
		b.WriteString("**Jobs** — not wired up\n")
	} else {
		total, failing, err := p.jobs.JobHealth(ctx, i.GuildID)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "**Jobs** — ⚠️ %d registered, state unreadable: %v\n", total, err)
		case total == 0:
			b.WriteString("**Jobs** — none registered (no rotation configured yet)\n")
		case failing > 0:
			fmt.Fprintf(&b, "**Jobs** — ⚠️ %d of %d failing; see `/scheduler list`\n", failing, total)
		default:
			fmt.Fprintf(&b, "**Jobs** — ✅ %d registered, all healthy\n", total)
		}
	}

	switch {
	case gs.WritesPaused && gs.WritesDryRun:
		b.WriteString("**Actions** — ⛔ paused *and* in dry-run; nothing will be changed\n")
	case gs.WritesPaused:
		b.WriteString("**Actions** — ⛔ paused; clear with `/config pause paused:false`\n")
	case gs.WritesDryRun:
		b.WriteString("**Actions** — 🧪 dry-run; clear with `/config dryrun enabled:false`\n")
	default:
		b.WriteString("**Actions** — ✅ running normally\n")
	}

	b.WriteString("\n")
	b.WriteString(p.configuredResourceLines(gs))

	// Warn rather than OK whenever something needs a human, so the colour
	// alone carries the answer at a glance.
	body := b.String()
	if strings.Contains(body, "❌") || strings.Contains(body, "⚠️") || strings.Contains(body, "⛔") {
		core.RespondWarn(s, i, "Merlin status", body)
		return
	}
	core.RespondInfo(s, i, "Merlin status", body)
}

// configuredResourceLines reports whether each configured channel/role still
// exists. A channel deleted out from under the configuration is silent
// otherwise: audit embeds and status alerts simply stop arriving, with
// nothing anywhere saying why.
func (p *Plugin) configuredResourceLines(gs settings.GuildSettings) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Audit log** — %s\n", p.channelState(gs.AuditLogChannelID))
	fmt.Fprintf(&b, "**Status channel** — %s\n", p.channelState(gs.StatusChannelID))

	switch n := len(gs.ModRoleIDs); n {
	case 0:
		b.WriteString("**Mod roles** — ⚠️ none configured; only admins can use mod commands\n")
	default:
		fmt.Fprintf(&b, "**Mod roles** — %d configured\n", n)
	}
	fmt.Fprintf(&b, "**Admins** — %d configured (plus anyone with Discord's Administrator permission)\n",
		len(gs.AdminUserIDs))
	return b.String()
}

func (p *Plugin) channelState(channelID string) string {
	if channelID == "" {
		return "⚠️ not configured; run `/config setup`"
	}
	if p.session == nil {
		return fmt.Sprintf("<#%s>", channelID)
	}
	if _, err := p.session.Channel(channelID); err != nil {
		if core.IsUnknownResource(err) {
			return fmt.Sprintf("❌ configured as `%s`, but that channel no longer exists — re-run `/config setup`", channelID)
		}
		return fmt.Sprintf("<#%s> (couldn't verify: %v)", channelID, err)
	}
	return fmt.Sprintf("<#%s> ✅", channelID)
}
