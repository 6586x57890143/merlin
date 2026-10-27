package roles

import (
	"context"
	"errors"
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
	settings.conversation["7000001"] = []string{"7000613"}

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
	settings.conversation["7000001"] = []string{"7000620"}

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
	settings.conversation["7000001"] = []string{"7000605"}

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

	settings.conversation["7000001"] = nil
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

	settings.conversation["7000001"] = []string{"7000605"}
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
	_ = p.store.InsertJail(context.Background(), rec)
	recordOwnedOverwrite(p, ops, "7000611", "7000102")
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

func lockedRoom(id string, typ discord.ChannelType, overwrites ...*fow) *fch {
	return &fch{ID: id, GuildID: "7000001", Type: typ, PermissionOverwrites: overwrites}
}

func TestExplicitAccessModesRepairStalePermissions(t *testing.T) {
	view, send, history := discord.PermissionViewChannel, discord.PermissionSendMessages, discord.PermissionReadMessageHistory
	ch := lockedRoom("7000690", discord.ChannelTypeGuildText,
		&fow{ID: "7000001", Type: discord.PermissionOverwriteTypeRole, Deny: view | send | history},
		&fow{ID: "7000396", Type: discord.PermissionOverwriteTypeRole, Deny: send | history | discord.PermissionManageMessages})
	allow, deny, needed := desiredOverwrite(ch.Disgo(), "7000396", accessConversation)
	if !needed || allow&(view|send|history) != view|send|history || deny&(view|send|history) != 0 || deny&discord.PermissionManageMessages == 0 {
		t.Fatalf("conversation did not repair access while preserving unrelated deny: allow=%d deny=%d", allow, deny)
	}
	ch.PermissionOverwrites[1].Allow, ch.PermissionOverwrites[1].Deny = allow, deny
	if _, _, needed := desiredOverwrite(ch.Disgo(), "7000396", accessConversation); needed {
		t.Fatal("repeat sync wrote unchanged permissions")
	}
	allow, deny, _ = desiredOverwrite(ch.Disgo(), "7000396", accessReadOnly)
	if allow&(view|history) != view|history || allow&send != 0 || deny&send == 0 {
		t.Fatalf("read-only: allow=%d deny=%d", allow, deny)
	}
	ch.PermissionOverwrites[1].Allow, ch.PermissionOverwrites[1].Deny = allow, deny
	allow, deny, needed = markerOverwrite("7000001", ch.Disgo(), "7000396", accessHidden)
	if !needed || allow&view != 0 || deny&view == 0 {
		t.Fatal("removing private channel preserved marker view allow")
	}
}

func TestConversationCannotOpenForumPosting(t *testing.T) {
	for _, typ := range []discord.ChannelType{discord.ChannelTypeGuildForum, discord.ChannelTypeGuildMedia, discord.ChannelTypeGuildStageVoice} {
		allow, deny := jailOverwriteFor(typ, accessConversation)
		if allow&discord.PermissionSendMessages != 0 || deny&discord.PermissionSendMessages == 0 {
			t.Fatalf("type %d permits posts", typ)
		}
		if allow&discord.PermissionReadMessageHistory == 0 {
			t.Fatalf("type %d lacks history", typ)
		}
	}
	allow, deny := jailOverwriteFor(discord.ChannelTypeGuildVoice, accessConversation)
	voice := discord.PermissionViewChannel | discord.PermissionReadMessageHistory | discord.PermissionSendMessages | discord.PermissionConnect | discord.PermissionSpeak
	if allow&voice != voice || deny&voice != 0 {
		t.Fatal("voice conversation missing access")
	}
	_, deny = jailOverwriteFor(discord.ChannelTypeGuildVoice, accessReadOnly)
	if deny&discord.PermissionConnect == 0 {
		t.Fatal("read-only allows voice participation")
	}
}

func TestSettingsMoveChannelBetweenAccessModes(t *testing.T) {
	p, ops, _, _, _, settings := handlerFixture()
	rt := handlerSession(t)
	// Cold cache, default marker already exists, @everyone denies the entire
	// room. Explicit conversation must not depend on the member baseline.
	p.jailRoleID = make(map[string]string)
	ops.channel["7000612"] = lockedRoom("7000612", discord.ChannelTypeGuildText,
		&fow{ID: "7000001", Type: discord.PermissionOverwriteTypeRole, Deny: discord.PermissionViewChannel | discord.PermissionSendMessages})
	set(p, panelJail+"conversation", nil, "7000612")
	if !rt.said("conversation access") {
		t.Fatalf("response: %v", rt.bodies)
	}
	ow := ops.overwrites[overwriteKey{"7000612", "7000301"}]
	if ow.allow&discord.PermissionSendMessages == 0 {
		t.Fatal("cold-cache conversation did not allow sending")
	}
	set(p, panelJail+"allow", nil, "7000612")
	if len(settings.JailConversationChannelIDs("7000001")) != 0 {
		t.Fatal("channel in both lists")
	}
	ow = ops.overwrites[overwriteKey{"7000612", "7000301"}]
	if ow.deny&discord.PermissionSendMessages == 0 || ow.allow&discord.PermissionReadMessageHistory == 0 {
		t.Fatal("read-only did not repair permissions")
	}
	set(p, panelJail+"allow", []string{"7000612"})
	ow = ops.overwrites[overwriteKey{"7000612", "7000301"}]
	if ow.allow&discord.PermissionViewChannel != 0 {
		t.Fatal("removed channel still visible")
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
