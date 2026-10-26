package roles

import (
	"context"
	"fmt"
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
	JailConversationChannelIDs(guildID string) []string
	AddJailConversationChannel(ctx context.Context, guildID, channelID string) error
	RemoveJailConversationChannel(ctx context.Context, guildID, channelID string) error
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
	// The supporter role merlin created here and where new supporters are
	// thanked (supporters.go). Empty means off and nowhere; "" clears.
	SupporterRoleID(guildID string) string
	SetSupporterRole(ctx context.Context, guildID, roleID string) error
	SupporterChannelID(guildID string) string
	SetSupporterChannel(ctx context.Context, guildID, channelID string) error
	ClearJailMarkerRole(ctx context.Context, guildID string) error
	// Vacation uses a configured marker; its access modes match jail's.
	VacationRoleID(guildID string) string
	SetVacationRole(ctx context.Context, guildID, roleID string) error
	// Legacy allowed-channel columns hold read-only channels.
	VacationAllowedChannelIDs(guildID string) []string
	AddVacationAllowedChannel(ctx context.Context, guildID, channelID string) error
	RemoveVacationAllowedChannel(ctx context.Context, guildID, channelID string) error
	VacationConversationChannelIDs(guildID string) []string
	AddVacationConversationChannel(ctx context.Context, guildID, channelID string) error
	RemoveVacationConversationChannel(ctx context.Context, guildID, channelID string) error
	// The ordinary member role used by the join-role script.
	MemberRoleID(guildID string) string
	SetMemberRole(ctx context.Context, guildID, roleID string) error
	// The extra roles the join-role script gives a new member besides
	// MemberRoleID (script_joinrole.go).
	JoinRoleIDs(guildID string) []string
	AddJoinRole(ctx context.Context, guildID, roleID string) error
	RemoveJoinRole(ctx context.Context, guildID, roleID string) error
}

// Categories are reconciled separately before these child channel types.
// Threads inherit their parent's permissions and cannot carry overwrites.
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

// Both visible modes prohibit threads, broadcasts, invites and external apps.
const restrictedBits = discord.Permissions(discord.PermissionCreatePublicThreads |
	discord.PermissionCreatePrivateThreads |
	discord.PermissionSendMessagesInThreads |
	discord.PermissionSendPolls |
	discord.PermissionCreateEvents |
	discord.PermissionCreateInstantInvite |
	discord.PermissionUseExternalApps |
	discord.PermissionUseEmbeddedActivities |
	discord.PermissionMentionEveryone)

// channelAccess is explicit administrator intent, independent of the roles
// stripped during jail. Existing allowlists migrate to read-only.
type channelAccess uint8

const (
	accessHidden channelAccess = iota
	accessReadOnly
	accessConversation
)

// Channel types with no ordinary top-level conversation stay read-only.
// Forum/media posting creates threads, which the confinement policy forbids.
func conversationChannel(t discord.ChannelType) bool {
	return t == discord.ChannelTypeGuildText || t == discord.ChannelTypeGuildNews ||
		t == discord.ChannelTypeGuildVoice
}

func jailOverwriteFor(t discord.ChannelType, access channelAccess) (allow, deny discord.Permissions) {
	if access == accessHidden || t == discord.ChannelTypeGuildCategory {
		deny = jailDenyFor(t)
		if t == discord.ChannelTypeGuildCategory {
			deny |= discord.PermissionConnect
		}
		return 0, deny
	}
	allow = discord.PermissionViewChannel | discord.PermissionReadMessageHistory
	deny = restrictedBits | discord.PermissionSendMessages | discord.PermissionAddReactions |
		discord.PermissionSendTTSMessages | discord.PermissionSendVoiceMessages |
		discord.PermissionConnect | discord.PermissionSpeak | discord.PermissionStream |
		discord.PermissionRequestToSpeak | discord.PermissionUseSoundboard | discord.PermissionUseExternalSounds
	if access == accessConversation && conversationChannel(t) {
		allow |= discord.PermissionSendMessages | discord.PermissionAddReactions
		if t == discord.ChannelTypeGuildVoice {
			allow |= discord.PermissionConnect | discord.PermissionSpeak | discord.PermissionUseVAD
		}
		deny &^= allow
	}
	return allow, deny
}

// desiredOverwrite owns only the access-mode bits and restrictedBits. It
// repairs stale managed allows/denies while preserving unrelated permissions.
func desiredOverwrite(ch discord.GuildChannel, roleID string, access channelAccess) (allow, deny discord.Permissions, needed bool) {
	allow, deny = jailOverwriteFor(ch.Type(), access)
	managed := restrictedBits | discord.PermissionViewChannel | discord.PermissionReadMessageHistory |
		discord.PermissionSendMessages | discord.PermissionAddReactions |
		discord.PermissionSendTTSMessages | discord.PermissionSendVoiceMessages |
		discord.PermissionConnect | discord.PermissionSpeak | discord.PermissionStream |
		discord.PermissionRequestToSpeak | discord.PermissionUseVAD |
		discord.PermissionUseSoundboard | discord.PermissionUseExternalSounds
	existing := findOverwrite(ch, roleID, discord.PermissionOverwriteTypeRole)
	if existing == nil {
		return allow, deny, true
	}
	allow |= existing.Allow &^ managed
	deny |= existing.Deny &^ managed
	return allow, deny, allow != existing.Allow || deny != existing.Deny
}

// markerOverwrite retains the no-op optimization only when no marker allow
// could override @everyone. Explicit allows must be revoked on removal.
func markerOverwrite(guildID string, ch discord.GuildChannel, roleID string, access channelAccess) (allow, deny discord.Permissions, needed bool) {
	if access == accessHidden && ch.Type() != discord.ChannelTypeGuildCategory {
		wantDeny := jailDenyFor(ch.Type())
		if ow := findOverwrite(ch, guildID, discord.PermissionOverwriteTypeRole); ow != nil && ow.Deny&wantDeny == wantDeny {
			existing := findOverwrite(ch, roleID, discord.PermissionOverwriteTypeRole)
			if existing == nil {
				return 0, 0, false
			}
			visibility := discord.PermissionViewChannel | discord.PermissionConnect
			if existing.Allow == 0 && existing.Deny != 0 && existing.Deny&^visibility == 0 {
				return 0, 0, true
			}
			if existing.Allow&visibility == 0 {
				return existing.Allow, existing.Deny, false
			}
		}
	}
	return desiredOverwrite(ch, roleID, access)
}

// visibilityMarker is one role whose channel visibility this plugin
// manages deny-by-default against an allowlist: the jail marker, or the
// vacation script's island role. Same sync, different list.
type visibilityMarker struct {
	name         string
	roleID       string
	allowed      map[string]bool
	conversation map[string]bool
}

func (p *Plugin) jailMarker(guildID, roleID string) visibilityMarker {
	return visibilityMarker{name: "jail", roleID: roleID, allowed: idSet(p.jailChannelConfig.JailAllowedChannelIDs(guildID)), conversation: idSet(p.jailChannelConfig.JailConversationChannelIDs(guildID))}
}

func (p *Plugin) vacationMarker(guildID, roleID string) visibilityMarker {
	return visibilityMarker{name: "vacation", roleID: roleID, allowed: idSet(p.vacationAllowlist(guildID)), conversation: idSet(p.jailChannelConfig.VacationConversationChannelIDs(guildID))}
}

func (m visibilityMarker) access(channelID string) channelAccess {
	if m.conversation[channelID] {
		return accessConversation
	}
	if m.allowed[channelID] {
		return accessReadOnly
	}
	return accessHidden
}

// A settings edit can arrive while this sync waits for the guild lock.
// Read mode lists after acquiring it so queued work cannot restore stale access.
func (p *Plugin) currentMarker(guildID string, m visibilityMarker) visibilityMarker {
	if m.name == "vacation" {
		return p.vacationMarker(guildID, m.roleID)
	}
	return p.jailMarker(guildID, m.roleID)
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

// A settings edit reconciles the selected channel immediately.
func (p *Plugin) syncJailChannelOverwrite(guildID, jailRoleID, channelID string) (withheld discord.Permissions, err error) {
	return p.syncChannelVisibility(guildID, p.jailMarker(guildID, jailRoleID), channelID)
}

// The permission result is retained for callers; explicit modes withhold no bits.
func (p *Plugin) syncChannelVisibility(guildID string, m visibilityMarker, channelID string) (withheld discord.Permissions, err error) {
	if p.dryRun(guildID) {
		return 0, fmt.Errorf("channel sync pending while dry-run is enabled")
	}
	unlock := p.lockVisibility(guildID)
	defer unlock()
	m = p.currentMarker(guildID, m)
	ch, err := guildChannel(p.ops(guildID), core.ID(channelID))
	if err != nil {
		return 0, fmt.Errorf("roles: fetch channel %s: %w", channelID, err)
	}
	if !jailManagedChannelType(ch.Type()) {
		return 0, nil
	}
	allowBits, denyBits, needed := markerOverwrite(guildID, ch, m.roleID, m.access(channelID))
	if !needed {
		return withheld, nil
	}
	if err := applyOverwrite(p.ops(guildID), ch, m.roleID, allowBits, denyBits); err != nil {
		p.markVisibilityDirty(guildID)
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
	_, err := p.clearOwnedMemberOverwrites(context.Background(), guildID, userID)
	return err
}

const auditLogPages = 20

func (p *Plugin) clearLeftoverMemberOverwrites(ctx context.Context, guildID string) (int, error) {
	return p.clearOwnedMemberOverwrites(ctx, guildID, "")
}

// Bit shape alone cannot distinguish an old jail deny from a moderator's
// restriction. Require the latest overwrite event to belong to this bot,
// including edits/deletes by other actors, and preserve anything unproven.
func (p *Plugin) clearOwnedMemberOverwrites(ctx context.Context, guildID, userID string) (int, error) {
	ops := p.ops(guildID)
	channels, err := ops.GetGuildChannels(core.ID(guildID))
	if err != nil {
		return 0, err
	}
	type spot struct{ channel, user string }
	candidates := make(map[spot]bool)
	for _, ch := range channels {
		if !jailManagedChannelType(ch.Type()) {
			continue
		}
		for _, ow := range core.Overwrites(ch.PermissionOverwrites()) {
			if jailMemberShape(ch, ow) && (userID == "" || ow.ID.String() == userID) {
				candidates[spot{ch.ID().String(), ow.ID.String()}] = true
			}
		}
	}
	if len(candidates) == 0 || p.botID == 0 {
		return 0, nil
	}
	latest := make(map[spot]discord.AuditLogEntry)
	var before snowflake.ID
	for range auditLogPages {
		page, err := ops.GetAuditLog(core.ID(guildID), 0, 0, before, 0, 100)
		if err != nil {
			return 0, fmt.Errorf("roles: verify overwrite ownership: %w", err)
		}
		for _, e := range page.AuditLogEntries {
			before = e.ID
			if e.ActionType != discord.AuditLogEventChannelOverwriteCreate &&
				e.ActionType != discord.AuditLogEventChannelOverwriteUpdate &&
				e.ActionType != discord.AuditLogEventChannelOverwriteDelete {
				continue
			}
			if e.TargetID == nil || e.Options == nil || e.Options.ID == nil {
				continue
			}
			key := spot{e.TargetID.String(), *e.Options.ID}
			if previous, ok := latest[key]; candidates[key] && (!ok || e.ID > previous.ID) {
				latest[key] = e
			}
		}
		if len(page.AuditLogEntries) < 100 {
			break
		}
	}
	removed := 0
	var firstErr error
	for key, event := range latest {
		if event.UserID != p.botID || event.ActionType == discord.AuditLogEventChannelOverwriteDelete {
			continue
		}
		if userID == "" {
			if _, jailed, err := p.store.GetJail(ctx, guildID, key.user); err != nil || jailed {
				continue
			}
		}
		ch, err := guildChannel(ops, core.ID(key.channel))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ow := findOverwrite(ch, key.user, discord.PermissionOverwriteTypeMember)
		if ow == nil || !jailMemberShape(ch, *ow) {
			continue
		}
		if err := ops.DeletePermissionOverwrite(ch.ID(), core.ID(key.user)); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

// Reconcile categories before re-reading children because Discord propagates
// category edits to children whose overwrites are still synchronized.
func (p *Plugin) syncAllJailChannelOverwrites(guildID, jailRoleID string) error {
	return p.syncAllVisibility(guildID, p.jailMarker(guildID, jailRoleID))
}

// syncAllVisibility is syncAllJailChannelOverwrites for either marker.
func (p *Plugin) syncAllVisibility(guildID string, m visibilityMarker) error {
	if p.dryRun(guildID) {
		return fmt.Errorf("channel sync pending while dry-run is enabled")
	}
	unlock := p.lockVisibility(guildID)
	defer unlock()
	m = p.currentMarker(guildID, m)
	ops := p.ops(guildID)
	channels, err := ops.GetGuildChannels(core.ID(guildID))
	if err != nil {
		return fmt.Errorf("roles: list guild channels: %w", err)
	}
	apply := func(ch discord.GuildChannel) error {
		allow, deny, needed := markerOverwrite(guildID, ch, m.roleID, m.access(ch.ID().String()))
		if !needed {
			return nil
		}
		return applyOverwrite(ops, ch, m.roleID, allow, deny)
	}
	logErr := func(ch discord.GuildChannel, err error) {
		p.log.Error("roles: sync overwrite failed", "guild", guildID, "marker", m.name, "channel", ch.ID(), "err", err)
	}
	var categories []discord.GuildChannel
	for _, ch := range channels {
		if ch.Type() == discord.ChannelTypeGuildCategory {
			categories = append(categories, ch)
		}
	}
	firstErr := p.forEachChannelConcurrent(categories, apply, logErr)
	// Discord propagates category edits to existing synced children. Re-read
	// after all category writes, including a partial failure, before diffing.
	channels, err = ops.GetGuildChannels(core.ID(guildID))
	if err != nil {
		return fmt.Errorf("roles: refresh channels after category sync: %w", err)
	}
	var children []discord.GuildChannel
	for _, ch := range channels {
		if jailManagedChannelType(ch.Type()) {
			children = append(children, ch)
		}
	}
	if err := p.forEachChannelConcurrent(children, apply, logErr); firstErr == nil {
		firstErr = err
	}
	if firstErr != nil {
		p.markVisibilityDirty(guildID)
	}
	return firstErr
}
