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

// handleStatus answers "is merlin okay?" in one embed, from inside Discord.
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
// severity is how much a status line needs a human, tracked as the body is
// built rather than recovered from it afterwards.
//
// This replaced a scan of the finished text for ❌/⚠️/⛔. That worked, but the
// control signal for the response colour was being read back out of prose
// this bot does not author: channelState interpolates arbitrary Discord error
// text, and Healthy() interpolates arbitrary database error text, so any
// error string containing a warning glyph turned a healthy server amber, and
// any future line that merely mentioned one did the same. The emoji are cues
// for the reader now, not load-bearing state.
type severity int

const (
	sevOK severity = iota
	// sevStopped is a deliberate operator state: paused, or in dry-run.
	// Reported as a warning but wearing the idle face, matching /config pause
	// and /config dryrun. A bot that was told to stop has not failed.
	sevStopped
	sevWarn
	sevError
)

func (p *Plugin) handleStatus(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	gs := p.settings.GuildSettings(i.GuildID)
	var b strings.Builder
	worst := sevOK
	note := func(sev severity) {
		if sev > worst {
			worst = sev
		}
	}

	// Database first: if it's down, everything below is suspect, and saying
	// so plainly beats reporting a lot of stale-looking numbers.
	if p.db == nil {
		b.WriteString("**Database:** not wired up\n")
	} else if err := p.db.Healthy(ctx); err != nil {
		fmt.Fprintf(&b, "**Database:** ❌ unreachable: %v\n", err)
		note(sevError)
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
			note(sevWarn)
		case total == 0:
			b.WriteString("**Jobs:** none registered (no rotation configured yet)\n")
		case failing > 0:
			fmt.Fprintf(&b, "**Jobs:** ⚠️ %d of %d failing; see `/scheduler list`\n", failing, total)
			note(sevWarn)
		default:
			fmt.Fprintf(&b, "**Jobs:** ✅ %d registered, all healthy\n", total)
		}
	}

	switch {
	case gs.WritesPaused && gs.WritesDryRun:
		b.WriteString("**Actions:** ⛔ paused *and* in dry-run; nothing will be changed\n")
		note(sevStopped)
	case gs.WritesPaused:
		b.WriteString("**Actions:** ⛔ paused; clear with `/config pause paused:false`\n")
		note(sevStopped)
	case gs.WritesDryRun:
		b.WriteString("**Actions:** 🧪 dry-run; clear with `/config dryrun enabled:false`\n")
		note(sevStopped)
	default:
		b.WriteString("**Actions:** ✅ running normally\n")
	}

	b.WriteString("\n")
	resources, resourceSev := p.configuredResourceLines(gs)
	b.WriteString(resources)
	note(resourceSev)

	// Truncated because the body interpolates error text from the database,
	// the scheduler and per-channel lookups, none of it bounded and none of
	// it ours. Over 4096 bytes Discord rejects the whole message, so without
	// this the health command would break in exactly the circumstances that
	// produce a long error.
	body := core.TruncateEmbedDescription(b.String())

	switch worst {
	case sevError, sevWarn:
		core.RespondWarn(s, i, "merlin status", body)
	case sevStopped:
		embed := core.WithMood(core.NewEmbed(core.ColorWarning, "merlin status", body), core.MoodIdle)
		_ = core.RespondEmbed(s, i, embed)
	default:
		core.RespondInfo(s, i, "merlin status", body)
	}
}

// configuredResourceLines reports whether each configured channel/role still
// exists. A channel deleted out from under the configuration is silent
// otherwise: audit embeds and status alerts simply stop arriving, with
// nothing anywhere saying why.
func (p *Plugin) configuredResourceLines(gs settings.GuildSettings) (string, severity) {
	var b strings.Builder
	worst := sevOK
	note := func(sev severity) {
		if sev > worst {
			worst = sev
		}
	}

	audit, sev := p.channelState(gs.AuditLogChannelID)
	fmt.Fprintf(&b, "**Audit log:** %s\n", audit)
	note(sev)
	status, sev := p.channelState(gs.StatusChannelID)
	fmt.Fprintf(&b, "**Status channel:** %s\n", status)
	note(sev)

	// Named rather than counted. "2 configured" answers a question nobody is
	// asking: an admin reading this wants to know *which* roles carry mod
	// powers in their server, and a count sends them off to a different
	// command to find out. The channels above were already mentions, so the
	// counts were also the odd ones out on the same screen.
	if len(gs.ModRoleIDs) == 0 {
		b.WriteString("**Mod roles:** ⚠️ none configured; only admins can use mod commands\n")
		note(sevWarn)
	} else {
		fmt.Fprintf(&b, "**Mod roles:** %s\n", mentionList(gs.ModRoleIDs, core.MentionRole))
	}
	if len(gs.AdminUserIDs) == 0 {
		b.WriteString("**Admins:** none listed (anyone with Discord's Administrator permission still counts)\n")
	} else {
		fmt.Fprintf(&b, "**Admins:** %s (plus anyone with Discord's Administrator permission)\n",
			mentionList(gs.AdminUserIDs, core.MentionUser))
	}
	return b.String(), worst
}

// maxMentionsListed caps how many roles or admins are named before the rest
// become a count.
//
// A readability limit, not a byte limit: a role mention is around 22 bytes,
// so even twice this is a rounding error against the 4096-byte description.
// Past about ten the line stops being scannable and the count is genuinely
// the more useful summary.
const maxMentionsListed = 10

func mentionList(ids []string, render func(string) string) string {
	shown := ids
	var suffix string
	if len(ids) > maxMentionsListed {
		shown = ids[:maxMentionsListed]
		suffix = fmt.Sprintf(", and %d more", len(ids)-maxMentionsListed)
	}
	parts := make([]string, 0, len(shown))
	for _, id := range shown {
		parts = append(parts, render(id))
	}
	return strings.Join(parts, ", ") + suffix
}

func (p *Plugin) channelState(channelID string) (string, severity) {
	if channelID == "" {
		return "⚠️ not configured; run `/config setup`", sevWarn
	}
	if p.session == nil {
		return fmt.Sprintf("<#%s>", channelID), sevOK
	}
	ch, err := p.session.Channel(channelID)
	if err != nil {
		if core.IsUnknownResource(err) {
			return fmt.Sprintf("❌ configured as `%s`, but that channel no longer exists. Re-run `/config setup`", channelID), sevError
		}
		return fmt.Sprintf("<#%s> (couldn't verify: %v)", channelID, err), sevWarn
	}
	if everyoneCanRead(ch) {
		return fmt.Sprintf("<#%s> ⚠️ everyone can read this; moderation actions and alerts are public", channelID), sevWarn
	}
	return fmt.Sprintf("<#%s> ✅", channelID), sevOK
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
