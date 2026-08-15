package roles

import (
	"context"
	"testing"
	"time"

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

	// Allowlisted: expect an explicit allow overwrite.
	owAllowed, ok := ops.overwrites[overwriteKey{"allowed1", "jail-role"}]
	if !ok {
		t.Fatal("expected an overwrite on the allowlisted channel")
	}
	if owAllowed.allow&int64(discordgo.PermissionViewChannel) == 0 || owAllowed.allow&int64(discordgo.PermissionSendMessages) == 0 {
		t.Fatalf("expected allowlisted channel to allow view+send, got allow=%d", owAllowed.allow)
	}
	if _, ok := ops.overwrites[overwriteKey{"category1", "jail-role"}]; ok {
		t.Fatal("expected categories to be skipped entirely (no cascade surprises)")
	}
}

func TestSyncAllJailChannelOverwritesAllowsSendOnNewsChannel(t *testing.T) {
	ops := newFakeOps()
	ops.channel["news1"] = &discordgo.Channel{ID: "news1", GuildID: "g1", Type: discordgo.ChannelTypeGuildNews}
	settings := newFakeSettings()
	settings.allowed["g1"] = []string{"news1"}

	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.syncAllJailChannelOverwrites("g1", "jail-role"); err != nil {
		t.Fatalf("syncAllJailChannelOverwrites: %v", err)
	}

	ow, ok := ops.overwrites[overwriteKey{"news1", "jail-role"}]
	if !ok {
		t.Fatal("expected an overwrite on the allowlisted news channel")
	}
	if ow.allow&int64(discordgo.PermissionViewChannel) == 0 || ow.allow&int64(discordgo.PermissionSendMessages) == 0 {
		t.Fatalf("expected allowlisted news channel to allow view+send, got allow=%d", ow.allow)
	}
}

// TestSyncAllJailChannelOverwritesClearsRemovedAllowEntry verifies a
// channel previously allowlisted, then removed from the allowlist, gets its
// stale "no overwrite" state replaced with an explicit deny, which is the whole
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
	ow, ok := ops.overwrites[overwriteKey{"ch1", "jail-role"}]
	if !ok {
		t.Fatal("expected an overwrite while allowlisted")
	}
	if ow.allow&int64(discordgo.PermissionViewChannel) == 0 || ow.allow&int64(discordgo.PermissionSendMessages) == 0 {
		t.Fatalf("expected allowlisted channel to allow view+send, got allow=%d", ow.allow)
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
	ow, ok := ops.overwrites[overwriteKey{"ch1", "jail-role"}]
	if !ok {
		t.Fatal("expected an overwrite once allowlisted")
	}
	if ow.allow&int64(discordgo.PermissionViewChannel) == 0 || ow.allow&int64(discordgo.PermissionSendMessages) == 0 {
		t.Fatalf("expected allowlisted channel to allow view+send, got allow=%d", ow.allow)
	}
}

// The member-overwrite hardening exists because Discord resolves conflicting
// role-tier overwrites by applying every held role's deny, then every held
// role's allow, last: an allow from any other role beats the Jailed role's
// deny on the same channel regardless of role position. These tests target
// the detection that decides which channels actually need the harder,
// member-tier fix.
func TestChannelHasConflictingRoleAllow(t *testing.T) {
	deny := jailDenyFor(discordgo.ChannelTypeGuildText)
	everyoneID := "g1" // @everyone's role ID is always the guild ID.

	cases := []struct {
		name string
		ch   *discordgo.Channel
		want bool
	}{
		{
			name: "another role explicitly allows the denied permission",
			ch: &discordgo.Channel{PermissionOverwrites: []*discordgo.PermissionOverwrite{
				{ID: "access-role", Type: discordgo.PermissionOverwriteTypeRole, Allow: int64(discordgo.PermissionViewChannel)},
			}},
			want: true,
		},
		{
			name: "no overwrites at all",
			ch:   &discordgo.Channel{},
			want: false,
		},
		{
			name: "only @everyone's own overwrite",
			ch: &discordgo.Channel{PermissionOverwrites: []*discordgo.PermissionOverwrite{
				{ID: everyoneID, Type: discordgo.PermissionOverwriteTypeRole, Allow: int64(discordgo.PermissionViewChannel)},
			}},
			want: false,
		},
		{
			name: "another role only denies, never allows",
			ch: &discordgo.Channel{PermissionOverwrites: []*discordgo.PermissionOverwrite{
				{ID: "some-role", Type: discordgo.PermissionOverwriteTypeRole, Deny: int64(discordgo.PermissionViewChannel)},
			}},
			want: false,
		},
		{
			name: "another role allows a permission that isn't the denied one",
			ch: &discordgo.Channel{PermissionOverwrites: []*discordgo.PermissionOverwrite{
				{ID: "access-role", Type: discordgo.PermissionOverwriteTypeRole, Allow: int64(discordgo.PermissionSendMessages)},
			}},
			want: false,
		},
		{
			name: "an existing member-type overwrite is not a role conflict",
			ch: &discordgo.Channel{PermissionOverwrites: []*discordgo.PermissionOverwrite{
				{ID: "u1", Type: discordgo.PermissionOverwriteTypeMember, Allow: int64(discordgo.PermissionViewChannel)},
			}},
			want: false,
		},
	}
	for _, tc := range cases {
		if got := channelHasConflictingRoleAllow(tc.ch, everyoneID, deny); got != tc.want {
			t.Errorf("%s: channelHasConflictingRoleAllow = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Voice channels deny Connect too, and a role that only allows Connect
// (not View Channel) should still count as a conflict there.
func TestChannelHasConflictingRoleAllowVoiceConnect(t *testing.T) {
	deny := jailDenyFor(discordgo.ChannelTypeGuildVoice)
	ch := &discordgo.Channel{PermissionOverwrites: []*discordgo.PermissionOverwrite{
		{ID: "access-role", Type: discordgo.PermissionOverwriteTypeRole, Allow: int64(discordgo.PermissionVoiceConnect)},
	}}
	if !channelHasConflictingRoleAllow(ch, "g1", deny) {
		t.Error("expected a role allowing Connect on a voice channel to count as a conflict")
	}
}

func TestAtRiskJailChannelsScoping(t *testing.T) {
	ops := newFakeOps()
	// at-risk: a non-allowlisted text channel with a conflicting role allow.
	ops.channel["risky"] = &discordgo.Channel{ID: "risky", GuildID: "g1", Type: discordgo.ChannelTypeGuildText,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{ID: "access-role", Type: discordgo.PermissionOverwriteTypeRole, Allow: int64(discordgo.PermissionViewChannel)},
		}}
	// not at-risk: no conflicting overwrite at all.
	ops.channel["plain"] = &discordgo.Channel{ID: "plain", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	// not at-risk: allowlisted, even though it has a conflicting-looking overwrite.
	ops.channel["allowlisted"] = &discordgo.Channel{ID: "allowlisted", GuildID: "g1", Type: discordgo.ChannelTypeGuildText,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{ID: "access-role", Type: discordgo.PermissionOverwriteTypeRole, Allow: int64(discordgo.PermissionViewChannel)},
		}}
	// not at-risk: categories are never jail-managed.
	ops.channel["cat"] = &discordgo.Channel{ID: "cat", GuildID: "g1", Type: discordgo.ChannelTypeGuildCategory,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{ID: "access-role", Type: discordgo.PermissionOverwriteTypeRole, Allow: int64(discordgo.PermissionViewChannel)},
		}}

	settings := newFakeSettings()
	settings.allowed["g1"] = []string{"allowlisted"}
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	channels, err := p.atRiskJailChannels("g1")
	if err != nil {
		t.Fatalf("atRiskJailChannels: %v", err)
	}
	if len(channels) != 1 || channels[0].ID != "risky" {
		t.Fatalf("atRiskJailChannels = %v, want only [risky]", channels)
	}
}

func TestSyncMemberJailOverwritesOnlyAtRiskChannels(t *testing.T) {
	ops := newFakeOps()
	ops.channel["risky"] = &discordgo.Channel{ID: "risky", GuildID: "g1", Type: discordgo.ChannelTypeGuildText,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{ID: "access-role", Type: discordgo.PermissionOverwriteTypeRole, Allow: int64(discordgo.PermissionViewChannel)},
		}}
	ops.channel["plain"] = &discordgo.Channel{ID: "plain", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	if err := p.syncMemberJailOverwrites("g1", "u1"); err != nil {
		t.Fatalf("syncMemberJailOverwrites: %v", err)
	}

	ow, ok := ops.overwrites[overwriteKey{"risky", "u1"}]
	if !ok {
		t.Fatal("expected a member-level deny overwrite on the at-risk channel")
	}
	if ow.deny&int64(discordgo.PermissionViewChannel) == 0 {
		t.Fatalf("expected ViewChannel denied for the member, got deny=%d", ow.deny)
	}
	if _, ok := ops.overwrites[overwriteKey{"plain", "u1"}]; ok {
		t.Error("set an unnecessary member overwrite on a channel with no conflicting role overwrite")
	}
}

func TestClearMemberJailOverwrites(t *testing.T) {
	ops := newFakeOps()
	ops.channel["risky"] = &discordgo.Channel{ID: "risky", GuildID: "g1", Type: discordgo.ChannelTypeGuildText,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{ID: "access-role", Type: discordgo.PermissionOverwriteTypeRole, Allow: int64(discordgo.PermissionViewChannel)},
		}}

	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	if err := p.syncMemberJailOverwrites("g1", "u1"); err != nil {
		t.Fatalf("syncMemberJailOverwrites: %v", err)
	}
	if _, ok := ops.overwrites[overwriteKey{"risky", "u1"}]; !ok {
		t.Fatal("setup: expected the member overwrite to exist before clearing")
	}

	if err := p.clearMemberJailOverwrites("g1", "u1"); err != nil {
		t.Fatalf("clearMemberJailOverwrites: %v", err)
	}
	if _, ok := ops.overwrites[overwriteKey{"risky", "u1"}]; ok {
		t.Error("member overwrite still present after clearMemberJailOverwrites")
	}
}

// End-to-end: this is the actual bug report. A member holds an
// onboarding-style access role whose own channel overwrite explicitly
// allows View Channel; jailing them must not leave that channel visible
// just because the access role's allow would otherwise beat the Jailed
// role's deny.
func TestApplyJailAddsMemberOverwriteOnConflictingAccessRoleChannel(t *testing.T) {
	ops := newFakeOps()
	ops.channel["gated"] = &discordgo.Channel{ID: "gated", GuildID: "g1", Type: discordgo.ChannelTypeGuildText,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{ID: "access-role", Type: discordgo.PermissionOverwriteTypeRole, Allow: int64(discordgo.PermissionViewChannel)},
		}}
	ops.setMember("g1", "u1", []string{"access-role"})

	p := newEvasionPlugin(t, ops, newFakeStore())
	if _, err := p.applyJail(context.Background(), "g1", "u1", "jail-role", []string{"access-role"}, time.Hour, "mod1", "test"); err != nil {
		t.Fatalf("applyJail: %v", err)
	}

	ow, ok := ops.overwrites[overwriteKey{"gated", "u1"}]
	if !ok {
		t.Fatal("expected a member-level deny on the channel the access role could otherwise unlock")
	}
	if ow.deny&int64(discordgo.PermissionViewChannel) == 0 {
		t.Fatalf("expected ViewChannel denied for the member, got deny=%d", ow.deny)
	}
}
