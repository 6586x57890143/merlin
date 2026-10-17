package roles

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordtest"
)

func TestSyncAllJailChannelOverwritesDeniesByDefaultAllowsListed(t *testing.T) {
	ops := newFakeOps()
	ops.channel["7000609"] = &fch{ID: "7000609", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	ops.channel["7000619"] = &fch{ID: "7000619", GuildID: "7000001", Type: discord.ChannelTypeGuildVoice}
	ops.channel["7000613"] = &fch{ID: "7000613", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	ops.channel["7000618"] = &fch{ID: "7000618", GuildID: "7000001", Type: discord.ChannelTypeGuildCategory}

	settings := newFakeSettings()
	settings.allowed["7000001"] = []string{"7000613"}

	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.syncAllJailChannelOverwrites("7000001", "7000301"); err != nil {
		t.Fatalf("syncAllJailChannelOverwrites: %v", err)
	}

	textOverwrite, ok := ops.overwrites[overwriteKey{"7000609", "7000301"}]
	if !ok {
		t.Fatal("expected a deny overwrite on text1")
	}
	if textOverwrite.deny&discord.PermissionViewChannel == 0 {
		t.Fatalf("expected ViewChannel denied on text1, got deny=%d", textOverwrite.deny)
	}

	voiceOverwrite, ok := ops.overwrites[overwriteKey{"7000619", "7000301"}]
	if !ok {
		t.Fatal("expected a deny overwrite on voice1")
	}
	if voiceOverwrite.deny&discord.PermissionConnect == 0 {
		t.Fatalf("expected Connect denied on voice1 (voice channel), got deny=%d", voiceOverwrite.deny)
	}

	// Allowlisted: expect an explicit allow overwrite.
	owAllowed, ok := ops.overwrites[overwriteKey{"7000613", "7000301"}]
	if !ok {
		t.Fatal("expected an overwrite on the allowlisted channel")
	}
	if owAllowed.allow&discord.PermissionViewChannel == 0 || owAllowed.allow&discord.PermissionSendMessages == 0 {
		t.Fatalf("expected allowlisted channel to allow view+send, got allow=%d", owAllowed.allow)
	}
	// Categories get a plain deny too, so a channel created under one later
	// starts denied by default (Discord copies a category's overwrites onto
	// a channel created with none of its own) rather than visible until the
	// next sync-channels run notices it. This has no effect on existing
	// channels: Discord's live permission check never consults a channel's
	// parent category.
	categoryOverwrite, ok := ops.overwrites[overwriteKey{"7000618", "7000301"}]
	if !ok {
		t.Fatal("expected a deny overwrite on the category, for channels created under it later")
	}
	if categoryOverwrite.deny&discord.PermissionViewChannel == 0 || categoryOverwrite.deny&discord.PermissionConnect == 0 {
		t.Fatalf("expected the category deny to cover both ViewChannel and Connect (any future child type), got deny=%d", categoryOverwrite.deny)
	}
}

func TestSyncAllJailChannelOverwritesAllowsSendOnNewsChannel(t *testing.T) {
	ops := newFakeOps()
	ops.channel["7000620"] = &fch{ID: "7000620", GuildID: "7000001", Type: discord.ChannelTypeGuildNews}
	settings := newFakeSettings()
	settings.allowed["7000001"] = []string{"7000620"}

	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.syncAllJailChannelOverwrites("7000001", "7000301"); err != nil {
		t.Fatalf("syncAllJailChannelOverwrites: %v", err)
	}

	ow, ok := ops.overwrites[overwriteKey{"7000620", "7000301"}]
	if !ok {
		t.Fatal("expected an overwrite on the allowlisted news channel")
	}
	if ow.allow&discord.PermissionViewChannel == 0 || ow.allow&discord.PermissionSendMessages == 0 {
		t.Fatalf("expected allowlisted news channel to allow view+send, got allow=%d", ow.allow)
	}
}

// TestSyncAllJailChannelOverwritesClearsRemovedAllowEntry verifies a
// channel previously allowlisted, then removed from the allowlist, gets its
// stale "no overwrite" state replaced with an explicit deny, which is the whole
// point of sync-channels being re-runnable.
func TestSyncAllJailChannelOverwritesClearsRemovedAllowEntry(t *testing.T) {
	ops := newFakeOps()
	ops.channel["7000605"] = &fch{ID: "7000605", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	settings := newFakeSettings()
	settings.allowed["7000001"] = []string{"7000605"}

	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())
	if err := p.syncAllJailChannelOverwrites("7000001", "7000301"); err != nil {
		t.Fatalf("syncAllJailChannelOverwrites: %v", err)
	}
	ow, ok := ops.overwrites[overwriteKey{"7000605", "7000301"}]
	if !ok {
		t.Fatal("expected an overwrite while allowlisted")
	}
	if ow.allow&discord.PermissionViewChannel == 0 || ow.allow&discord.PermissionSendMessages == 0 {
		t.Fatalf("expected allowlisted channel to allow view+send, got allow=%d", ow.allow)
	}

	settings.allowed["7000001"] = nil
	if err := p.syncAllJailChannelOverwrites("7000001", "7000301"); err != nil {
		t.Fatalf("syncAllJailChannelOverwrites (2nd): %v", err)
	}
	if _, ok := ops.overwrites[overwriteKey{"7000605", "7000301"}]; !ok {
		t.Fatal("expected a deny overwrite after removal from allowlist")
	}
}

func TestSyncJailChannelOverwriteSingleChannel(t *testing.T) {
	ops := newFakeOps()
	ops.channel["7000605"] = &fch{ID: "7000605", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	settings := newFakeSettings()

	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	if _, err := p.syncJailChannelOverwrite("7000001", "7000301", "7000605"); err != nil {
		t.Fatalf("syncJailChannelOverwrite: %v", err)
	}
	if _, ok := ops.overwrites[overwriteKey{"7000605", "7000301"}]; !ok {
		t.Fatal("expected deny overwrite for a non-allowlisted channel")
	}

	settings.allowed["7000001"] = []string{"7000605"}
	if _, err := p.syncJailChannelOverwrite("7000001", "7000301", "7000605"); err != nil {
		t.Fatalf("syncJailChannelOverwrite (allowed): %v", err)
	}
	ow, ok := ops.overwrites[overwriteKey{"7000605", "7000301"}]
	if !ok {
		t.Fatal("expected an overwrite once allowlisted")
	}
	if ow.allow&discord.PermissionViewChannel == 0 || ow.allow&discord.PermissionSendMessages == 0 {
		t.Fatalf("expected allowlisted channel to allow view+send, got allow=%d", ow.allow)
	}
}
// memberDeny is the member-level jail deny older versions wrote.
func memberDeny(userID string) *fow {
	return &fow{ID: userID, Type: discord.PermissionOverwriteTypeMember, Deny: discord.PermissionViewChannel}
}

func hasOverwrite(ch *fch, id string, kind discord.PermissionOverwriteType) bool {
	return discordtest.Find(ch.PermissionOverwrites, id, kind) != nil
}

// The bug report: release recomputed "at risk" and only cleared there, so a
// member deny on a channel whose role allow had gone in the meantime stayed
// after release. Now every channel is checked. A member deny of any other
// shape is somebody's hand-set decision and stays.
func TestReleaseClearsMemberDenyOnEveryChannel(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000301"})
	ops.roles["7000001"] = []*frole{{ID: "7000302"}, {ID: "7000301"}}
	noLongerAtRisk := lockedRoom("7000611", discord.ChannelTypeGuildText, memberDeny("7000102"))
	handSet := lockedRoom("7000612", discord.ChannelTypeGuildText,
		&fow{ID: "7000102", Type: discord.PermissionOverwriteTypeMember, Deny: discord.PermissionViewChannel | discord.PermissionSendMessages})
	ops.channel["7000611"], ops.channel["7000612"] = noLongerAtRisk, handSet

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	rec := JailRecord{GuildID: "7000001", UserID: "7000102", SnapshotRoleIDs: []string{"7000302"}, JailRoleID: "7000301"}
	if err := p.releaseJail(context.Background(), "7000001", "7000102", rec, core.ActorSystem); err != nil {
		t.Fatalf("releaseJail: %v", err)
	}
	if hasOverwrite(noLongerAtRisk, "7000102", discord.PermissionOverwriteTypeMember) {
		t.Error("released member still carries the old member deny")
	}
	if !hasOverwrite(handSet, "7000102", discord.PermissionOverwriteTypeMember) {
		t.Error("release removed a member overwrite of a shape merlin never wrote")
	}
}

// Neither half of the evasion routine writes a member overwrite any more,
// even on a channel where another role's allow would once have qualified.
func TestEvasionWritesNoMemberOverwrites(t *testing.T) {
	ops := newFakeOps()
	ops.channel["7000611"] = lockedRoom("7000611", discord.ChannelTypeGuildText,
		&fow{ID: "7000316", Type: discord.PermissionOverwriteTypeRole, Allow: discord.PermissionViewChannel})
	ops.setMemberJoined("7000001", "7000102", []string{"7000316"}, fixedNow.Add(-time.Minute))
	store := newFakeStore()
	_ = store.InsertJail(context.Background(), activeJail("7000102", []string{"7000302"}))
	p := newEvasionPlugin(t, ops, store)
	if err := p.reapplyEvadedJails(context.Background(), "7000001"); err != nil {
		t.Fatalf("reapplyEvadedJails: %v", err)
	}
	p.HandleMemberUpdate(context.Background(), "7000001", "7000102", []string{"7000301", "7000316"})
	if ops.permSetCalls != 0 {
		t.Fatalf("evasion wrote %d permission overwrites, want none", ops.permSetCalls)
	}
}

// A marker deny on a room @everyone is already denied is one more overwrite
// saying nothing: a jailed member holds @everyone and the marker. The sync
// writes none there, removes one in exactly merlin's shape, leaves one the
// guild edited, and still denies the room @everyone can see.
func TestMarkerSkipsRoomsEveryoneCannotSee(t *testing.T) {
	everyoneDenied := func(id string, extra ...*fow) *fch {
		return lockedRoom(id, discord.ChannelTypeGuildText,
			append([]*fow{{ID: "7000001", Type: discord.PermissionOverwriteTypeRole, Deny: discord.PermissionViewChannel}}, extra...)...)
	}
	ops := newFakeOps()
	fresh := everyoneDenied("7000611")
	stale := everyoneDenied("7000612", &fow{ID: "7000301", Type: discord.PermissionOverwriteTypeRole, Deny: discord.PermissionViewChannel | discord.PermissionConnect})
	edited := everyoneDenied("7000613", &fow{ID: "7000301", Type: discord.PermissionOverwriteTypeRole, Deny: discord.PermissionViewChannel | discord.PermissionAddReactions})
	public := lockedRoom("7000614", discord.ChannelTypeGuildText)
	for _, ch := range []*fch{fresh, stale, edited, public} {
		ops.channel[ch.ID] = ch
	}
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.syncAllJailChannelOverwrites("7000001", "7000301"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if hasOverwrite(fresh, "7000301", discord.PermissionOverwriteTypeRole) {
		t.Error("wrote a marker deny on a room @everyone cannot see")
	}
	if hasOverwrite(stale, "7000301", discord.PermissionOverwriteTypeRole) {
		t.Error("kept merlin's own unnecessary marker deny")
	}
	if !hasOverwrite(edited, "7000301", discord.PermissionOverwriteTypeRole) {
		t.Error("removed a marker overwrite the guild had edited")
	}
	if ow := discordtest.Find(public.PermissionOverwrites, "7000301", discord.PermissionOverwriteTypeRole); ow == nil || ow.Deny&discord.PermissionViewChannel == 0 {
		t.Error("a room @everyone can see must still deny the marker")
	}
	if ops.permSetCalls != 1 || ops.permDeleteCalls != 1 {
		t.Fatalf("writes = %d set, %d delete; want 1 and 1", ops.permSetCalls, ops.permDeleteCalls)
	}
	if err := p.syncAllJailChannelOverwrites("7000001", "7000301"); err != nil || ops.permSetCalls != 1 || ops.permDeleteCalls != 1 {
		t.Fatalf("a repeat sync must write nothing: err=%v set=%d delete=%d", err, ops.permSetCalls, ops.permDeleteCalls)
	}
}

// Leftovers on members already released are only removed when Discord's
// audit log says merlin created them, since the shape alone matches a mod
// hiding a channel by hand. A member still jailed keeps theirs until
// release, and an unreadable audit log removes nothing.
func TestClearLeftoverMemberOverwritesNeedsMerlinsAuditEntry(t *testing.T) {
	ours := lockedRoom("7000611", discord.ChannelTypeGuildText, memberDeny("7000102"), memberDeny("7000103"))
	theirs := lockedRoom("7000612", discord.ChannelTypeGuildText, memberDeny("7000104"))
	ops := newFakeOps()
	ops.channel["7000611"], ops.channel["7000612"] = ours, theirs
	bot := core.ID("7000130")
	entry := func(id, channel, user string) discord.AuditLogEntry {
		target := core.ID(channel)
		return discord.AuditLogEntry{ID: core.ID(id), UserID: bot, ActionType: discord.AuditLogEventChannelOverwriteCreate,
			TargetID: &target, Options: &discord.OptionalAuditLogEntryInfo{ID: &user}}
	}
	ops.auditLog = []discord.AuditLogEntry{entry("1", "7000611", "7000102"), entry("2", "7000611", "7000103")}
	store := newFakeStore()
	_ = store.InsertJail(context.Background(), activeJail("7000103", nil))
	p := newTestPlugin(ops, store, newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	p.botID = bot

	ops.auditLogErr = errors.New("missing access")
	if n, err := p.clearLeftoverMemberOverwrites(context.Background(), "7000001"); err == nil || n != 0 || ops.permDeleteCalls != 0 {
		t.Fatalf("unreadable audit log: n=%d err=%v deletes=%d, want an error and nothing removed", n, err, ops.permDeleteCalls)
	}
	ops.auditLogErr = nil
	n, err := p.clearLeftoverMemberOverwrites(context.Background(), "7000001")
	if err != nil || n != 1 {
		t.Fatalf("removed %d (err %v), want 1", n, err)
	}
	if hasOverwrite(ours, "7000102", discord.PermissionOverwriteTypeMember) {
		t.Error("merlin's leftover on a released member survived")
	}
	if !hasOverwrite(ours, "7000103", discord.PermissionOverwriteTypeMember) {
		t.Error("removed the deny of a member who is still jailed")
	}
	if !hasOverwrite(theirs, "7000104", discord.PermissionOverwriteTypeMember) {
		t.Error("removed a member deny merlin never wrote")
	}
}

// The baseline: a marker role is never allowed more on an allowlisted
// channel than an ordinary member (@everyone plus the configured member
// role) can do there, and what it is refused goes into its own deny so the
// cap holds however the room was locked.

func lockedRoom(id string, typ discord.ChannelType, overwrites ...*fow) *fch {
	return &fch{ID: id, GuildID: "7000001", Type: typ, PermissionOverwrites: overwrites}
}

func TestMemberBaselineResolvesLikeDiscord(t *testing.T) {
	view, send := discord.PermissionViewChannel, discord.PermissionSendMessages
	roles := []*frole{
		{ID: "7000001", Permissions: view},        // @everyone: can see, cannot talk
		{ID: "7000321", Permissions: view | send}, // members: can talk
		{ID: "7000320", Permissions: discord.PermissionAdministrator},
	}
	plain := lockedRoom("7000692", discord.ChannelTypeGuildText)
	if got := memberBaseline("7000001", "", plain.Disgo(), disgoRoles(roles)); got != view {
		t.Fatalf("@everyone alone = %d, want view", got)
	}
	if got := memberBaseline("7000001", "7000321", plain.Disgo(), disgoRoles(roles)); got != view|send {
		t.Fatalf("with member role = %d, want view|send", got)
	}
	// An announcements room: @everyone denied Send at channel level beats
	// the member role's guild-level Send.
	announce := lockedRoom("7000690", discord.ChannelTypeGuildText, &fow{ID: "7000001", Type: discord.PermissionOverwriteTypeRole, Deny: send})
	if got := memberBaseline("7000001", "7000321", announce.Disgo(), disgoRoles(roles)); got != view {
		t.Fatalf("channel deny on @everyone = %d, want view", got)
	}
	// ...unless the member role's own overwrite gives it back.
	announce.PermissionOverwrites = append(announce.PermissionOverwrites, &fow{ID: "7000321", Type: discord.PermissionOverwriteTypeRole, Allow: send})
	if got := memberBaseline("7000001", "7000321", announce.Disgo(), disgoRoles(roles)); got != view|send {
		t.Fatalf("member role overwrite = %d, want view|send", got)
	}
	// A room locked only on the member role.
	memberLocked := lockedRoom("7000691", discord.ChannelTypeGuildText, &fow{ID: "7000321", Type: discord.PermissionOverwriteTypeRole, Deny: send})
	if got := memberBaseline("7000001", "7000321", memberLocked.Disgo(), disgoRoles(roles)); got != view {
		t.Fatalf("deny on member role = %d, want view", got)
	}
	if got := memberBaseline("7000001", "7000320", plain.Disgo(), disgoRoles(roles)); got != discord.PermissionsAll {
		t.Fatal("Administrator is everything")
	}
	if got := memberBaseline("7000001", "gone", plain.Disgo(), disgoRoles(roles)); got != view {
		t.Fatalf("a deleted member role contributes nothing, got %d", got)
	}
}

func TestDesiredOverwriteWithholdsWhatMembersLack(t *testing.T) {
	view, send, connect := discord.PermissionViewChannel, discord.PermissionSendMessages, discord.PermissionConnect

	// Fresh overwrite on a read-only room: View granted, Send refused and
	// denied outright, the ways out denied as always.
	allow, deny, withheld, needed := desiredOverwrite(lockedRoom("7000690", discord.ChannelTypeGuildText).Disgo(), "7000396", true, view)
	if !needed || allow != view || deny != restrictedBits|send || withheld != send {
		t.Fatalf("read-only room: allow=%d deny=%d withheld=%d", allow, deny, withheld)
	}
	// A room members cannot even see: nothing granted, View denied.
	allow, deny, withheld, _ = desiredOverwrite(lockedRoom("7000693", discord.ChannelTypeGuildText).Disgo(), "7000396", true, 0)
	if allow != 0 || deny&view == 0 || deny&send == 0 || withheld != view|send {
		t.Fatalf("hidden room: allow=%d deny=%d withheld=%d", allow, deny, withheld)
	}
	// A locked voice room withholds Connect.
	_, deny, withheld, _ = desiredOverwrite(lockedRoom("7000694", discord.ChannelTypeGuildVoice).Disgo(), "7000396", true, view)
	if withheld != connect || deny&connect == 0 {
		t.Fatalf("locked voice: deny=%d withheld=%d", deny, withheld)
	}
	// An escalation an earlier sync already wrote is repaired: Send comes
	// out of allow and goes into deny, everything else the guild set stays.
	escalated := lockedRoom("7000690", discord.ChannelTypeGuildText, &fow{ID: "7000396", Type: discord.PermissionOverwriteTypeRole,
		Allow: view | send | discord.PermissionAddReactions, Deny: restrictedBits})
	allow, deny, withheld, needed = desiredOverwrite(escalated.Disgo(), "7000396", true, view)
	if !needed || allow != view|discord.PermissionAddReactions || deny != restrictedBits|send || withheld != send {
		t.Fatalf("repair: allow=%d deny=%d withheld=%d needed=%v", allow, deny, withheld, needed)
	}
	// And once repaired, nothing more to write.
	escalated.PermissionOverwrites[0].Allow, escalated.PermissionOverwrites[0].Deny = allow, deny
	if _, _, _, needed = desiredOverwrite(escalated.Disgo(), "7000396", true, view); needed {
		t.Fatal("a repaired overwrite must not be rewritten")
	}
	// Off the allowlist the baseline is irrelevant.
	allow, deny, withheld, _ = desiredOverwrite(lockedRoom("7000695", discord.ChannelTypeGuildText).Disgo(), "7000396", false, 0)
	if allow != 0 || deny != view || withheld != 0 {
		t.Fatalf("off-list: allow=%d deny=%d withheld=%d", allow, deny, withheld)
	}
	// A category is never capped: its overwrite is only the template a new
	// channel inherits.
	if _, _, withheld, _ = desiredOverwrite(lockedRoom("7000626", discord.ChannelTypeGuildCategory).Disgo(), "7000396", true, 0); withheld != 0 {
		t.Fatal("categories carry no allow to withhold")
	}
}

// TestAllowChannelWarnsWhenMembersCannotPost: the live bug. Allowlisting
// an announcements room for the jail marker used to hand jailed members
// Send there; now they get View, an explicit Send deny, a warning naming
// it, and an audit line saying so.
func TestAllowChannelWarnsWhenMembersCannotPost(t *testing.T) {
	view, send := discord.PermissionViewChannel, discord.PermissionSendMessages
	p, ops, _, audit, _, settings := handlerFixture()
	settings.memberRole["7000001"] = "7000321"
	ops.roles["7000001"] = append(ops.roles["7000001"], &frole{ID: "7000321", Permissions: everyonePerms})
	ops.channel["7000616"] = lockedRoom("7000616", discord.ChannelTypeGuildText,
		&fow{ID: "7000001", Type: discord.PermissionOverwriteTypeRole, Deny: send})
	ops.channel["7000612"] = lockedRoom("7000612", discord.ChannelTypeGuildText)
	p.jailRoleID["7000001"] = "7000301"
	rt := handlerSession(t)

	set(p, panelJail+"allow", nil, "7000616")

	ow := ops.overwrites[overwriteKey{"7000616", "7000301"}]
	if ow.allow != view || ow.deny != restrictedBits|send {
		t.Fatalf("expected view only with Send denied, got %+v", ow)
	}
	if !rt.said("Channel allowed, with limits") || !rt.said("no Send Messages there") || !rt.said("<@&7000321>") {
		t.Fatalf("expected the warning naming Send and the baseline role, got %v", rt.bodies)
	}
	if len(audit.records) != 1 || !strings.Contains(audit.records[0].newValue, "withheld=Send Messages") {
		t.Fatalf("audit: %+v", audit.records)
	}

	// The appeals room is untouched by the cap: members can talk there.
	set(p, panelJail+"allow", nil, "7000612")
	if ow := ops.overwrites[overwriteKey{"7000612", "7000301"}]; ow.allow != view|send {
		t.Fatalf("expected view+send in the appeals room, got %+v", ow)
	}
	if !rt.said("✓ Channel allowed: <#7000612> stays visible to jailed members.") {
		t.Fatalf("expected a plain success, got %v", rt.bodies)
	}
}

// TestAllowChannelWarnsWhenMembersCannotSee: a room ordinary members
// cannot see at all is refused outright and the admin is told the entry
// does nothing.
func TestAllowChannelWarnsWhenMembersCannotSee(t *testing.T) {
	view := discord.PermissionViewChannel
	p, ops, _, _, _, _ := handlerFixture()
	ops.channel["7000330"] = lockedRoom("7000330", discord.ChannelTypeGuildText,
		&fow{ID: "7000001", Type: discord.PermissionOverwriteTypeRole, Deny: view})
	p.jailRoleID["7000001"] = "7000301"
	rt := handlerSession(t)

	set(p, panelJail+"allow", nil, "7000330")

	if ow := ops.overwrites[overwriteKey{"7000330", "7000301"}]; ow.allow != 0 || ow.deny&view == 0 {
		t.Fatalf("expected the room kept hidden, got %+v", ow)
	}
	if !rt.said("Channel allowed, but hidden anyway") || !rt.said("@everyone") {
		t.Fatalf("got %v", rt.bodies)
	}
}

// TestMemberRoleChangeResyncsBothMarkers: the baseline moved everywhere,
// so both markers are recomputed, and the room locked only on the member
// role is now caught.
func TestMemberRoleChangeResyncsBothMarkers(t *testing.T) {
	view, send := discord.PermissionViewChannel, discord.PermissionSendMessages
	p, ops, _, audit, _, settings := handlerFixture()
	// @everyone cannot talk anywhere; melted can, except in the locked room.
	ops.roles["7000001"] = append(ops.roles["7000001"],
		&frole{ID: "7000001", Permissions: view},
		&frole{ID: "7000321", Permissions: view | send},
		&frole{ID: "7000322", Permissions: 0})
	settings.allowed["7000001"] = []string{"7000612", "7000615"}
	settings.vacationRole["7000001"] = "7000322"
	settings.vacationAllowed["7000001"] = []string{"7000615"}
	ops.channel["7000612"] = lockedRoom("7000612", discord.ChannelTypeGuildText)
	ops.channel["7000615"] = lockedRoom("7000615", discord.ChannelTypeGuildText,
		&fow{ID: "7000321", Type: discord.PermissionOverwriteTypeRole, Deny: send})
	p.jailRoleID["7000001"] = "7000301"
	rt := handlerSession(t)

	set(p, panelJail+"member", nil, "7000321")

	if settings.memberRole["7000001"] != "7000321" || !rt.said("Member role set") {
		t.Fatalf("expected the role saved and confirmed, got %q / %v", settings.memberRole["7000001"], rt.bodies)
	}
	if ow := ops.overwrites[overwriteKey{"7000612", "7000301"}]; ow.allow != view|send {
		t.Fatalf("melted can talk in the appeals room, so jail may too: %+v", ow)
	}
	if ow := ops.overwrites[overwriteKey{"7000615", "7000301"}]; ow.allow != view || ow.deny&send == 0 {
		t.Fatalf("a room locked on the member role caps the jail marker: %+v", ow)
	}
	if ow := ops.overwrites[overwriteKey{"7000615", "7000322"}]; ow.allow != view || ow.deny&send == 0 {
		t.Fatalf("and the island's role alike: %+v", ow)
	}
	if got := auditActions(audit); !slices.Equal(got, []string{"roles.configure_jail_channels"}) {
		t.Fatalf("audit: %v", got)
	}

	set(p, panelJail+"member", nil)
	if settings.memberRole["7000001"] != "" || !rt.said("Member role cleared") {
		t.Fatalf("expected the role cleared, got %q / %v", settings.memberRole["7000001"], rt.bodies)
	}
	// Back on @everyone alone, which cannot talk at all here: Send is
	// withdrawn from the appeals room too.
	if ow := ops.overwrites[overwriteKey{"7000612", "7000301"}]; ow.allow != view || ow.deny&send == 0 {
		t.Fatalf("with @everyone unable to talk, jail may not either: %+v", ow)
	}
}

func TestListChannelsNamesTheBaseline(t *testing.T) {
	p, _, _, _, _, settings := handlerFixture()
	rt := handlerSession(t)
	showPage(p, "7000396")
	if !rt.said("**Member role**") || !rt.said("Now: @everyone") {
		t.Fatalf("got %v", rt.bodies)
	}
	settings.memberRole["7000001"] = "7000321"
	showPage(p, "7000396")
	if !rt.said("Now: <@&7000321>") {
		t.Fatalf("got %v", rt.bodies)
	}
}
