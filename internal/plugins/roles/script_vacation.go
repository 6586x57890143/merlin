package roles

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

// The vacation script: jail, sent somewhere warmer.
//
// /roles vacation does exactly what /roles jail does (snapshot the member's
// roles, strip them to one marker, restore on a timer or on demand, survive
// a rejoin, yield to a mod who released them by hand) with a different
// marker role: the island's, which the guild already built the channel
// permissions for. That is the whole difference, and it is why a vacation
// is not a second kind of record. It is a role_jails row whose jail_role_id
// is the vacation role, and which of the two a row is gets derived from
// that column (sentenceFor), never stored beside it: a kind column next to
// a marker column is two facts that have to agree, and what a disagreement
// produces is a member released with the wrong words or, worse, hardened
// with the wrong overwrites.
//
// merlin never creates the vacation role (contrast resolveJailRole): it
// is the guild's own, the Melting Pot's island or whatever /roles configure
// vacation-role names, and the command refuses without one. Its channel
// visibility is managed exactly as the jail marker's is, deny everywhere
// but the vacation allowlist, with two differences spelled out above
// syncAllVacationOverwrites: the sync runs on explicit moments rather than
// on resolve, and it merges with what the guild already set on a channel
// rather than replacing it.
//
// A member can be moved between the two, in either direction, and the move
// keeps their snapshot and takes the new duration: /roles jail on somebody
// on vacation cuts the trip short, /roles vacation on somebody in jail
// paroles them to the beach (jailMany's transfer branch). A mod who swaps
// the markers by hand in Discord gets the same treatment from
// HandleMemberUpdate and the sweep (detectManualTransfer): the row follows
// the marker, the sentence keeps its end, and merlin says what happened.
//
// Like every script it is off until an admin turns it on, and the switch
// gates only the /roles vacation command: a transfer or a release of
// somebody already on the island works whatever the switch says, because
// turning a script off must never strand anyone in it.

const scriptVacation = "vacation"

// errVacationOff is what /roles vacation says while the script is off.
var errVacationOff = errors.New("the vacation script is off in this server: `/roles scripts set script:vacation enabled:true`")

// sentence is everything that differs between a jail and a vacation once
// the row exists: the words, and the audit action names. One table rather
// than a branch at every call site, so a surface that forgets to consult it
// reads as a jail everywhere, which is the wrong words and nothing worse.
type sentence struct {
	name        string // "jail" / "vacation", for audit detail and log lines
	title       string // "Member jailed" / "Member sent on vacation"
	verb        string // "jailed" / "sent on vacation"
	noticeKey   voice.Key
	overKey     voice.Key
	announceKey voice.Key
	overAnnKey  voice.Key
	// The transfer *into* this sentence, DM and channel post.
	intoKey      voice.Key
	intoAnnKey   voice.Key
	audit        string // one member
	auditBulk    string
	auditMoved   string // re-date
	auditPending string // recorded against somebody who is not in the guild
}

var (
	jailSentence = sentence{
		name: "jail", title: "Member jailed", verb: "jailed",
		noticeKey: voice.KeyJailNotice, overKey: voice.KeyReleaseNotice,
		announceKey: voice.KeyJailAnnounce, overAnnKey: voice.KeyReleaseAnnounce,
		intoKey: voice.KeyVacationToJail, intoAnnKey: voice.KeyVacationToJailAnnounce,
		audit: "roles.jail", auditBulk: "roles.jail_bulk", auditMoved: "roles.jail_resentenced",
		auditPending: "roles.jail_pending",
	}
	vacationSentence = sentence{
		name: "vacation", title: "Member sent on vacation", verb: "sent on vacation",
		noticeKey: voice.KeyVacationNotice, overKey: voice.KeyVacationOver,
		announceKey: voice.KeyVacationAnnounce, overAnnKey: voice.KeyVacationOverAnnounce,
		intoKey: voice.KeyVacationFromJail, intoAnnKey: voice.KeyVacationFromJailAnnounce,
		audit: "roles.vacation", auditBulk: "roles.vacation_bulk", auditMoved: "roles.vacation_resentenced",
		auditPending: "roles.vacation_pending",
	}
)

// sentenceFor says which sentence a row with this marker is serving. No
// Discord call: the vacation role is whatever the guild configured or the
// Melting Pot default, and anything else is a jail, including a marker the
// guild has since reconfigured away from, which is the safe direction (a
// jail's words and a jail's hardening, never a vacation's leniency for a
// row that might be a jail).
func (p *Plugin) sentenceFor(guildID, markerRoleID string) sentence {
	if markerRoleID != "" && markerRoleID == p.configuredVacationRole(guildID) {
		return vacationSentence
	}
	return jailSentence
}

// configuredVacationRole is the vacation role this guild would use, from
// configuration alone: the configured role, else the Melting Pot's. Empty
// when there is none. Cheaper than resolveVacationRole and never wrong
// about the ID, only about whether the role still exists.
func (p *Plugin) configuredVacationRole(guildID string) string {
	if id := p.jailChannelConfig.VacationRoleID(guildID); id != "" {
		return id
	}
	if guildID == meltingPotGuildID {
		return meltingPotDefaultVacationRoleID
	}
	return ""
}

// resolveVacationRole returns guildID's vacation marker role, confirming it
// exists. Nothing is created (contrast resolveJailRole): the island is the
// guild's own, and a role merlin made would carry none of its permissions.
// A configured role that has been deleted is cleared, so the next attempt
// reports "not configured" rather than the same missing-role error forever.
func (p *Plugin) resolveVacationRole(guildID string) (string, error) {
	p.jailRoleMu.Lock()
	defer p.jailRoleMu.Unlock()
	if id, ok := p.vacationRoleID[guildID]; ok {
		return id, nil
	}
	want := p.configuredVacationRole(guildID)
	if want == "" {
		return "", errors.New("no vacation role is configured for this server: `/roles configure vacation-role`")
	}
	rolesList, err := p.ops(guildID).GuildRoles(guildID)
	if err != nil {
		return "", fmt.Errorf("roles: list guild roles: %w", err)
	}
	if findRole(rolesList, want) == nil {
		if p.jailChannelConfig.VacationRoleID(guildID) == want {
			if err := p.jailChannelConfig.SetVacationRole(context.Background(), guildID, ""); err != nil {
				p.log.Error("roles: failed to clear missing vacation role", "guild", guildID, "role", want, "err", err)
			}
		}
		return "", fmt.Errorf("the vacation role %s no longer exists; set another with `/roles configure vacation-role`", core.MentionRole(want))
	}
	p.vacationRoleID[guildID] = want
	return want, nil
}

func (p *Plugin) forgetVacationRole(guildID string) {
	p.jailRoleMu.Lock()
	defer p.jailRoleMu.Unlock()
	delete(p.vacationRoleID, guildID)
}

// knownJailRole is the jail marker as far as this process knows without
// asking Discord: the cache, the configured role, or the Melting Pot's.
// Empty when none is known, in which case a hand swap onto the jail marker
// is not recognised until something resolves it (the next /roles jail).
func (p *Plugin) knownJailRole(guildID string) string {
	p.jailRoleMu.Lock()
	id, ok := p.jailRoleID[guildID]
	p.jailRoleMu.Unlock()
	if ok {
		return id
	}
	if id := p.jailChannelConfig.JailMarkerRoleID(guildID); id != "" {
		return id
	}
	if guildID == meltingPotGuildID {
		return meltingPotDefaultJailRoleID
	}
	return ""
}

// counterpartMarker is the other marker: the vacation role for a jail row,
// the jail role for a vacation row. ok is false when it is not known.
func (p *Plugin) counterpartMarker(guildID, markerRoleID string) (string, bool) {
	var other string
	if p.sentenceFor(guildID, markerRoleID) == vacationSentence {
		other = p.knownJailRole(guildID)
	} else {
		other = p.configuredVacationRole(guildID)
	}
	return other, other != "" && other != markerRoleID
}

// transferJail moves userID's existing sentence onto newMarker and re-dates
// it. currentRoles is what they hold now. Strip first, then move the row:
// the other order leaves a row saying "vacation" over a member still holding
// the jail marker if the edit fails, and release would then read the marker
// as already gone and drop the row with the member still stuck. A member
// re-marked but not yet re-recorded is the state detectManualTransfer
// already repairs on the next event or sweep, so that failure heals itself.
//
// Leaving jail also clears any member-level denies a hardened jail wrote
// (syncMemberJailOverwrites), or the beach would be as locked as the nest.
//
// The row is claimed for the duration, the same claim releaseJail takes.
// Strip-first means there is a moment where the member wears the new
// marker and the row still names the old one, and the role edit's own
// GUILD_MEMBER_UPDATE lands inside it: HandleMemberUpdate read that as a
// mod swapping the roles by hand and announced the same move a second
// time. A caller that finds the row claimed leaves it to whoever holds it.
func (p *Plugin) transferJail(ctx context.Context, guildID string, t jailTarget, from JailRecord, newMarker string, releaseAt *time.Time) error {
	userID := t.userID
	key := jailKey(guildID, userID)
	if !p.claim(key) {
		return errReleaseInProgress
	}
	defer p.unclaim(key)
	// An absent target has no roles to move; only the row does. The marker
	// they end up wearing is whichever one the row names when they arrive,
	// which reapplyIfEvaded reads fresh, so moving somebody between the nest
	// and the island before they get here works with nothing stripped.
	expected, _ := jailRoles(p.perms, guildID, newMarker, t.roles)
	if !t.absent && !sameRoleSet(expected, t.roles) {
		if _, err := p.stripToJailRoles(guildID, userID, expected); err != nil {
			return fmt.Errorf("roles: move %s to %s marker: %w", userID, p.sentenceFor(guildID, newMarker).name, err)
		}
	}
	if err := p.store.TransferJail(ctx, guildID, userID, newMarker, releaseAt); err != nil {
		return err
	}
	if p.sentenceFor(guildID, from.JailRoleID) == jailSentence {
		if err := p.clearMemberJailOverwrites(guildID, userID); err != nil {
			p.log.Warn("roles: failed to clear member-level jail overwrites on transfer", "guild", guildID, "user", userID, "err", err)
		}
	}
	if releaseAt != nil {
		p.armJailRelease(guildID, userID, *releaseAt)
	}
	return nil
}

// detectManualTransfer recognises a mod moving somebody between the nest
// and the island by hand: rec's marker is gone from roles and the other
// marker is on them instead. The row follows the marker, keeping its end
// (a hand swap is a change of place, not of sentence), the member is
// stripped to the new marker's set in case anything else came along, and
// merlin voices the move where the guild's announcements go. Reports true
// when it handled the change; the caller then leaves the row alone.
//
// Why this exists: without it a hand swap reads as the marker being gone,
// which is the confused-deputy "mod released them" case, and the row would
// be dropped when the sentence came due with the member still holding the
// island's role and nothing tracking them.
func (p *Plugin) detectManualTransfer(ctx context.Context, guildID string, rec JailRecord, roles []string) bool {
	if slices.Contains(roles, rec.JailRoleID) {
		return false
	}
	other, ok := p.counterpartMarker(guildID, rec.JailRoleID)
	if !ok || !slices.Contains(roles, other) {
		return false
	}
	if err := p.transferJail(ctx, guildID, jailTarget{userID: rec.UserID, roles: roles}, rec, other, rec.ReleaseAt); err != nil {
		if !errors.Is(err, errReleaseInProgress) {
			p.log.Error("roles: record a hand transfer", "guild", guildID, "user", rec.UserID, "err", err)
		}
		// Handled as far as the caller is concerned: not a release. A claim
		// miss is a command transfer mid-flight, which voices the move
		// itself.
		return true
	}
	to := p.sentenceFor(guildID, other)
	p.log.Warn("roles: member moved by hand", "guild", guildID, "user", rec.UserID, "to", to.name)
	if err := p.audit.Record(ctx, guildID, core.ActorSystem, "roles.transferred", core.MentionUser(rec.UserID),
		fmt.Sprintf("moved by hand from %s to %s; until %s", p.sentenceFor(guildID, rec.JailRoleID).name, to.name, releaseAtText(rec))); err != nil {
		p.log.Error("roles: audit hand transfer failed", "guild", guildID, "user", rec.UserID, "err", err)
	}
	p.publishTransferred(ctx, guildID, rec.UserID, core.ActorSystem, "", p.sentenceFor(guildID, rec.JailRoleID), to, rec.ReleaseAt)
	p.notifyMoved(ctx, guildID, rec.UserID, rec.ReleaseAt, to, "")
	p.announceMoved(ctx, guildID, "", []string{rec.UserID}, rec.ReleaseAt, to)
	return true
}

// handleVacation is /roles vacation: handleJail with the island's marker,
// behind the script switch. Same deferral, same rank check before anything
// is written, same jailMany, so the two cannot drift in behaviour.
func (p *Plugin) handleVacation(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := core.LeafArgs(i)
	userIDs := collectJailUserIDs(opts)
	duration, err := core.ParseFlexibleDuration(opts["duration"].StringValue())
	if err != nil {
		core.RespondErr(s, i, "Invalid duration", err)
		return
	}
	reason := ""
	if v, ok := opts["reason"]; ok {
		reason = v.StringValue()
	}
	if len(userIDs) == 0 {
		core.RespondErr(s, i, "No members given", errors.New("pick at least one member to send on vacation"))
		return
	}
	if !p.scriptOn(ctx, i.GuildID, scriptVacation) {
		core.RespondErr(s, i, "Vacation is off", errVacationOff)
		return
	}

	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("roles: defer vacation response failed", "guild", i.GuildID, "err", err)
		return
	}
	fail := func(title string, err error) {
		if ferr := core.FollowUpErr(s, i, title, err); ferr != nil {
			p.log.Error("roles: vacation follow-up failed", "guild", i.GuildID, "err", ferr)
		}
	}
	if p.dryRun(i.GuildID) {
		if err := core.FollowUpOK(s, i, "Dry-run", fmt.Sprintf("Dry-run is enabled for this server: %s not sent on vacation. Turn it off with `/config dryrun enabled:false`.",
			mentionList(userIDs))); err != nil {
			p.log.Error("roles: vacation dry-run follow-up failed", "guild", i.GuildID, "err", err)
		}
		return
	}

	targets, fetchFailed := p.resolveTargets(i.GuildID, userIDs)
	allowed, res := p.partitionByRank(i.GuildID, i.Member, targets)
	res.failed = append(res.failed, fetchFailed...)

	if len(allowed) > 0 {
		roleID, err := p.resolveVacationRole(i.GuildID)
		if err != nil {
			fail("No vacation role", err)
			return
		}
		if err := p.perms.CanManageRole(i.GuildID, roleID); err != nil {
			fail("Cannot use the vacation role", err)
			return
		}
		res = res.merge(p.jailMany(ctx, i.GuildID, roleID, allowed, duration, actorID(i), reason))
	}

	p.reportSentence(ctx, s, i, vacationSentence, userIDs, duration, reason, res)
}

// scriptOn reads a script's switch, treating no store and an unreadable
// switch as off, like enforceEternalRoles.
func (p *Plugin) scriptOn(ctx context.Context, guildID, script string) bool {
	if p.scripts == nil {
		return false
	}
	on, err := p.scripts.Enabled(ctx, guildID, script)
	if err != nil {
		p.log.Error("roles: read script switch, treating as off", "guild", guildID, "script", script, "err", err)
		return false
	}
	return on
}

// handleVacationRole is /roles configure vacation-role: set, or with the
// option omitted clear, the island's marker. Setting syncs every channel
// for the new role (deferred: that is O(channels)), and the answer reads
// back what the role can now see.
func (p *Plugin) handleVacationRole(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	roleID := ""
	if opt, ok := core.LeafArgs(i)["role"]; ok {
		roleID, _ = opt.Value.(string)
	}
	// Setting a role syncs every channel, O(channels), past Discord's
	// 3-second deadline in a large guild. Every path below answers with a
	// follow-up, the clear included, so the two cannot drift.
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("roles: defer vacation-role response failed", "guild", i.GuildID, "err", err)
		return
	}
	if err := p.jailChannelConfig.SetVacationRole(ctx, i.GuildID, roleID); err != nil {
		_ = core.FollowUpErr(s, i, "Failed to save", err)
		return
	}
	p.forgetVacationRole(i.GuildID)
	detail := "none"
	if roleID != "" {
		detail = core.MentionRole(roleID)
	}
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.configure_jail_channels", "", "vacation_role="+detail); err != nil {
		p.log.Error("roles: audit vacation-role failed", "guild", i.GuildID, "err", err)
	}
	if roleID == "" {
		msg := "No vacation role is configured."
		if i.GuildID == meltingPotGuildID {
			msg = fmt.Sprintf("Cleared. merlin will use %s, the Melting Pot's own island role.", core.MentionRole(meltingPotDefaultVacationRoleID))
		}
		_ = core.FollowUpOK(s, i, "Vacation role cleared", msg)
		return
	}
	if err := p.syncAllVacationOverwrites(i.GuildID); err != nil {
		p.log.Error("roles: sync vacation overwrites for new role failed", "guild", i.GuildID, "err", err)
	}
	_ = core.FollowUpOK(s, i, "Vacation role set",
		fmt.Sprintf("Members on vacation will hold %s.\n\n%s", core.MentionRole(roleID), p.describeRoleAccess(i.GuildID, roleID)))
}

// describeRoleAccess is the read-back after a sync: which channels carry an
// overwrite for roleID allowing or denying View. Effective permissions are
// not computed (that is Discord's job and a much longer function); this is
// what an admin needs to confirm the beach is the only room left open.
func (p *Plugin) describeRoleAccess(guildID, roleID string) string {
	const wontTouch = "merlin only moves this role's View (and Connect) bits, deny everywhere but the vacation allowlist; everything else on each channel's overwrite is left as the server set it."
	channels, err := p.ops(guildID).GuildChannels(guildID)
	if err != nil {
		return "Could not read this server's channels to describe what the role sees. " + wontTouch
	}
	var allowed, denied []string
	for _, ch := range channels {
		ow := findOverwrite(ch, roleID, discordgo.PermissionOverwriteTypeRole)
		if ow == nil {
			continue
		}
		switch {
		case ow.Deny&discordgo.PermissionViewChannel != 0:
			denied = append(denied, core.MentionChannel(ch.ID))
		case ow.Allow&discordgo.PermissionViewChannel != 0:
			allowed = append(allowed, core.MentionChannel(ch.ID))
		}
	}
	if len(allowed) == 0 && len(denied) == 0 {
		return "This role has no channel overwrites of its own yet, so vacation currently changes nothing about what a member can see. " + wontTouch
	}
	var b strings.Builder
	fmt.Fprintf(&b, "It is explicitly allowed to view %d channel(s) and denied %d.", len(allowed), len(denied))
	if len(allowed) > 0 {
		fmt.Fprintf(&b, "\nAllowed: %s", strings.Join(allowed, " "))
	}
	if len(denied) > 0 {
		fmt.Fprintf(&b, "\nDenied: %s", strings.Join(denied, " "))
	}
	b.WriteString("\n" + wontTouch)
	return core.TruncateEmbedDescription(b.String())
}

// The island's visibility. The same deny-by-default sync as the jail
// marker (jailchannels.go), against the vacation allowlist: every managed
// channel gets a deny for the island's role except the listed ones, so a
// member on vacation sees the beach and nothing else. What differs from
// jail is *when* it runs, and how it treats what it finds. It runs when
// the script is turned on, when the role or the list changes, and on
// /roles configure sync-channels; never on resolve, so a restart writes
// nothing (see resolveJailRole for why that matters to the guild's audit
// log). And desiredOverwrite merges with an overwrite the guild already
// set rather than replacing it, so the beach keeps working the way it was
// built and only its visibility bits are ever moved.

// vacationAllowlist is the configured list, or the Melting Pot's beach
// there when nothing is configured, mirroring the role default.
func (p *Plugin) vacationAllowlist(guildID string) []string {
	if ids := p.jailChannelConfig.VacationAllowedChannelIDs(guildID); len(ids) > 0 {
		return ids
	}
	if guildID == meltingPotGuildID {
		return []string{meltingPotDefaultVacationChannelID}
	}
	return nil
}

// syncAllVacationOverwrites recomputes every channel for the island's
// role. Nothing to do without a configured role.
func (p *Plugin) syncAllVacationOverwrites(guildID string) error {
	roleID := p.configuredVacationRole(guildID)
	if roleID == "" {
		return nil
	}
	return p.syncAllVisibility(guildID, p.vacationMarker(guildID, roleID))
}

func (p *Plugin) syncVacationChannel(guildID, channelID string) (withheld int64, err error) {
	roleID := p.configuredVacationRole(guildID)
	if roleID == "" {
		return 0, nil
	}
	return p.syncChannelVisibility(guildID, p.vacationMarker(guildID, roleID), channelID)
}

func (p *Plugin) handleVacationAllowChannel(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	channelID := core.LeafArgs(i)["channel"].Value.(string)
	if err := p.jailChannelConfig.AddVacationAllowedChannel(ctx, i.GuildID, channelID); err != nil {
		core.RespondErr(s, i, "Failed to save", err)
		return
	}
	withheld, err := p.syncVacationChannel(i.GuildID, channelID)
	if err != nil {
		p.log.Error("roles: sync vacation channel failed", "guild", i.GuildID, "channel", channelID, "err", err)
	}
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.configure_jail_channels", "", "vacation_allow="+core.MentionChannel(channelID)+withheldDetail(withheld)); err != nil {
		p.log.Error("roles: audit vacation-allow-channel failed", "guild", i.GuildID, "err", err)
	}
	p.respondAllowed(s, i, "members on vacation", channelID, withheld)
}

func (p *Plugin) handleVacationDisallowChannel(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	channelID := core.LeafArgs(i)["channel"].Value.(string)
	if err := p.jailChannelConfig.RemoveVacationAllowedChannel(ctx, i.GuildID, channelID); err != nil {
		core.RespondErr(s, i, "Failed to save", err)
		return
	}
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "roles.configure_jail_channels", "vacation_allow="+core.MentionChannel(channelID), ""); err != nil {
		p.log.Error("roles: audit vacation-disallow-channel failed", "guild", i.GuildID, "err", err)
	}
	if _, err := p.syncVacationChannel(i.GuildID, channelID); err != nil {
		p.log.Error("roles: sync vacation channel failed", "guild", i.GuildID, "channel", channelID, "err", err)
	}
	core.RespondOK(s, i, "Channel hidden", fmt.Sprintf("<#%s> is hidden from members on vacation again.", channelID))
}

// vacationLine renders the script's state for /roles scripts list and the
// enable notice.
func (p *Plugin) vacationLine(guildID string) string {
	if id := p.configuredVacationRole(guildID); id != "" {
		return "island role " + core.MentionRole(id)
	}
	return "no island role configured (`/roles configure vacation-role`)"
}
