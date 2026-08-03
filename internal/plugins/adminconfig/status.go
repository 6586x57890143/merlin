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
// so this plugin can be tested against tiny fakes, the same seam pattern as
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
// matter because a deleted audit-log channel doesn't announce itself: the
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
		b.WriteString("**Database:** not wired up\n")
	} else if err := p.db.Healthy(ctx); err != nil {
		fmt.Fprintf(&b, "**Database:** ❌ unreachable: %v\n", err)
	} else {
		b.WriteString("**Database:** ✅ reachable\n")
	}

	if p.jobs == nil {
		b.WriteString("**Jobs:** not wired up\n")
	} else {
		total, failing, err := p.jobs.JobHealth(ctx, i.GuildID)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "**Jobs:** ⚠️ %d registered, state unreadable: %v\n", total, err)
		case total == 0:
			b.WriteString("**Jobs:** none registered (no rotation configured yet)\n")
		case failing > 0:
			fmt.Fprintf(&b, "**Jobs:** ⚠️ %d of %d failing; see `/scheduler list`\n", failing, total)
		default:
			fmt.Fprintf(&b, "**Jobs:** ✅ %d registered, all healthy\n", total)
		}
	}

	switch {
	case gs.WritesPaused && gs.WritesDryRun:
		b.WriteString("**Actions:** ⛔ paused *and* in dry-run; nothing will be changed\n")
	case gs.WritesPaused:
		b.WriteString("**Actions:** ⛔ paused; clear with `/config pause paused:false`\n")
	case gs.WritesDryRun:
		b.WriteString("**Actions:** 🧪 dry-run; clear with `/config dryrun enabled:false`\n")
	default:
		b.WriteString("**Actions:** ✅ running normally\n")
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
	fmt.Fprintf(&b, "**Audit log:** %s\n", p.channelState(gs.AuditLogChannelID))
	fmt.Fprintf(&b, "**Status channel:** %s\n", p.channelState(gs.StatusChannelID))

	switch n := len(gs.ModRoleIDs); n {
	case 0:
		b.WriteString("**Mod roles:** ⚠️ none configured; only admins can use mod commands\n")
	default:
		fmt.Fprintf(&b, "**Mod roles:** %d configured\n", n)
	}
	fmt.Fprintf(&b, "**Admins:** %d configured (plus anyone with Discord's Administrator permission)\n",
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
	ch, err := p.session.Channel(channelID)
	if err != nil {
		if core.IsUnknownResource(err) {
			return fmt.Sprintf("❌ configured as `%s`, but that channel no longer exists. Re-run `/config setup`", channelID)
		}
		return fmt.Sprintf("<#%s> (couldn't verify: %v)", channelID, err)
	}
	if everyoneCanRead(ch) {
		return fmt.Sprintf("<#%s> ⚠️ everyone can read this; moderation actions and alerts are public", channelID)
	}
	return fmt.Sprintf("<#%s> ✅", channelID)
}

// everyoneCanRead reports whether the guild's @everyone role can still see
// ch.
//
// This is checked here, rather than only when the channel is chosen,
// because it drifts: a channel that was private when an admin picked it
// stops being private the moment someone removes the overwrite, and nothing
// about the audit log continuing to fill up says otherwise. The wizard's
// own "create it for me" path applies core.DenyEveryoneExceptBot, but
// picking an existing channel deliberately changes no permissions on a
// channel the guild already uses for something, so that path can leave a
// perfectly public channel configured as the audit log.
//
// It answers "yes" whenever it is unsure, because the two mistakes are not
// symmetric: a spurious warning costs an admin ten seconds, while a missed
// one means every jail, every config change, and every job failure has been
// published to the whole server without anyone noticing.
//
// Reading the channel's own overwrites is sufficient, including for
// category-synced channels, since syncing copies the category's overwrites
// onto the channel rather than leaving them to be inherited at read time.
func everyoneCanRead(ch *discordgo.Channel) bool {
	if ch == nil {
		return true
	}
	// The @everyone role's ID is the guild's own ID.
	for _, ow := range ch.PermissionOverwrites {
		if ow.Type != discordgo.PermissionOverwriteTypeRole || ow.ID != ch.GuildID {
			continue
		}
		if ow.Deny&discordgo.PermissionViewChannel != 0 {
			return false
		}
	}
	return true
}
