package roles

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestSyncAllJailChannelOverwritesDeniesByDefaultAllowsListed(t *testing.T) {
	ops := newFakeOps()
	ops.channel["text1"] = &discordgo.Channel{ID: "text1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	ops.channel["voice1"] = &discordgo.Channel{ID: "voice1", GuildID: "g1", Type: discordgo.ChannelTypeGuildVoice}
	ops.channel["allowed1"] = &discordgo.Channel{ID: "allowed1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	ops.channel["category1"] = &discordgo.Channel{ID: "category1", GuildID: "g1", Type: discordgo.ChannelTypeGuildCategory}

	settings := newFakeSettings()
	settings.allowed["g1"] = []string{"allowed1"}

	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.syncAllJailChannelOverwrites("g1", "jail-role"); err != nil {
		t.Fatalf("syncAllJailChannelOverwrites: %v", err)
	}

	textOverwrite, ok := ops.overwrites[overwriteKey{"text1", "jail-role"}]
	if !ok {
		t.Fatal("expected a deny overwrite on text1")
	}
	if textOverwrite.deny&int64(discordgo.PermissionViewChannel) == 0 {
		t.Fatalf("expected ViewChannel denied on text1, got deny=%d", textOverwrite.deny)
	}

	voiceOverwrite, ok := ops.overwrites[overwriteKey{"voice1", "jail-role"}]
	if !ok {
		t.Fatal("expected a deny overwrite on voice1")
	}
	if voiceOverwrite.deny&int64(discordgo.PermissionVoiceConnect) == 0 {
		t.Fatalf("expected Connect denied on voice1 (voice channel), got deny=%d", voiceOverwrite.deny)
	}

	if _, ok := ops.overwrites[overwriteKey{"allowed1", "jail-role"}]; ok {
		t.Fatal("expected no overwrite on the allowlisted channel")
	}
	if _, ok := ops.overwrites[overwriteKey{"category1", "jail-role"}]; ok {
		t.Fatal("expected categories to be skipped entirely (no cascade surprises)")
	}
}

// TestSyncAllJailChannelOverwritesClearsRemovedAllowEntry verifies a
// channel previously allowlisted, then removed from the allowlist, gets its
// stale "no overwrite" state replaced with an explicit deny — the whole
// point of sync-channels being re-runnable.
func TestSyncAllJailChannelOverwritesClearsRemovedAllowEntry(t *testing.T) {
	ops := newFakeOps()
	ops.channel["ch1"] = &discordgo.Channel{ID: "ch1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	settings := newFakeSettings()
	settings.allowed["g1"] = []string{"ch1"}

	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())
	if err := p.syncAllJailChannelOverwrites("g1", "jail-role"); err != nil {
		t.Fatalf("syncAllJailChannelOverwrites: %v", err)
	}
	if _, ok := ops.overwrites[overwriteKey{"ch1", "jail-role"}]; ok {
		t.Fatal("expected no overwrite while allowlisted")
	}

	settings.allowed["g1"] = nil
	if err := p.syncAllJailChannelOverwrites("g1", "jail-role"); err != nil {
		t.Fatalf("syncAllJailChannelOverwrites (2nd): %v", err)
	}
	if _, ok := ops.overwrites[overwriteKey{"ch1", "jail-role"}]; !ok {
		t.Fatal("expected a deny overwrite after removal from allowlist")
	}
}

func TestSyncJailChannelOverwriteSingleChannel(t *testing.T) {
	ops := newFakeOps()
	ops.channel["ch1"] = &discordgo.Channel{ID: "ch1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	settings := newFakeSettings()

	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.syncJailChannelOverwrite("g1", "jail-role", "ch1"); err != nil {
		t.Fatalf("syncJailChannelOverwrite: %v", err)
	}
	if _, ok := ops.overwrites[overwriteKey{"ch1", "jail-role"}]; !ok {
		t.Fatal("expected deny overwrite for a non-allowlisted channel")
	}

	settings.allowed["g1"] = []string{"ch1"}
	if err := p.syncJailChannelOverwrite("g1", "jail-role", "ch1"); err != nil {
		t.Fatalf("syncJailChannelOverwrite (allowed): %v", err)
	}
	if _, ok := ops.overwrites[overwriteKey{"ch1", "jail-role"}]; ok {
		t.Fatal("expected overwrite cleared once allowlisted")
	}
}
