package roles

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/ui"
	"github.com/6586x57890143/merlin/internal/discordguard"
	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/voice"
)

// Supporters is who backs the birds now, as cygnet keeps it
// (cygnet.supporters). main.go wires the fleet link in, so this package
// never imports it.
type Supporters interface {
	Supporters(ctx context.Context) ([]Backer, error)
}

// SupportersFunc is a function as Supporters.
type SupportersFunc func(ctx context.Context) ([]Backer, error)

// Supporters calls f.
func (f SupportersFunc) Supporters(ctx context.Context) ([]Backer, error) { return f(ctx) }

// Backer is one supporter: a Discord user and when their paid time ends.
type Backer struct {
	UserID string
	Until  time.Time
}

// How often the list is read, and what merlin names the role she creates.
const (
	supporterRoleName         = "Supporter"
	supporterSyncInterval     = 10 * time.Minute
	supporterReason           = "supporter, via cygnet"
	// supporterRemindAhead is how long before backing ends its DM goes.
	supporterRemindAhead = 7 * 24 * time.Hour
	supportURL           = "https://cygnet.lol/support"
)

// WithSupporters turns on the supporter role sync, in every guild that
// turns supporters on from /roles settings.
func (p *Plugin) WithSupporters(s Supporters) { p.supporters = s }

// registerSupporterSync arms the sync job for guildID when the fleet link
// is wired. Whether the guild has supporters on is read on every run, so
// turning them on or off needs no re-registration. Called from SyncGuild
// with p.mu held.
func (p *Plugin) registerSupporterSync(guildID string) {
	if p.supporters == nil {
		return
	}
	key := scheduler.JobKey(guildID, "roles-supporters")
	err := p.sched.Register(key, core.CronSpec{Schedule: core.IntervalSchedule{Interval: supporterSyncInterval}}, func(ctx context.Context) error {
		role := p.jailChannelConfig.SupporterRoleID(guildID)
		if role == "" {
			return nil
		}
		if err := p.syncSupporters(ctx, guildID, role); err != nil && !discordguard.Skipped(err) {
			return err
		}
		return nil
	})
	if err != nil {
		p.log.Error("roles: register supporter sync", "guild", guildID, "err", err)
	}
}

// syncSupporters gives every current supporter the role as a timed grant
// ending when their paid time does, so the grant timer and the sweep take it
// away again with nothing new to keep. It only ever adds and re-dates: a
// member who holds the role without a grant from merlin (given by hand) and
// a permanent grant are left alone, and a jailed member is skipped until the
// jail ends. A renewal moves the grant's end later, never earlier.
func (p *Plugin) syncSupporters(ctx context.Context, guildID, roleID string) error {
	if p.dryRun(guildID) {
		return nil
	}
	if err := p.perms.CanManageRole(guildID, roleID); err != nil {
		return fmt.Errorf("supporter role: %w", err)
	}
	backers, err := p.supporters.Supporters(ctx)
	if err != nil {
		return fmt.Errorf("supporters: %w", err)
	}
	var errs []error
	var joined []string
	for _, b := range backers {
		if !b.Until.After(p.now()) {
			continue
		}
		fresh, err := p.syncSupporter(ctx, guildID, roleID, b)
		if err != nil {
			errs = append(errs, err)
		}
		if fresh {
			joined = append(joined, b.UserID)
		}
		if err := p.remindSupporter(ctx, guildID, b); err != nil {
			errs = append(errs, err)
		}
	}
	p.thankSupporters(ctx, guildID, joined)
	return errors.Join(errs...)
}

// createSupporterRole makes merlin's own supporter role for guildID, with
// no permissions (it is a badge), and turns supporters on with it. Discord
// puts a new role at the bottom, under merlin's top role, so she can always
// give and take it. A guild that already has one keeps it.
func (p *Plugin) createSupporterRole(ctx context.Context, guildID string) (string, error) {
	if id := p.jailChannelConfig.SupporterRoleID(guildID); id != "" {
		return id, nil
	}
	none := discord.PermissionsNone
	role, err := p.ops(guildID).CreateRole(core.ID(guildID), discord.RoleCreate{Name: supporterRoleName, Permissions: &none})
	if err != nil {
		return "", fmt.Errorf("create supporter role: %w", err)
	}
	id := role.ID.String()
	return id, p.jailChannelConfig.SetSupporterRole(ctx, guildID, id)
}

// maxThanked is the most new supporters one card names; past it, "and N
// more", so a backlog (the first sync after launch) is still one card.
const maxThanked = 10

// thankSupporters posts one thank you card for members who just became
// supporters, in the channel picked on /roles settings. One card a sync,
// never on a renewal, and nothing pings: the guard's senders zero
// AllowedMentions. It carries the one small ad, a code-authored field
// saying where. Best effort: no channel picked, or one merlin can't post
// in, means no card.
func (p *Plugin) thankSupporters(ctx context.Context, guildID string, userIDs []string) {
	channel := p.jailChannelConfig.SupporterChannelID(guildID)
	if len(userIDs) == 0 || channel == "" {
		return
	}
	names := userIDs
	more := ""
	if len(names) > maxThanked {
		names, more = names[:maxThanked], fmt.Sprintf(" and %d more", len(userIDs)-maxThanked)
	}
	body := p.voice.Line(ctx, guildID, voice.KeySupporterThanks, map[string]string{"members": listMentions(names) + more})
	if body == "" {
		return
	}
	embed := core.Embed(core.ColorPrimary, "Thank you", body,
		discord.EmbedField{Name: "Back the birds", Value: supportURL})
	core.SetMood(&embed, core.MoodSupporter)
	if _, err := p.ops(guildID).CreateMessage(core.ID(channel), discord.MessageCreate{
		Embeds: []discord.Embed{embed},
		Files:  core.EmbedUploads(embed),
	}); err != nil {
		p.log.Info("roles: could not thank supporters", "guild", guildID, "err", err)
	}
}

// listMentions is user mentions joined the owner's way: "a, b and c".
func listMentions(ids []string) string {
	m := make([]string, len(ids))
	for i, id := range ids {
		m[i] = core.MentionUser(id)
	}
	if len(m) < 2 {
		return strings.Join(m, "")
	}
	return strings.Join(m[:len(m)-1], ", ") + " and " + m[len(m)-1]
}

// remindSupporter DMs b a week before their backing ends, once per paid
// period: the claim is written before the DM, so a failed write never
// sends it twice, and a renewal moves Until and so earns its own. A DM
// that can't be delivered (closed DMs) is not retried.
func (p *Plugin) remindSupporter(ctx context.Context, guildID string, b Backer) error {
	if b.Until.Sub(p.now()) > supporterRemindAhead {
		return nil
	}
	first, err := p.store.ClaimSupporterReminder(ctx, b.UserID, b.Until)
	if err != nil || !first {
		return err
	}
	p.dm(ctx, guildID, b.UserID, voice.KeySupporterReminder, "Your backing ends soon", core.ColorInfo,
		map[string]string{"when": relativeTimestamp(b.Until)},
		discord.EmbedField{Name: "Renew", Value: supportURL})
	return nil
}

// syncSupporter gives or re-dates b's grant, and reports whether b just
// became a supporter here (a first grant, not a renewal).
func (p *Plugin) syncSupporter(ctx context.Context, guildID, roleID string, b Backer) (bool, error) {
	rec, granted, err := p.store.GetGrant(ctx, guildID, b.UserID, roleID)
	if err != nil {
		return false, err
	}
	if granted && (rec.ExpiresAt == nil || !rec.ExpiresAt.Before(b.Until)) {
		return false, nil
	}
	if _, jailed, err := p.store.GetJail(ctx, guildID, b.UserID); err != nil || jailed {
		return false, err
	}
	member, err := p.ops(guildID).GetMember(core.ID(guildID), core.ID(b.UserID))
	if core.HasDiscordErrorCode(err, int(rest.JSONErrorCodeUnknownMember)) {
		return false, nil // not in this server; the next read gives it them if they join
	}
	if err != nil {
		return false, err
	}
	holds := slices.Contains(core.IDStrings(member.RoleIDs), roleID)
	if !granted && holds {
		return false, nil // given by hand: not merlin's to time out
	}
	if !holds {
		if err := p.ops(guildID).AddMemberRole(core.ID(guildID), core.ID(b.UserID), core.ID(roleID)); err != nil {
			return false, err
		}
	}
	until := b.Until
	if err := p.store.InsertGrant(ctx, GrantRecord{
		GuildID: guildID, UserID: b.UserID, RoleID: roleID,
		GrantedAt: p.now(), ExpiresAt: &until, GrantedBy: core.ActorSystem, Reason: supporterReason,
	}); err != nil {
		return false, err
	}
	p.armGrantRevoke(guildID, b.UserID, roleID, until)
	action := "roles.supporter_grant"
	if granted {
		action = "roles.supporter_renew"
	}
	if err := p.audit.Record(ctx, guildID, core.ActorSystem, action, "", fmt.Sprintf("user=%s role=%s until=%s", core.MentionUser(b.UserID), core.MentionRole(roleID), until.UTC().Format(time.RFC3339))); err != nil {
		p.log.Error("roles: audit supporter grant failed", "guild", guildID, "user", b.UserID, "err", err)
	}
	return !granted, nil
}

// supporterParts is the supporters page of /roles settings: turn them on
// (merlin makes her own role), and where new ones are thanked.
func (p *Plugin) supporterParts(guildID string, canSet bool) []discord.ContainerSubComponent {
	role := p.jailChannelConfig.SupporterRoleID(guildID)
	if role == "" {
		return []discord.ContainerSubComponent{
			ui.Switch("**Supporters are off.** Turned on, merlin makes a **Supporter** role with no permissions and gives it to whoever backs the birds at cygnet.lol/support, for as long as they do.",
				ui.Button("Turn on", panelJail+"suprole:", discord.ButtonStyleSecondary, !canSet)),
		}
	}
	thanks := "nowhere"
	if ch := p.jailChannelConfig.SupporterChannelID(guildID); ch != "" {
		thanks = core.MentionChannel(ch)
	}
	return []discord.ContainerSubComponent{
		ui.Switch("**Supporters are on**: whoever backs the birds holds "+core.MentionRole(role)+" for as long as they do.\n-# Turning them off keeps the role and lets the ones given run out.",
			ui.Button("Turn off", panelJail+"supoff:", discord.ButtonStyleDanger, !canSet)),
		ui.Text("**Thank you card**: one card when somebody becomes a supporter, never on a renewal.\n-# Now: " + thanks),
		ui.Row(ui.Clearable(ui.ChannelSelect(panelJail+"supchan", "Thank new supporters in", p.jailChannelConfig.SupporterChannelID(guildID), !canSet, discord.ChannelTypeGuildText))),
	}
}

// setSupporters applies one control on the supporters page.
func (p *Plugin) setSupporters(ctx context.Context, ix *core.Ix, verb string) string {
	g := ix.GuildID
	switch verb {
	case "suprole":
		id, err := p.createSupporterRole(ctx, g)
		if err != nil {
			return fmt.Sprintf("✗ Couldn't make the supporter role: %v", err)
		}
		p.auditConfig(ctx, ix, "", "supporter_role="+core.MentionRole(id))
		return "✓ Supporters are on: " + core.MentionRole(id) + " goes to whoever backs the birds."
	case "supoff":
		old := p.jailChannelConfig.SupporterRoleID(g)
		if err := p.jailChannelConfig.SetSupporterRole(ctx, g, ""); err != nil {
			return fmt.Sprintf("✗ Couldn't turn supporters off: %v", err)
		}
		p.auditConfig(ctx, ix, "supporter_role="+core.MentionRole(old), "supporter_role=none")
		return "✓ Supporters are off. The role stays, and the ones given run out on their own."
	default:
		ch := ix.FirstValue()
		if err := p.jailChannelConfig.SetSupporterChannel(ctx, g, ch); err != nil {
			return fmt.Sprintf("✗ Couldn't save the channel: %v", err)
		}
		if ch == "" {
			p.auditConfig(ctx, ix, "", "supporter_channel=none")
			return "✓ New supporters won't be thanked in a channel."
		}
		p.auditConfig(ctx, ix, "", "supporter_channel="+core.MentionChannel(ch))
		return "✓ New supporters will be thanked in " + core.MentionChannel(ch) + "."
	}
}
