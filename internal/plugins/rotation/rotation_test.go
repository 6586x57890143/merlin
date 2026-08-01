package rotation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/settings"
)

func retentionDaysPtr(d int) *int { return &d }

func finiteRetentionRC() settings.RotationChannel {
	return settings.RotationChannel{
		GuildID: "g1", ChannelID: "old1", IntervalHours: 24,
		ArchiveCategoryID: "archivecat", ArchiveVisibility: "mod_only",
		RetentionDays: retentionDaysPtr(7),
		StickyEnabled: true, StickyMessages: []string{"hello!"},
	}
}

func whitelistVisibilityRC() settings.RotationChannel {
	return settings.RotationChannel{
		GuildID: "g1", ChannelID: "old1", IntervalHours: 24,
		ArchiveCategoryID: "archivecat", ArchiveVisibility: "whitelist",
		ArchiveWhitelistRoleIDs: []string{"vip-role"}, ArchiveWhitelistUserIDs: []string{"vip-user"},
		RetentionDays: retentionDaysPtr(7),
	}
}

func foreverRetentionRC() settings.RotationChannel {
	return settings.RotationChannel{
		GuildID: "g1", ChannelID: "old1", IntervalHours: 24,
		ArchiveCategoryID: "archivecat", ArchiveVisibility: "mod_only",
	}
}

var fixedNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func setupRotation(t *testing.T, rc settings.RotationChannel) (*fakeOps, *fakeArchiveStore, *fakeAudit, *Plugin, settings.RotationChannel) {
	t.Helper()
	fs := newFakeSettings()
	fs.modRoles["g1"] = []string{"modrole1"}
	_ = fs.UpsertRotationChannel(context.Background(), rc)

	ops := newFakeOps()
	ops.addChannel(&discordgo.Channel{
		ID:       "old1",
		GuildID:  "g1",
		Name:     "general-chat",
		Topic:    "chat here",
		Position: 3,
		ParentID: "cat0",
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{ID: "g1", Type: discordgo.PermissionOverwriteTypeRole, Allow: discordgo.PermissionViewChannel},
		},
	})

	archives := newFakeArchiveStore()
	audit := &fakeAudit{}
	p := newTestPlugin(ops, archives, audit, fs, fixedNow)
	return ops, archives, audit, p, rc
}

func TestRotateFullCycle(t *testing.T) {
	ops, archives, audit, p, rc := setupRotation(t, finiteRetentionRC())

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	oldCh, err := ops.Channel("old1")
	if err != nil {
		t.Fatalf("Channel(old1): %v", err)
	}
	if oldCh.ParentID != "archivecat" {
		t.Fatalf("expected old channel moved to archivecat, got %s", oldCh.ParentID)
	}
	if !strings.Contains(oldCh.Name, "general-chat-archive-") {
		t.Fatalf("expected archive name, got %s", oldCh.Name)
	}
	foundDeny := false
	for _, ow := range oldCh.PermissionOverwrites {
		if ow.ID == "g1" && ow.Deny&discordgo.PermissionViewChannel != 0 {
			foundDeny = true
		}
	}
	if !foundDeny {
		t.Fatal("expected @everyone denied on the archived channel")
	}
	foundBotAccess := false
	for _, ow := range oldCh.PermissionOverwrites {
		if ow.ID == "bot-user-id" && ow.Allow&discordgo.PermissionViewChannel != 0 {
			foundBotAccess = true
		}
	}
	if !foundBotAccess {
		t.Fatal("expected the bot itself to retain VIEW_CHANNEL on the archived channel it just denied @everyone on — " +
			"regression test for the bug where the bot locked itself out of channels it created/archived")
	}

	channels, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	newCh := findOtherChannelByName(channels, "general-chat", "old1")
	if newCh == nil {
		t.Fatal("expected a new channel named general-chat")
	}
	everyoneAllowed := false
	for _, ow := range newCh.PermissionOverwrites {
		if ow.ID == "g1" && ow.Allow&discordgo.PermissionViewChannel != 0 {
			everyoneAllowed = true
		}
	}
	if !everyoneAllowed {
		t.Fatal("expected the revealed new channel to restore @everyone's original view access")
	}

	msgs, err := ops.ChannelMessages(newCh.ID, 10, "", "", "")
	if err != nil {
		t.Fatalf("ChannelMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (sticky + notice) in the new channel, got %d", len(msgs))
	}

	due, err := archives.DueForDeletion(context.Background(), "g1", fixedNow.AddDate(0, 0, 8))
	if err != nil {
		t.Fatalf("DueForDeletion: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 due archive 8 days after a 7-day retention rotation, got %d", len(due))
	}

	if len(audit.records) != 1 || audit.records[0].action != "channel.rotated" {
		t.Fatalf("expected 1 channel.rotated audit record, got %+v", audit.records)
	}
}

// TestRotateSucceedsDespiteAuditFailure guards against a real bug fixed
// alongside this test: rotation's own steps (rename, archive, new channel,
// stickies, archive record) had already fully succeeded by the time audit
// posting ran, so an audit-embed failure (e.g. #bot-audit-log not yet
// configured, or deleted) must not make the Scheduler treat this job as
// failed — that would trigger pointless retries of an already-complete
// rotation and eventually a false failure alert, masking that rotation
// itself is fine. Every other audit call site in this codebase already
// logs-and-continues; rotate must too.
func TestRotateSucceedsDespiteAuditFailure(t *testing.T) {
	ops, _, audit, p, rc := setupRotation(t, finiteRetentionRC())
	audit.failErr = errors.New("audit channel deleted")

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate should succeed even when audit posting fails, got: %v", err)
	}

	channels, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	if findOtherChannelByName(channels, "general-chat", "old1") == nil {
		t.Fatal("expected the rotation to have completed (new channel present) despite the audit failure")
	}
}

func TestRotateWhitelistVisibilityGrantsExtraAccess(t *testing.T) {
	ops, _, _, p, rc := setupRotation(t, whitelistVisibilityRC())

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	oldCh, err := ops.Channel("old1")
	if err != nil {
		t.Fatalf("Channel(old1): %v", err)
	}

	var roleAllowed, userAllowed, modAllowed, everyoneDenied bool
	for _, ow := range oldCh.PermissionOverwrites {
		switch {
		case ow.ID == "g1" && ow.Deny&discordgo.PermissionViewChannel != 0:
			everyoneDenied = true
		case ow.ID == "modrole1" && ow.Allow&discordgo.PermissionViewChannel != 0:
			modAllowed = true
		case ow.ID == "vip-role" && ow.Type == discordgo.PermissionOverwriteTypeRole && ow.Allow&discordgo.PermissionViewChannel != 0:
			roleAllowed = true
		case ow.ID == "vip-user" && ow.Type == discordgo.PermissionOverwriteTypeMember && ow.Allow&discordgo.PermissionViewChannel != 0:
			userAllowed = true
		}
	}
	if !everyoneDenied {
		t.Error("expected @everyone denied on the whitelist archive")
	}
	if !modAllowed {
		t.Error("expected mod roles to always retain access under whitelist visibility")
	}
	if !roleAllowed {
		t.Error("expected the whitelisted role to be granted access")
	}
	if !userAllowed {
		t.Error("expected the whitelisted user to be granted access")
	}
}

func TestRotateForeverRetentionNeverDue(t *testing.T) {
	_, archives, _, p, rc := setupRotation(t, foreverRetentionRC())

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	due, err := archives.DueForDeletion(context.Background(), "g1", fixedNow.AddDate(10, 0, 0))
	if err != nil {
		t.Fatalf("DueForDeletion: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("expected a forever-retention archive to never be due, got %d due rows", len(due))
	}
}

func TestRotateFailureLeavesNoLaterStepApplied(t *testing.T) {
	tests := []struct {
		name       string
		failMethod string
		failOnCall int
	}{
		{"fetch old channel fails", "Channel", 1},
		{"list channels fails", "GuildChannels", 1},
		{"create staging channel fails", "GuildChannelCreateComplex", 1},
		{"post sticky message fails", "ChannelMessageSend", 1},
		{"reveal new channel fails", "ChannelEditComplex", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops, archives, audit, p, rc := setupRotation(t, finiteRetentionRC())
			ops.failOnCall[tt.failMethod] = tt.failOnCall

			err := p.rotate(context.Background(), "g1", rc)
			if err == nil {
				t.Fatal("expected rotate to return an error")
			}

			oldCh, chErr := ops.Channel("old1")
			if chErr != nil {
				t.Fatalf("Channel(old1): %v", chErr)
			}
			if oldCh.Name != "general-chat" || oldCh.ParentID != "cat0" {
				t.Fatalf("old channel must be untouched on this failure, got name=%s parent=%s", oldCh.Name, oldCh.ParentID)
			}
			if len(archives.records) != 0 {
				t.Fatalf("expected no archive record on this failure, got %d", len(archives.records))
			}
			if len(audit.records) != 0 {
				t.Fatalf("expected no audit record on this failure, got %d", len(audit.records))
			}
		})
	}
}

func TestRotateFailureArchivingOldLeavesDualVisibleWindow(t *testing.T) {
	ops, archives, audit, p, rc := setupRotation(t, finiteRetentionRC())
	// The 2nd ChannelEditComplex call is the "archive old" step (the 1st is
	// "reveal new"). This is the accepted trade-off from the Milestone 3
	// design: a brief window where both channels are visible under the
	// same name, rather than any zero-live-channel outage.
	ops.failOnCall["ChannelEditComplex"] = 2

	err := p.rotate(context.Background(), "g1", rc)
	if err == nil {
		t.Fatal("expected rotate to return an error")
	}

	oldCh, chErr := ops.Channel("old1")
	if chErr != nil {
		t.Fatalf("Channel(old1): %v", chErr)
	}
	if oldCh.Name != "general-chat" {
		t.Fatalf("expected old channel NOT yet archived (still named general-chat), got %s", oldCh.Name)
	}

	channels, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	newCh := findOtherChannelByName(channels, "general-chat", "old1")
	if newCh == nil {
		t.Fatal("expected the new channel to already be revealed as general-chat (the accepted dual-visible window)")
	}

	if len(archives.records) != 0 {
		t.Fatalf("expected no archive record yet, got %d", len(archives.records))
	}
	if len(audit.records) != 0 {
		t.Fatalf("expected no audit record yet, got %d", len(audit.records))
	}
}

func TestRotateRetryAfterArchiveFailureIsIdempotent(t *testing.T) {
	ops, archives, audit, p, rc := setupRotation(t, finiteRetentionRC())
	ops.failOnCall["ChannelEditComplex"] = 2 // fail archiving old on the first attempt only

	if err := p.rotate(context.Background(), "g1", rc); err == nil {
		t.Fatal("expected first rotate attempt to fail")
	}

	// Retry: should NOT create a second staging/replacement channel, should
	// NOT re-post sticky messages, and should finish archiving old1.
	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("retry rotate: %v", err)
	}

	channels, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	var generalChatCount int
	for _, c := range channels {
		if c.Name == "general-chat" {
			generalChatCount++
		}
	}
	if generalChatCount != 1 {
		t.Fatalf("expected exactly 1 channel named general-chat after retry, got %d", generalChatCount)
	}

	oldCh, err := ops.Channel("old1")
	if err != nil {
		t.Fatalf("Channel(old1): %v", err)
	}
	if !strings.Contains(oldCh.Name, "general-chat-archive-") {
		t.Fatalf("expected old channel archived after retry, got name %s", oldCh.Name)
	}

	newCh := findOtherChannelByName(channels, "general-chat", "old1")
	if newCh == nil {
		t.Fatal("expected the revealed replacement channel to still exist")
	}
	msgs, err := ops.ChannelMessages(newCh.ID, 10, "", "", "")
	if err != nil {
		t.Fatalf("ChannelMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected exactly 2 messages (no duplicate sticky/notice from the retry), got %d", len(msgs))
	}

	if len(archives.records) != 1 {
		t.Fatalf("expected exactly 1 archive record after retry, got %d", len(archives.records))
	}
	if len(audit.records) != 1 {
		t.Fatalf("expected exactly 1 audit record after retry, got %d", len(audit.records))
	}
}

func TestRotateChannelCapPreflightBlocksRotation(t *testing.T) {
	ops, _, _, p, rc := setupRotation(t, finiteRetentionRC())
	for i := range maxChannelsPerGuild {
		ops.addChannel(&discordgo.Channel{ID: intToID(i), GuildID: "g1", Name: intToID(i)})
	}

	if err := p.rotate(context.Background(), "g1", rc); err == nil {
		t.Fatal("expected rotation to be blocked by the channel-cap preflight")
	}
}

func intToID(i int) string {
	return "filler" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
}

// TestDenyEveryoneGrantsBotAccess is a direct, pure-function regression test
// for the bug where the bot locked itself out of the hidden staging channel
// it had just created: denying @everyone VIEW_CHANNEL with no counteracting
// overwrite for the bot itself meant the bot inherited that deny too, since
// its own role deliberately carries no guild-wide Administrator bit.
func TestDenyEveryoneGrantsBotAccess(t *testing.T) {
	out := denyEveryone(nil, "g1", "bot-user-id")

	var everyoneDenied, botAllowed bool
	for _, ow := range out {
		if ow.ID == "g1" && ow.Type == discordgo.PermissionOverwriteTypeRole && ow.Deny&discordgo.PermissionViewChannel != 0 {
			everyoneDenied = true
		}
		if ow.ID == "bot-user-id" && ow.Type == discordgo.PermissionOverwriteTypeMember && ow.Allow&botOverwriteAllow == botOverwriteAllow {
			botAllowed = true
		}
	}
	if !everyoneDenied {
		t.Fatal("expected @everyone to be denied VIEW_CHANNEL")
	}
	if !botAllowed {
		t.Fatal("expected the bot to be explicitly granted view/send/manage-messages on the channel it's creating")
	}
}

// TestDenyEveryoneMergesExistingBotOverwrite guards against a second bug the
// naive fix could introduce: if the source channel already had a member
// overwrite for the bot (however unlikely), appending a second one for the
// same ID+Type would produce a duplicate entry Discord's API would reject.
func TestDenyEveryoneMergesExistingBotOverwrite(t *testing.T) {
	src := []*discordgo.PermissionOverwrite{
		{ID: "bot-user-id", Type: discordgo.PermissionOverwriteTypeMember, Deny: discordgo.PermissionSendMessages},
	}
	out := denyEveryone(src, "g1", "bot-user-id")

	count := 0
	for _, ow := range out {
		if ow.ID == "bot-user-id" && ow.Type == discordgo.PermissionOverwriteTypeMember {
			count++
			if ow.Allow&botOverwriteAllow != botOverwriteAllow {
				t.Fatalf("expected the merged overwrite to grant the full bot allow set, got Allow=%d", ow.Allow)
			}
			if ow.Deny&discordgo.PermissionSendMessages != 0 {
				t.Fatal("expected the merge to clear the pre-existing deny it's overriding")
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one bot overwrite entry after merging, got %d", count)
	}
}

func TestArchiveOverwritesGrantsBotViewAccess(t *testing.T) {
	out := archiveOverwrites("g1", "bot-user-id", nil, finiteRetentionRC())

	found := false
	for _, ow := range out {
		if ow.ID == "bot-user-id" && ow.Type == discordgo.PermissionOverwriteTypeMember && ow.Allow&discordgo.PermissionViewChannel != 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the bot to retain VIEW_CHANNEL on the archived channel (needed later by sweep.go)")
	}
}
