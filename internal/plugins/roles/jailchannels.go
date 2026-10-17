package roles

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
)

// channelSyncConcurrency bounds how many permission-overwrite writes run at
// once during a guild-wide resync (syncAllJailChannelOverwrites,
// clearMemberJailOverwrites). Each channel has its
// own Discord rate-limit bucket for permission writes, so concurrent writes
// to different channels don't contend with each other; this cap exists only
// to keep a large guild's fan-out well clear of Discord's global per-bot
// rate limit, not because discordguard's own opChannelPermissions cap
// (governor.go) is tight, while still being a large speedup over one write
// at a time.
const channelSyncConcurrency = 10

// forEachChannelConcurrent runs fn for each channel across a bounded worker
// pool (channelSyncConcurrency), rather than one Discord API call at a time.
// One channel's failure is logged (via onErr) and doesn't stop the rest, and
// the first error seen (if any) is returned once every channel has been
// tried, matching the log-and-continue policy every jail-channel sync
// already used before this existed, just without paying for it serially.
func (p *Plugin) forEachChannelConcurrent(channels []discord.GuildChannel, fn func(discord.GuildChannel) error, onErr func(discord.GuildChannel, error)) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, channelSyncConcurrency)
	for _, ch := range channels {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := fn(ch); err != nil {
				onErr(ch, err)
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return firstErr
}

// JailChannelConfig is the narrow slice of internal/settings.Store this
// plugin depends on for jail's channel-visibility allowlist. Which channels
// stay visible to a jailed member is guild configuration, not jail/grant
// runtime state, so it belongs in settings.Store rather than this plugin's
// own pgStore. Mirrors rotation's own settings-vs-store split exactly.
type JailChannelConfig interface {
	JailAllowedChannelIDs(guildID string) []string
	AddJailAllowedChannel(ctx context.Context, guildID, channelID string) error
	RemoveJailAllowedChannel(ctx context.Context, guildID, channelID string) error
	// Optional per-guild configured jail marker role. If set, the plugin will
	// use this existing role as the jail role instead of auto-creating a
	// "Jailed" role.
	JailMarkerRoleID(guildID string) string
	SetJailMarkerRole(ctx context.Context, guildID, roleID string) error
	// Optional single extra channel jail/release announcements go to, on
	// top of the channel the command was run in. Empty means none; an empty
	// channelID passed to the setter clears it.
	JailAnnounceChannelID(guildID string) string
	SetJailAnnounceChannel(ctx context.Context, guildID, channelID string) error
	ClearJailMarkerRole(ctx context.Context, guildID string) error
	// The vacation script's marker role (script_vacation.go). Empty means
	// none; an empty roleID passed to the setter clears it. Unlike the jail
	// marker, merlin never creates this role and never syncs its overwrites.
	VacationRoleID(guildID string) string
	SetVacationRole(ctx context.Context, guildID, roleID string) error
	// The island's additive allowlist: channels merlin writes an allow
	// overwrite for the vacation role on. Never a deny anywhere.
	VacationAllowedChannelIDs(guildID string) []string
	AddVacationAllowedChannel(ctx context.Context, guildID, channelID string) error
	RemoveVacationAllowedChannel(ctx context.Context, guildID, channelID string) error
	// The ordinary member role (memberBaseline). Empty means @everyone
	// alone; an empty roleID passed to the setter clears it.
	MemberRoleID(guildID string) string
	SetMemberRole(ctx context.Context, guildID, roleID string) error
	// The extra roles the join-role script gives a new member besides
	// MemberRoleID (script_joinrole.go).
	JoinRoleIDs(guildID string) []string
	AddJoinRole(ctx context.Context, guildID, roleID string) error
	RemoveJoinRole(ctx context.Context, guildID, roleID string) error
}

// jailManagedChannelTypes are the channel kinds jail's deny-by-default
// overwrite applies to. Categories are deliberately excluded here, not
// because it's unsafe to also touch them (see
// syncAllJailChannelOverwrites' separate category pass) but because a
// category overwrite doesn't do what it looks like it does: Discord's
// permission check for a channel only ever reads that channel's own
// permission_overwrites, never its parent category's, so setting an
// overwrite on a category has zero effect on any existing channel under
// it, jail-managed or not. The one thing a category overwrite is
// documented to do is get copied onto a channel *at creation time* if
// that channel is created with no overwrites of its own, which is a
// real but narrow benefit, not a substitute for the per-channel
// overwrites this function still has to write for every existing
// channel to actually restrict anything. Threads aren't a channel type
// Discord accepts permission overwrites on at all; they inherit their
// parent channel's visibility.
func jailManagedChannelType(t discord.ChannelType) bool {
	switch t {
	case discord.ChannelTypeGuildText, discord.ChannelTypeGuildVoice, discord.ChannelTypeGuildNews,
		discord.ChannelTypeGuildStageVoice, discord.ChannelTypeGuildForum, discord.ChannelTypeGuildMedia:
		return true
	default:
		return false
	}
}

// jailDenyFor is what the Jailed role is denied on a non-allowlisted
// channel: View Channel covers text visibility everywhere, plus Connect on
// voice/stage channels so a jailed member can't even see who's present in a
// voice channel they can't view.
func jailDenyFor(t discord.ChannelType) discord.Permissions {
	deny := discord.PermissionViewChannel
	if t == discord.ChannelTypeGuildVoice || t == discord.ChannelTypeGuildStageVoice {
		deny |= discord.PermissionConnect
	}
	return deny
}

// restrictedBits is what a marker role is denied on the channels it *can*
// see: the ways out of a room that is supposed to be the whole server for
// a while. Threads are a second room inside the first, a poll or an event
// is a broadcast, an invite or an external app reaches past the guild.
// Plain talk stays: send, react, attach, and in a voice room connect and
// speak, since the beach and the appeal room are both meant to be talked
// in. Denied on both the jail marker and the island's role, and merged
// onto an allowlisted channel's overwrite without touching any other bit.
const restrictedBits = discord.Permissions(discord.PermissionCreatePublicThreads |
	discord.PermissionCreatePrivateThreads |
	discord.PermissionSendMessagesInThreads |
	discord.PermissionSendPolls |
	discord.PermissionCreateEvents |
	discord.PermissionCreateInstantInvite |
	discord.PermissionUseExternalApps |
	discord.PermissionUseEmbeddedActivities |
	discord.PermissionMentionEveryone)

// jailOverwriteFor is the single source of truth for what a marker role
// must carry on a channel with no overwrite of its own yet: the (allow,
// deny) pair for a channel of type t, given whether it is on the role's
// visibility allowlist. Both the single-channel path and the full resync
// compute it here (through desiredOverwrite), so the two can no longer
// drift apart the way two copies of the same switch eventually do.
//
// Allowlisted channels get an explicit view + send (or view + connect, in
// voice) allow, so a jailed member can read and type in the appeal room,
// with restrictedBits denied; a category gets voice's deny bits, the
// superset any child type could need, since a category overwrite only ever
// matters as the template a newly-created channel inherits (see
// jailManagedChannelType).
func jailOverwriteFor(t discord.ChannelType, allowed bool) (allow, deny discord.Permissions) {
	switch {
	case t == discord.ChannelTypeGuildCategory:
		return 0, jailDenyFor(discord.ChannelTypeGuildVoice)
	case allowed && (t == discord.ChannelTypeGuildVoice || t == discord.ChannelTypeGuildStageVoice):
		return discord.PermissionViewChannel | discord.PermissionConnect, restrictedBits
	case allowed:
		return discord.PermissionViewChannel | discord.PermissionSendMessages, restrictedBits
	default:
		return 0, jailDenyFor(t)
	}
}

// memberBaseline is what an ordinary member can do in ch: Discord's own
// resolution for somebody holding @everyone plus the guild's configured
// member role (settings, "melted" in the Melting Pot), role tier only.
// Guild permissions of both roles, Administrator meaning everything, then
// the channel's @everyone overwrite, then the member role's overwrite.
// With no member role configured it is @everyone alone. A role that no
// longer exists contributes nothing, which is the safe direction.
//
// This is the ceiling a marker role is held under on an allowlisted
// channel (desiredOverwrite): a jailed member holds nothing but @everyone
// and the marker, so the marker's allow is exactly where an escalation
// would come from, and "no more than a plain member here" is the only rule
// that is right for an appeals room and an announcements room alike.
func memberBaseline(guildID, memberRoleID string, ch discord.GuildChannel, roles []discord.Role) discord.Permissions {
	var base discord.Permissions
	for _, r := range roles {
		if r.ID.String() == guildID || (memberRoleID != "" && r.ID.String() == memberRoleID) {
			base |= r.Permissions
		}
	}
	if base&discord.PermissionAdministrator != 0 {
		return discord.PermissionsAll
	}
	if ow := findOverwrite(ch, guildID, discord.PermissionOverwriteTypeRole); ow != nil {
		base = base&^ow.Deny | ow.Allow
	}
	if memberRoleID != "" {
		if ow := findOverwrite(ch, memberRoleID, discord.PermissionOverwriteTypeRole); ow != nil {
			base = base&^ow.Deny | ow.Allow
		}
	}
	return base
}

// permissionNames renders the bits a marker can be refused for the
// warning an admin reads. Only the bits jailOverwriteFor ever grants.
var permissionNames = []struct {
	bit  discord.Permissions
	name string
}{
	{discord.PermissionViewChannel, "View Channel"},
	{discord.PermissionSendMessages, "Send Messages"},
	{discord.PermissionConnect, "Connect"},
}

func namePermissions(bits discord.Permissions) string {
	var out []string
	for _, p := range permissionNames {
		if bits&p.bit != 0 {
			out = append(out, p.name)
		}
	}
	return strings.Join(out, ", ")
}

// desiredOverwrite is what roleID's overwrite on ch should be, given
// whether ch is on that role's allowlist and what an ordinary member can do
// there (baseline), whether a write is needed to get there, and which of
// the bits jail would normally grant were withheld because a plain member
// lacks them.
//
// The withheld bits go into the marker's own deny, not merely out of its
// allow. A jailed member no longer holds the member role, so a room locked
// by a deny on that role alone would still leave them @everyone's Send;
// the marker's deny is the one overwrite that is guaranteed to apply to
// them. The same rule strips a grant an earlier sync already wrote, which
// is how an allowlisted announcements room gets repaired rather than
// preserved. A channel with no overwrite for the role gets exactly
// jailOverwriteFor's pair. A channel that already has one is *merged*, not
// replaced: only the visibility bits (jailDenyFor: View, plus Connect on
// voice) are moved between allow and deny, restrictedBits are denied where
// the channel is allowlisted, and every other bit the guild set on that
// overwrite stays as it was. Two reasons, both learned from
// the island. A channel the guild built by hand (the Melting Pot's beach,
// with its own idea of who may speak, attach or react there) must keep
// working the way it was set up, and a marker role's job is only to decide
// whether the member can see it. And every permission write lands in the
// guild's own Discord audit log, so rewriting an overwrite to a value that
// differs only in bits this plugin has no opinion about buries a
// moderator's real entries under one line per channel per sync.
func desiredOverwrite(ch discord.GuildChannel, roleID string, allowed bool, baseline discord.Permissions) (allow, deny, withheld discord.Permissions, needed bool) {
	wantAllow, wantDeny := jailOverwriteFor(ch.Type(), allowed)
	if allowed && ch.Type() != discord.ChannelTypeGuildCategory {
		withheld = wantAllow &^ baseline
		if withheld&discord.PermissionViewChannel != 0 {
			// A room a plain member cannot see grants the marker nothing at
			// all; Send behind a View deny is not a permission, only a
			// confusing overwrite.
			withheld = wantAllow
		}
		wantAllow &^= withheld
		wantDeny |= withheld
	}
	existing := findOverwrite(ch, roleID, discord.PermissionOverwriteTypeRole)
	if existing == nil {
		return wantAllow, wantDeny, withheld, true
	}
	if allowed {
		// The room they may use: what jail has always granted there (view
		// and send, or view and connect) goes on, restrictedBits and the
		// withheld bits go off, and the rest of the overwrite is the guild's.
		allow, deny = (existing.Allow|wantAllow)&^wantDeny, (existing.Deny&^wantAllow)|wantDeny
	} else {
		// A room they may not see: only the visibility bits move.
		allow, deny = existing.Allow&^wantDeny, existing.Deny|wantDeny
	}
	return allow, deny, withheld, allow != existing.Allow || deny != existing.Deny
}

// markerOverwrite is desiredOverwrite plus the one question it cannot ask
// without the guild: does a room the marker may not see need a deny at all?
// Not when the channel's own @everyone overwrite already denies those bits,
// which is how a gated guild (the Melting Pot: @everyone sees nothing,
// melted sees the server) is built. A jailed member holds @everyone and the
// marker, never the member role (jail strips it and the join-role script
// will not hand it back), so @everyone's deny already hides the room and a
// marker deny on top is one more overwrite on every channel saying nothing.
// Where one is already there in exactly the shape merlin writes, it is
// removed (allow and deny both zero, which applyOverwrite reads as delete);
// one the guild changed is left as it is.
//
// Only a channel-level @everyone deny counts. A guild whose @everyone simply
// lacks View at guild level keeps the deny, since a role a jailed member
// keeps (one above merlin, a booster role) could carry View guild wide and a
// channel-level deny is what still stops it.
func markerOverwrite(guildID string, ch discord.GuildChannel, roleID string, allowed bool, baseline discord.Permissions) (allow, deny, withheld discord.Permissions, needed bool) {
	if !allowed {
		_, wantDeny := jailOverwriteFor(ch.Type(), false)
		if ow := findOverwrite(ch, guildID, discord.PermissionOverwriteTypeRole); ow != nil && ow.Deny&wantDeny == wantDeny {
			existing := findOverwrite(ch, roleID, discord.PermissionOverwriteTypeRole)
			visibility := jailDenyFor(discord.ChannelTypeGuildVoice)
			mine := existing != nil && existing.Allow == 0 && existing.Deny != 0 && existing.Deny&^visibility == 0
			return 0, 0, 0, mine
		}
	}
	return desiredOverwrite(ch, roleID, allowed, baseline)
}

// visibilityMarker is one role whose channel visibility this plugin
// manages deny-by-default against an allowlist: the jail marker, or the
// vacation script's island role. Same sync, different list.
type visibilityMarker struct {
	name    string
	roleID  string
	allowed map[string]bool
}

func (p *Plugin) jailMarker(guildID, roleID string) visibilityMarker {
	return visibilityMarker{name: "jail", roleID: roleID, allowed: idSet(p.jailChannelConfig.JailAllowedChannelIDs(guildID))}
}

func (p *Plugin) vacationMarker(guildID, roleID string) visibilityMarker {
	return visibilityMarker{name: "vacation", roleID: roleID, allowed: idSet(p.vacationAllowlist(guildID))}
}

func idSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// findOverwrite returns ch's overwrite for one target, or nil when it has
// none.
func findOverwrite(ch discord.GuildChannel, targetID string, kind discord.PermissionOverwriteType) *core.Overwrite {
	for _, ow := range core.Overwrites(ch.PermissionOverwrites()) {
		if ow.Type == kind && ow.ID.String() == targetID {
			return &ow
		}
	}
	return nil
}

// syncJailChannelOverwrite sets or updates channelID's permission overwrite
// for the Jailed role to match whether it's currently in guildID's
// allowlist, called after a single allow-channel/disallow-channel
// configuration change, so a config edit costs exactly one Discord API
// call, never a full-guild resync.
//
// For allowlisted channels we explicitly grant ViewChannel and a limited
// SendMessages permission while denying AttachFiles and EmbedLinks so a
// jailed member can read history and type but cannot post images or embeds.
func (p *Plugin) syncJailChannelOverwrite(guildID, jailRoleID, channelID string) (withheld discord.Permissions, err error) {
	return p.syncChannelVisibility(guildID, p.jailMarker(guildID, jailRoleID), channelID)
}

// syncChannelVisibility is syncJailChannelOverwrite for either marker. It
// reports the bits withheld from the marker on an allowlisted channel
// (memberBaseline), for the handler to warn about.
func (p *Plugin) syncChannelVisibility(guildID string, m visibilityMarker, channelID string) (withheld discord.Permissions, err error) {
	ch, err := guildChannel(p.ops(guildID), core.ID(channelID))
	if err != nil {
		return 0, fmt.Errorf("roles: fetch channel %s: %w", channelID, err)
	}
	if !jailManagedChannelType(ch.Type()) {
		return 0, nil
	}
	roles, err := p.ops(guildID).GetRoles(core.ID(guildID))
	if err != nil {
		return 0, fmt.Errorf("roles: list guild roles: %w", err)
	}
	baseline := memberBaseline(guildID, p.jailChannelConfig.MemberRoleID(guildID), ch, roles)
	allowBits, denyBits, withheld, needed := markerOverwrite(guildID, ch, m.roleID, m.allowed[channelID], baseline)
	if !needed {
		return withheld, nil
	}
	if err := applyOverwrite(p.ops(guildID), ch, m.roleID, allowBits, denyBits); err != nil {
		return withheld, fmt.Errorf("roles: set %s overwrite on %s: %w", m.name, channelID, err)
	}
	return withheld, nil
}

// applyOverwrite writes what markerOverwrite asked for: the pair, or, when
// it asked for nothing at all, the removal of the marker's overwrite.
func applyOverwrite(ops DiscordMemberOps, ch discord.GuildChannel, roleID string, allow, deny discord.Permissions) error {
	if allow == 0 && deny == 0 {
		return ops.DeletePermissionOverwrite(ch.ID(), core.ID(roleID))
	}
	return setOverwrite(ops, ch.ID(), core.ID(roleID), discord.PermissionOverwriteTypeRole, allow, deny)
}

// jailMemberShape reports whether ow is the member-level deny older
// versions of merlin wrote for a jailed member on ch: no allow, and exactly
// jailDenyFor's bits. That hardening existed because a guild's Onboarding
// flow could hand a jailed member back a role whose channel allow beat the
// marker's deny. Onboarding is gone, merlin hands out the member role itself
// (script_joinrole.go, which yields to jail), and HandleMemberUpdate strips
// anything regranted, so nothing writes these any more. What is left is
// cleaning up the ones still out there.
func jailMemberShape(ch discord.GuildChannel, ow core.Overwrite) bool {
	return ow.Type == discord.PermissionOverwriteTypeMember && ow.Allow == 0 && ow.Deny == jailDenyFor(ch.Type())
}

// clearMemberJailOverwrites removes userID's leftover member-level jail deny
// from every jail-managed channel, on release and on a transfer out of jail.
// Every channel, not the ones that look at risk today: the old version
// recomputed "at risk" at release time, so a channel whose conflicting role
// allow (or allowlist entry) changed while the member was jailed kept its
// member deny forever, which is how released members stayed locked out.
// Finding none costs the one channel list read and nothing else.
func (p *Plugin) clearMemberJailOverwrites(guildID, userID string) error {
	channels, err := p.ops(guildID).GetGuildChannels(core.ID(guildID))
	if err != nil {
		return fmt.Errorf("roles: list guild channels: %w", err)
	}
	var targets []discord.GuildChannel
	for _, ch := range channels {
		if ow := findOverwrite(ch, userID, discord.PermissionOverwriteTypeMember); ow != nil && jailManagedChannelType(ch.Type()) && jailMemberShape(ch, *ow) {
			targets = append(targets, ch)
		}
	}
	return p.forEachChannelConcurrent(targets, func(ch discord.GuildChannel) error {
		return p.ops(guildID).DeletePermissionOverwrite(ch.ID(), core.ID(userID))
	}, func(ch discord.GuildChannel, err error) {
		p.log.Error("roles: clear member jail overwrite failed", "guild", guildID, "user", userID, "channel", ch.ID(), "err", err)
	})
}

// auditLogPages bounds how far back clearLeftoverMemberOverwrites reads
// Discord's audit log, 100 entries a page. Discord keeps 45 days anyway.
const auditLogPages = 20

// clearLeftoverMemberOverwrites removes the member-level jail denies still
// sitting on members who are no longer jailed: the ones a release before
// this version left behind. Run from Sync every channel.
//
// The shape alone is not proof: a mod hiding one text channel from one
// member by hand writes exactly the same overwrite. So a candidate goes only
// if Discord's own audit log says merlin created it, and when that log
// cannot be read nothing goes. No candidates costs no audit log read at all,
// which is the steady state once the old ones are gone. Returns how many
// were removed.
func (p *Plugin) clearLeftoverMemberOverwrites(ctx context.Context, guildID string) (int, error) {
	ops := p.ops(guildID)
	channels, err := ops.GetGuildChannels(core.ID(guildID))
	if err != nil {
		return 0, fmt.Errorf("roles: list guild channels: %w", err)
	}
	type spot struct{ channel, user string }
	var candidates []spot
	for _, ch := range channels {
		if !jailManagedChannelType(ch.Type()) {
			continue
		}
		for _, ow := range core.Overwrites(ch.PermissionOverwrites()) {
			if jailMemberShape(ch, ow) {
				candidates = append(candidates, spot{ch.ID().String(), ow.ID.String()})
			}
		}
	}
	if len(candidates) == 0 || p.botID == 0 {
		return 0, nil
	}
	ours := make(map[spot]bool)
	var before snowflake.ID
	for range auditLogPages {
		page, err := ops.GetAuditLog(core.ID(guildID), p.botID, discord.AuditLogEventChannelOverwriteCreate, before, 0, 100)
		if err != nil {
			return 0, fmt.Errorf("roles: read audit log: %w", err)
		}
		for _, e := range page.AuditLogEntries {
			if e.TargetID != nil && e.Options != nil && e.Options.ID != nil {
				ours[spot{e.TargetID.String(), *e.Options.ID}] = true
			}
			before = e.ID
		}
		if len(page.AuditLogEntries) < 100 {
			break
		}
	}
	removed := 0
	for _, c := range candidates {
		if !ours[c] {
			continue
		}
		if _, jailed, err := p.store.GetJail(ctx, guildID, c.user); err != nil || jailed {
			continue // still jailed, or unknown: release clears it
		}
		if err := ops.DeletePermissionOverwrite(core.ID(c.channel), core.ID(c.user)); err != nil {
			p.log.Error("roles: clear leftover member jail overwrite failed", "guild", guildID, "user", c.user, "channel", c.channel, "err", err)
			continue
		}
		removed++
	}
	return removed, nil
}

// syncAllJailChannelOverwrites recomputes every managed channel's Jailed-role
// overwrite in guildID against the current allowlist and channel list, plus
// a plain deny on every category (see below). Run once when the Jailed role
// is first created (so a fresh setup starts deny-by-default everywhere) and
// again on demand via /roles configure sync-channels (e.g. after creating
// new channels, which don't automatically inherit a deny overwrite; this
// plugin has no gateway listener for channel creation, by design: keeping
// this to an explicit, mod-triggered action avoids a second event-handling
// surface to reason about for a case a mod can just re-run after setting up
// new channels). Writes fan out across a bounded worker pool
// (channelSyncConcurrency): a guild's channel count can run into the
// hundreds, and Discord gives each channel its own rate-limit bucket for
// permission writes, so there's no reason to pay for them one at a time.
// One channel's failure is logged and doesn't abort the rest, matching
// rotation.sweep's policy.
func (p *Plugin) syncAllJailChannelOverwrites(guildID, jailRoleID string) error {
	return p.syncAllVisibility(guildID, p.jailMarker(guildID, jailRoleID))
}

// syncAllVisibility is syncAllJailChannelOverwrites for either marker.
func (p *Plugin) syncAllVisibility(guildID string, m visibilityMarker) error {
	channels, err := p.ops(guildID).GetGuildChannels(core.ID(guildID))
	if err != nil {
		return fmt.Errorf("roles: list guild channels: %w", err)
	}
	roles, err := p.ops(guildID).GetRoles(core.ID(guildID))
	if err != nil {
		return fmt.Errorf("roles: list guild roles: %w", err)
	}
	memberRole := p.jailChannelConfig.MemberRoleID(guildID)

	var targets []discord.GuildChannel
	for _, ch := range channels {
		if jailManagedChannelType(ch.Type()) || ch.Type() == discord.ChannelTypeGuildCategory {
			targets = append(targets, ch)
		}
	}

	return p.forEachChannelConcurrent(targets, func(ch discord.GuildChannel) error {
		// Categories are never permission-checked directly (see
		// jailManagedChannelType's doc comment); their overwrite only ever
		// matters as the template Discord copies onto a channel created
		// under them with no overwrites of its own, so a channel created
		// after this sync starts denied rather than visible until someone
		// re-runs sync-channels.
		baseline := memberBaseline(guildID, memberRole, ch, roles)
		allowBits, denyBits, withheld, needed := markerOverwrite(guildID, ch, m.roleID, m.allowed[ch.ID().String()], baseline)
		if withheld != 0 {
			p.log.Info("roles: allowlisted channel grants less than jail would", "guild", guildID, "marker", m.name,
				"channel", ch.ID(), "withheld", namePermissions(withheld))
		}
		if !needed {
			return nil
		}
		return applyOverwrite(p.ops(guildID), ch, m.roleID, allowBits, denyBits)
	}, func(ch discord.GuildChannel, err error) {
		p.log.Error("roles: sync overwrite failed", "guild", guildID, "marker", m.name, "channel", ch.ID(), "err", err)
	})
}
