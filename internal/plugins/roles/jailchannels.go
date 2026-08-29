package roles

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// channelSyncConcurrency bounds how many permission-overwrite writes run at
// once during a guild-wide resync (syncAllJailChannelOverwrites,
// syncMemberJailOverwrites, clearMemberJailOverwrites). Each channel has its
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
func (p *Plugin) forEachChannelConcurrent(channels []*discordgo.Channel, fn func(*discordgo.Channel) error, onErr func(*discordgo.Channel, error)) error {
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
func jailManagedChannelType(t discordgo.ChannelType) bool {
	switch t {
	case discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildVoice, discordgo.ChannelTypeGuildNews,
		discordgo.ChannelTypeGuildStageVoice, discordgo.ChannelTypeGuildForum, discordgo.ChannelTypeGuildMedia:
		return true
	default:
		return false
	}
}

// jailDenyFor is what the Jailed role is denied on a non-allowlisted
// channel: View Channel covers text visibility everywhere, plus Connect on
// voice/stage channels so a jailed member can't even see who's present in a
// voice channel they can't view.
func jailDenyFor(t discordgo.ChannelType) int64 {
	deny := int64(discordgo.PermissionViewChannel)
	if t == discordgo.ChannelTypeGuildVoice || t == discordgo.ChannelTypeGuildStageVoice {
		deny |= discordgo.PermissionVoiceConnect
	}
	return deny
}

// jailOverwriteFor is the single source of truth for what the Jailed role
// must carry on one channel: the (allow, deny) pair for a channel of type t,
// given whether it is on the guild's visibility allowlist. Both the
// single-channel path and the full resync compute it here, so the two can no
// longer drift apart the way two copies of the same switch eventually do.
//
// Allowlisted channels get an explicit view + send (or view + connect, in
// voice) allow, so a jailed member can read and type in the appeal room; a
// category gets voice's deny bits, the superset any child type could need,
// since a category overwrite only ever matters as the template a
// newly-created channel inherits (see jailManagedChannelType).
func jailOverwriteFor(t discordgo.ChannelType, allowed bool) (allow, deny int64) {
	switch {
	case t == discordgo.ChannelTypeGuildCategory:
		return 0, jailDenyFor(discordgo.ChannelTypeGuildVoice)
	case allowed && (t == discordgo.ChannelTypeGuildVoice || t == discordgo.ChannelTypeGuildStageVoice):
		return int64(discordgo.PermissionViewChannel | discordgo.PermissionVoiceConnect), 0
	case allowed:
		return int64(discordgo.PermissionViewChannel | discordgo.PermissionSendMessages), 0
	default:
		return 0, jailDenyFor(t)
	}
}

// overwriteMatches reports whether ch already carries exactly the overwrite
// we were about to write. Every permission write lands in the guild's own
// Discord audit log, so a resync of a large guild that changed nothing still
// buried a moderator's real entries under one line per channel, and
// re-running /roles configure sync-channels did it again. The channel list
// these paths already fetch carries the live overwrites, so skipping the
// no-op writes costs no extra API call and nothing in correctness: a write
// that sets what is already set is not a write worth making.
func overwriteMatches(ch *discordgo.Channel, targetID string, kind discordgo.PermissionOverwriteType, allow, deny int64) bool {
	ow := findOverwrite(ch, targetID, kind)
	return ow != nil && ow.Allow == allow && ow.Deny == deny
}

// findOverwrite returns ch's overwrite for one target, or nil when it has
// none. Nil is the answer clearMemberJailOverwrites needs: deleting an
// overwrite that is not there is a wasted call that still writes an audit
// entry.
func findOverwrite(ch *discordgo.Channel, targetID string, kind discordgo.PermissionOverwriteType) *discordgo.PermissionOverwrite {
	for _, ow := range ch.PermissionOverwrites {
		if ow.Type == kind && ow.ID == targetID {
			return ow
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
func (p *Plugin) syncJailChannelOverwrite(guildID, jailRoleID, channelID string) error {
	ch, err := p.ops(guildID).Channel(channelID)
	if err != nil {
		return fmt.Errorf("roles: fetch channel %s: %w", channelID, err)
	}
	if !jailManagedChannelType(ch.Type) {
		return nil
	}

	allowed := slices.Contains(p.jailChannelConfig.JailAllowedChannelIDs(guildID), channelID)
	allowBits, denyBits := jailOverwriteFor(ch.Type, allowed)
	if overwriteMatches(ch, jailRoleID, discordgo.PermissionOverwriteTypeRole, allowBits, denyBits) {
		return nil
	}
	if err := p.ops(guildID).ChannelPermissionSet(channelID, jailRoleID, discordgo.PermissionOverwriteTypeRole, allowBits, denyBits); err != nil {
		return fmt.Errorf("roles: set jail overwrite on %s: %w", channelID, err)
	}
	return nil
}

// channelHasConflictingRoleAllow reports whether some role other than
// @everyone has an explicit channel-level overwrite allowing a permission
// deny is supposed to remove. Discord resolves conflicting role-tier
// overwrites by combining every held role's deny bits, then every held
// role's allow bits, applying deny then allow: an allow from any other role
// beats the Jailed role's deny on the same channel, regardless of role
// position ("permissions do not obey the role hierarchy" for this
// resolution, per Discord's own docs). A member-tier overwrite is applied
// after all role-tier overwrites and cannot be beaten by any role, which is
// what syncMemberJailOverwrites adds, and only where it's actually needed:
// a channel with no conflicting role overwrite is already correctly denied
// by the Jailed role's own overwrite alone.
//
// everyoneRoleID is guildID itself: Discord gives the @everyone role the
// same ID as the guild. Not scoped to one specific "access role" by design:
// this catches any role with a conflicting allow, present now or granted
// later by anything (a guild's Onboarding flow, a manual grant, a future
// misconfiguration), not just whichever role happens to be causing trouble
// today.
func channelHasConflictingRoleAllow(ch *discordgo.Channel, everyoneRoleID string, deny int64) bool {
	for _, ow := range ch.PermissionOverwrites {
		if ow.Type != discordgo.PermissionOverwriteTypeRole || ow.ID == everyoneRoleID {
			continue
		}
		if ow.Allow&deny != 0 {
			return true
		}
	}
	return false
}

// atRiskJailChannels returns guildID's jail-managed, non-allowlisted
// channels that actually have a conflicting role-level allow overwrite (see
// channelHasConflictingRoleAllow). In practice this is a small subset of a
// guild's channels, since most channels carry no per-role overwrites at
// all, which is what keeps syncMemberJailOverwrites cheap: it only ever
// writes to channels where a member-tier overwrite is the sole thing that
// can actually stop a competing role's allow from beating the Jailed role's
// deny.
func (p *Plugin) atRiskJailChannels(guildID string) ([]*discordgo.Channel, error) {
	channels, err := p.ops(guildID).GuildChannels(guildID)
	if err != nil {
		return nil, fmt.Errorf("roles: list guild channels: %w", err)
	}
	allowed := make(map[string]bool)
	for _, id := range p.jailChannelConfig.JailAllowedChannelIDs(guildID) {
		allowed[id] = true
	}

	var out []*discordgo.Channel
	for _, ch := range channels {
		if !jailManagedChannelType(ch.Type) || allowed[ch.ID] {
			continue
		}
		if channelHasConflictingRoleAllow(ch, guildID, jailDenyFor(ch.Type)) {
			out = append(out, ch)
		}
	}
	return out, nil
}

// syncMemberJailOverwrites adds a member-level deny for userID on every
// at-risk channel (atRiskJailChannels): the hardening that makes a jail
// unconditional regardless of what roles userID holds or is later granted,
// since a member-tier overwrite is the only thing in Discord's permission
// model that reliably wins over every role-tier overwrite at once. Called
// from stripToJailRoles, so it runs on every jail (re)application: the
// initial jail, a rejoin-evasion re-apply (Discord clears a member's own
// channel overwrites when they leave the guild, so a rejoin needs this
// reapplied too, not just the role), and a regrant reassertion. Per-channel
// failures are logged and don't abort the rest, matching
// syncAllJailChannelOverwrites' own policy; the caller treats this whole
// call as best-effort, since the role strip it runs alongside is what must
// not fail.
func (p *Plugin) syncMemberJailOverwrites(guildID, userID string) error {
	channels, err := p.atRiskJailChannels(guildID)
	if err != nil {
		return err
	}
	return p.forEachChannelConcurrent(channels, func(ch *discordgo.Channel) error {
		if overwriteMatches(ch, userID, discordgo.PermissionOverwriteTypeMember, 0, jailDenyFor(ch.Type)) {
			return nil
		}
		return p.ops(guildID).ChannelPermissionSet(ch.ID, userID, discordgo.PermissionOverwriteTypeMember, 0, jailDenyFor(ch.Type))
	}, func(ch *discordgo.Channel, err error) {
		p.log.Error("roles: set member jail overwrite failed", "guild", guildID, "user", userID, "channel", ch.ID, "err", err)
	})
}

// clearMemberJailOverwrites removes userID's member-level deny from every
// currently at-risk channel, called from releaseJail. Recomputing "at risk"
// fresh rather than remembering what was set at jail time means a channel
// whose conflicting role overwrite was removed while the member was jailed
// is simply skipped here too: it no longer needs cleanup, which keeps
// release scoped to the same small channel set jail itself touches instead
// of a blanket sweep of every jail-managed channel.
func (p *Plugin) clearMemberJailOverwrites(guildID, userID string) error {
	channels, err := p.atRiskJailChannels(guildID)
	if err != nil {
		return err
	}
	return p.forEachChannelConcurrent(channels, func(ch *discordgo.Channel) error {
		if findOverwrite(ch, userID, discordgo.PermissionOverwriteTypeMember) == nil {
			return nil
		}
		return p.ops(guildID).ChannelPermissionDelete(ch.ID, userID)
	}, func(ch *discordgo.Channel, err error) {
		p.log.Error("roles: clear member jail overwrite failed", "guild", guildID, "user", userID, "channel", ch.ID, "err", err)
	})
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
	channels, err := p.ops(guildID).GuildChannels(guildID)
	if err != nil {
		return fmt.Errorf("roles: list guild channels: %w", err)
	}
	allowed := make(map[string]bool)
	for _, id := range p.jailChannelConfig.JailAllowedChannelIDs(guildID) {
		allowed[id] = true
	}

	var targets []*discordgo.Channel
	for _, ch := range channels {
		if jailManagedChannelType(ch.Type) || ch.Type == discordgo.ChannelTypeGuildCategory {
			targets = append(targets, ch)
		}
	}

	return p.forEachChannelConcurrent(targets, func(ch *discordgo.Channel) error {
		// Categories are never permission-checked directly (see
		// jailManagedChannelType's doc comment); their overwrite only ever
		// matters as the template Discord copies onto a channel created
		// under them with no overwrites of its own, so a channel created
		// after this sync starts denied rather than visible until someone
		// re-runs sync-channels.
		allowBits, denyBits := jailOverwriteFor(ch.Type, allowed[ch.ID])
		if overwriteMatches(ch, jailRoleID, discordgo.PermissionOverwriteTypeRole, allowBits, denyBits) {
			return nil
		}
		return p.ops(guildID).ChannelPermissionSet(ch.ID, jailRoleID, discordgo.PermissionOverwriteTypeRole, allowBits, denyBits)
	}, func(ch *discordgo.Channel, err error) {
		p.log.Error("roles: sync jail overwrite failed", "guild", guildID, "channel", ch.ID, "err", err)
	})
}
