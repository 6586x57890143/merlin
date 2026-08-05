package roles

import (
	"context"
	"fmt"

	"github.com/bwmarrin/discordgo"
)

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
	ClearJailMarkerRole(ctx context.Context, guildID string) error
}

// jailManagedChannelTypes are the channel kinds jail's deny-by-default
// overwrite applies to. Categories are deliberately excluded: setting an
// overwrite on a category cascades to any child channel that doesn't have
// its own overwrite, which would fight per-channel overwrites in
// unpredictable ways depending on child/category ordering. Explicit,
// per-channel overwrites only, no cascade surprises. Threads aren't a
// channel type Discord accepts permission overwrites on at all; they
// inherit their parent channel's visibility.
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

	allowed := false
	for _, id := range p.jailChannelConfig.JailAllowedChannelIDs(guildID) {
		if id == channelID {
			allowed = true
			break
		}
	}
	if allowed {
		// Allowlisted: explicitly allow view and a minimal send/connect
		// permission on the jailed role, and deny AttachFiles/EmbedLinks in
		// text channels so jailed members can't post images/embeds into
		// channels they remain allowed to read.
		var allowBits int64 = int64(discordgo.PermissionViewChannel)
		var denyBits int64 = 0
		if ch.Type == discordgo.ChannelTypeGuildVoice || ch.Type == discordgo.ChannelTypeGuildStageVoice {
			allowBits |= int64(discordgo.PermissionVoiceConnect)
		} else {
			allowBits |= int64(discordgo.PermissionSendMessages)
			denyBits = int64(discordgo.PermissionAttachFiles | discordgo.PermissionEmbedLinks)
		}
		if err := p.ops(guildID).ChannelPermissionSet(channelID, jailRoleID, discordgo.PermissionOverwriteTypeRole, allowBits, denyBits); err != nil {
			return fmt.Errorf("roles: set jail allow overwrite on %s: %w", channelID, err)
		}
		return nil
	}

	// Not allowlisted: deny view (and connect for voice) explicitly.
	if err := p.ops(guildID).ChannelPermissionSet(channelID, jailRoleID, discordgo.PermissionOverwriteTypeRole, 0, jailDenyFor(ch.Type)); err != nil {
		return fmt.Errorf("roles: set jail overwrite on %s: %w", channelID, err)
	}
	return nil
}

// syncAllJailChannelOverwrites recomputes every managed channel's Jailed-role
// overwrite in guildID against the current allowlist and channel list.
// Run once when the Jailed role is first created (so a fresh setup starts
// deny-by-default everywhere) and again on demand via /roles configure
// sync-channels (e.g. after creating new channels, which don't
// automatically inherit a deny overwrite; this plugin has no gateway
// listener for channel creation, by design: keeping this to an explicit,
// mod-triggered action avoids a second event-handling surface to reason
// about for a case a mod can just re-run after setting up new channels).
// One row's failure is logged and doesn't abort the rest, matching
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

	var firstErr error
	for _, ch := range channels {
		if !jailManagedChannelType(ch.Type) {
			continue
		}
		var err error
		if allowed[ch.ID] {
			// Explicit allow for allowlisted channels: view + send/connect,
			// plus deny AttachFiles/EmbedLinks for text channels.
			var allowBits int64 = int64(discordgo.PermissionViewChannel)
			var denyBits int64 = 0
			if ch.Type == discordgo.ChannelTypeGuildVoice || ch.Type == discordgo.ChannelTypeGuildStageVoice {
				allowBits |= int64(discordgo.PermissionVoiceConnect)
			} else {
				allowBits |= int64(discordgo.PermissionSendMessages)
				denyBits = int64(discordgo.PermissionAttachFiles | discordgo.PermissionEmbedLinks)
			}
			err = p.ops(guildID).ChannelPermissionSet(ch.ID, jailRoleID, discordgo.PermissionOverwriteTypeRole, allowBits, denyBits)
		} else {
			err = p.ops(guildID).ChannelPermissionSet(ch.ID, jailRoleID, discordgo.PermissionOverwriteTypeRole, 0, jailDenyFor(ch.Type))
		}
		if err != nil {
			p.log.Error("roles: sync jail overwrite failed", "guild", guildID, "channel", ch.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
