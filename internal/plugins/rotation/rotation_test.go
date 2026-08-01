package rotation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/config"
)

const guildYAMLFiniteRetention = `
guilds:
  "g1":
    guild_id: "g1"
    mod_role_ids: ["modrole1"]
    audit_log_channel_id: "audit"
    status_channel_id: "status"
    rotating_channels:
      - channel_id: "old1"
        interval_hours: 24
        archive_category_id: "archivecat"
        archive_visibility: mod_only
        retention_days: 7
        sticky:
          enabled: true
          template: welcome
sticky_templates:
  welcome:
    messages: ["hello!"]
`

const guildYAMLWhitelistVisibility = `
guilds:
  "g1":
    guild_id: "g1"
    mod_role_ids: ["modrole1"]
    audit_log_channel_id: "audit"
    status_channel_id: "status"
    rotating_channels:
      - channel_id: "old1"
        interval_hours: 24
        archive_category_id: "archivecat"
        archive_visibility: whitelist
        archive_whitelist_role_ids: ["vip-role"]
        archive_whitelist_user_ids: ["vip-user"]
        retention_days: 7
`

const guildYAMLForeverRetention = `
guilds:
  "g1":
    guild_id: "g1"
    mod_role_ids: ["modrole1"]
    audit_log_channel_id: "audit"
    status_channel_id: "status"
    rotating_channels:
      - channel_id: "old1"
        interval_hours: 24
        archive_category_id: "archivecat"
        archive_visibility: mod_only
`

var fixedNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func setupRotation(t *testing.T, yamlContents string) (*fakeOps, *fakeArchiveStore, *fakeAudit, *Plugin, config.RotationConfig) {
	t.Helper()
	cfg := newTestLoader(t, yamlContents)
	gc, err := cfg.Guild("g1")
	if err != nil {
		t.Fatalf("Guild: %v", err)
	}
	rc := gc.RotatingChannels[0]

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
	p := newTestPlugin(ops, archives, audit, cfg, fixedNow)
	return ops, archives, audit, p, rc
}

func TestRotateFullCycle(t *testing.T) {
	ops, archives, audit, p, rc := setupRotation(t, guildYAMLFiniteRetention)

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

func TestRotateWhitelistVisibilityGrantsExtraAccess(t *testing.T) {
	ops, _, _, p, rc := setupRotation(t, guildYAMLWhitelistVisibility)

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
	_, archives, _, p, rc := setupRotation(t, guildYAMLForeverRetention)

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
			ops, archives, audit, p, rc := setupRotation(t, guildYAMLFiniteRetention)
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
	ops, archives, audit, p, rc := setupRotation(t, guildYAMLFiniteRetention)
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
	ops, archives, audit, p, rc := setupRotation(t, guildYAMLFiniteRetention)
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
	ops, _, _, p, rc := setupRotation(t, guildYAMLFiniteRetention)
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
