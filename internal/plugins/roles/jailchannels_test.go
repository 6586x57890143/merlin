package roles

import (
	"context"
	"slices"
	"strings"
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
	// Categories get a plain deny too, so a channel created under one later
	// starts denied by default (Discord copies a category's overwrites onto
	// a channel created with none of its own) rather than visible until the
	// next sync-channels run notices it. This has no effect on existing
	// channels: Discord's live permission check never consults a channel's
	// parent category.
	categoryOverwrite, ok := ops.overwrites[overwriteKey{"category1", "jail-role"}]
	if !ok {
		t.Fatal("expected a deny overwrite on the category, for channels created under it later")
	}
	if categoryOverwrite.deny&int64(discordgo.PermissionViewChannel) == 0 || categoryOverwrite.deny&int64(discordgo.PermissionVoiceConnect) == 0 {
		t.Fatalf("expected the category deny to cover both ViewChannel and Connect (any future child type), got deny=%d", categoryOverwrite.deny)
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

	if _, err := p.syncJailChannelOverwrite("g1", "jail-role", "ch1"); err != nil {
		t.Fatalf("syncJailChannelOverwrite: %v", err)
	}
	if _, ok := ops.overwrites[overwriteKey{"ch1", "jail-role"}]; !ok {
		t.Fatal("expected deny overwrite for a non-allowlisted channel")
	}

	settings.allowed["g1"] = []string{"ch1"}
	if _, err := p.syncJailChannelOverwrite("g1", "jail-role", "ch1"); err != nil {
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

// The regular jail path stays pure role-based, deliberately: member-level
// overwrites cost one write per at-risk channel, and an ordinary jail that
// nobody is evading shouldn't pay for hardening it doesn't need. Even when a
// conflicting access-role overwrite exists, applyJail alone must not touch
// channel overwrites at all.
func TestApplyJailDoesNotSetMemberOverwrites(t *testing.T) {
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

	if _, ok := ops.overwrites[overwriteKey{"gated", "u1"}]; ok {
		t.Error("applyJail set a member-level overwrite; that hardening belongs to the evasion routine only")
	}
}

// End-to-end: this is the actual bug report. A member rejoins (or has a
// role regranted by a guild's Onboarding/Membership Screening flow) while
// jailed, holding an access role whose own channel overwrite explicitly
// allows View Channel. reapplyIfEvaded is the evasion routine, so unlike an
// ordinary jail it must add the member-level deny that actually stops that
// role from beating the Jailed role's own channel-level deny.
func TestReapplyIfEvadedSetsMemberOverwriteOnConflictingAccessRoleChannel(t *testing.T) {
	ops := newFakeOps()
	ops.channel["gated"] = &discordgo.Channel{ID: "gated", GuildID: "g1", Type: discordgo.ChannelTypeGuildText,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{ID: "access-role", Type: discordgo.PermissionOverwriteTypeRole, Allow: int64(discordgo.PermissionViewChannel)},
		}}
	// Rejoined (JoinedAt after JailedAt) with the access role but no marker.
	ops.setMemberJoined("g1", "u1", []string{"access-role"}, fixedNow.Add(-time.Minute))

	store := newFakeStore()
	if err := store.InsertJail(context.Background(), activeJail("u1", []string{"role-a"})); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	p := newEvasionPlugin(t, ops, store)
	if err := p.reapplyEvadedJails(context.Background(), "g1"); err != nil {
		t.Fatalf("reapplyEvadedJails: %v", err)
	}

	ow, ok := ops.overwrites[overwriteKey{"gated", "u1"}]
	if !ok {
		t.Fatal("expected a member-level deny on the channel the access role could otherwise unlock")
	}
	if ow.deny&int64(discordgo.PermissionViewChannel) == 0 {
		t.Fatalf("expected ViewChannel denied for the member, got deny=%d", ow.deny)
	}
}

// Same bug report, live-event path: HandleMemberUpdate is the other half of
// the evasion routine and must set the same hardening.
func TestHandleMemberUpdateSetsMemberOverwriteOnConflictingAccessRoleChannel(t *testing.T) {
	ops := newFakeOps()
	ops.channel["gated"] = &discordgo.Channel{ID: "gated", GuildID: "g1", Type: discordgo.ChannelTypeGuildText,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{ID: "access-role", Type: discordgo.PermissionOverwriteTypeRole, Allow: int64(discordgo.PermissionViewChannel)},
		}}
	ops.setMemberJoined("g1", "u1", []string{"jail-role", "access-role"}, jailedAt.Add(-24*time.Hour))

	store := newFakeStore()
	if err := store.InsertJail(context.Background(), activeJail("u1", []string{"role-a"})); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	p := newEvasionPlugin(t, ops, store)
	p.HandleMemberUpdate(context.Background(), "g1", "u1", []string{"jail-role", "access-role"})

	ow, ok := ops.overwrites[overwriteKey{"gated", "u1"}]
	if !ok {
		t.Fatal("expected a member-level deny on the channel the access role could otherwise unlock")
	}
	if ow.deny&int64(discordgo.PermissionViewChannel) == 0 {
		t.Fatalf("expected ViewChannel denied for the member, got deny=%d", ow.deny)
	}
}

// The baseline: a marker role is never allowed more on an allowlisted
// channel than an ordinary member (@everyone plus the configured member
// role) can do there, and what it is refused goes into its own deny so the
// cap holds however the room was locked.

func lockedRoom(id string, typ discordgo.ChannelType, overwrites ...*discordgo.PermissionOverwrite) *discordgo.Channel {
	return &discordgo.Channel{ID: id, GuildID: "g1", Type: typ, PermissionOverwrites: overwrites}
}

func TestMemberBaselineResolvesLikeDiscord(t *testing.T) {
	view, send := int64(discordgo.PermissionViewChannel), int64(discordgo.PermissionSendMessages)
	roles := []*discordgo.Role{
		{ID: "g1", Permissions: view},            // @everyone: can see, cannot talk
		{ID: "melted", Permissions: view | send}, // members: can talk
		{ID: "admin", Permissions: discordgo.PermissionAdministrator},
	}
	plain := lockedRoom("c", discordgo.ChannelTypeGuildText)
	if got := memberBaseline("g1", "", plain, roles); got != view {
		t.Fatalf("@everyone alone = %d, want view", got)
	}
	if got := memberBaseline("g1", "melted", plain, roles); got != view|send {
		t.Fatalf("with member role = %d, want view|send", got)
	}
	// An announcements room: @everyone denied Send at channel level beats
	// the member role's guild-level Send.
	announce := lockedRoom("a", discordgo.ChannelTypeGuildText, &discordgo.PermissionOverwrite{ID: "g1", Type: discordgo.PermissionOverwriteTypeRole, Deny: send})
	if got := memberBaseline("g1", "melted", announce, roles); got != view {
		t.Fatalf("channel deny on @everyone = %d, want view", got)
	}
	// ...unless the member role's own overwrite gives it back.
	announce.PermissionOverwrites = append(announce.PermissionOverwrites, &discordgo.PermissionOverwrite{ID: "melted", Type: discordgo.PermissionOverwriteTypeRole, Allow: send})
	if got := memberBaseline("g1", "melted", announce, roles); got != view|send {
		t.Fatalf("member role overwrite = %d, want view|send", got)
	}
	// A room locked only on the member role.
	memberLocked := lockedRoom("m", discordgo.ChannelTypeGuildText, &discordgo.PermissionOverwrite{ID: "melted", Type: discordgo.PermissionOverwriteTypeRole, Deny: send})
	if got := memberBaseline("g1", "melted", memberLocked, roles); got != view {
		t.Fatalf("deny on member role = %d, want view", got)
	}
	if got := memberBaseline("g1", "admin", plain, roles); got != discordgo.PermissionAll {
		t.Fatal("Administrator is everything")
	}
	if got := memberBaseline("g1", "gone", plain, roles); got != view {
		t.Fatalf("a deleted member role contributes nothing, got %d", got)
	}
}

func TestDesiredOverwriteWithholdsWhatMembersLack(t *testing.T) {
	view, send, connect := int64(discordgo.PermissionViewChannel), int64(discordgo.PermissionSendMessages), int64(discordgo.PermissionVoiceConnect)

	// Fresh overwrite on a read-only room: View granted, Send refused and
	// denied outright, the ways out denied as always.
	allow, deny, withheld, needed := desiredOverwrite(lockedRoom("a", discordgo.ChannelTypeGuildText), "jail", true, view)
	if !needed || allow != view || deny != restrictedBits|send || withheld != send {
		t.Fatalf("read-only room: allow=%d deny=%d withheld=%d", allow, deny, withheld)
	}
	// A room members cannot even see: nothing granted, View denied.
	allow, deny, withheld, _ = desiredOverwrite(lockedRoom("h", discordgo.ChannelTypeGuildText), "jail", true, 0)
	if allow != 0 || deny&view == 0 || deny&send == 0 || withheld != view|send {
		t.Fatalf("hidden room: allow=%d deny=%d withheld=%d", allow, deny, withheld)
	}
	// A locked voice room withholds Connect.
	_, deny, withheld, _ = desiredOverwrite(lockedRoom("v", discordgo.ChannelTypeGuildVoice), "jail", true, view)
	if withheld != connect || deny&connect == 0 {
		t.Fatalf("locked voice: deny=%d withheld=%d", deny, withheld)
	}
	// An escalation an earlier sync already wrote is repaired: Send comes
	// out of allow and goes into deny, everything else the guild set stays.
	escalated := lockedRoom("a", discordgo.ChannelTypeGuildText, &discordgo.PermissionOverwrite{ID: "jail", Type: discordgo.PermissionOverwriteTypeRole,
		Allow: view | send | int64(discordgo.PermissionAddReactions), Deny: restrictedBits})
	allow, deny, withheld, needed = desiredOverwrite(escalated, "jail", true, view)
	if !needed || allow != view|int64(discordgo.PermissionAddReactions) || deny != restrictedBits|send || withheld != send {
		t.Fatalf("repair: allow=%d deny=%d withheld=%d needed=%v", allow, deny, withheld, needed)
	}
	// And once repaired, nothing more to write.
	escalated.PermissionOverwrites[0].Allow, escalated.PermissionOverwrites[0].Deny = allow, deny
	if _, _, _, needed = desiredOverwrite(escalated, "jail", true, view); needed {
		t.Fatal("a repaired overwrite must not be rewritten")
	}
	// Off the allowlist the baseline is irrelevant.
	allow, deny, withheld, _ = desiredOverwrite(lockedRoom("o", discordgo.ChannelTypeGuildText), "jail", false, 0)
	if allow != 0 || deny != view || withheld != 0 {
		t.Fatalf("off-list: allow=%d deny=%d withheld=%d", allow, deny, withheld)
	}
	// A category is never capped: its overwrite is only the template a new
	// channel inherits.
	if _, _, withheld, _ = desiredOverwrite(lockedRoom("cat", discordgo.ChannelTypeGuildCategory), "jail", true, 0); withheld != 0 {
		t.Fatal("categories carry no allow to withhold")
	}
}

// TestAllowChannelWarnsWhenMembersCannotPost: the live bug. Allowlisting
// an announcements room for the jail marker used to hand jailed members
// Send there; now they get View, an explicit Send deny, a warning naming
// it, and an audit line saying so.
func TestAllowChannelWarnsWhenMembersCannotPost(t *testing.T) {
	view, send := int64(discordgo.PermissionViewChannel), int64(discordgo.PermissionSendMessages)
	p, ops, _, audit, _, settings := handlerFixture()
	settings.memberRole["g1"] = "melted"
	ops.roles["g1"] = append(ops.roles["g1"], &discordgo.Role{ID: "melted", Permissions: everyonePerms})
	ops.channel["announce"] = lockedRoom("announce", discordgo.ChannelTypeGuildText,
		&discordgo.PermissionOverwrite{ID: "g1", Type: discordgo.PermissionOverwriteTypeRole, Deny: send})
	ops.channel["appeals"] = lockedRoom("appeals", discordgo.ChannelTypeGuildText)
	p.jailRoleID["g1"] = "jail-role"
	s, rt := handlerSession(t)

	p.handleAllowChannel(context.Background(), s, rolesInteraction("configure", "allow-channel", channelArg("channel", "announce")))

	ow := ops.overwrites[overwriteKey{"announce", "jail-role"}]
	if ow.allow != view || ow.deny != restrictedBits|send {
		t.Fatalf("expected view only with Send denied, got %+v", ow)
	}
	if !rt.said("Channel allowed, with limits") || !rt.said("no Send Messages there") || !rt.said("<@&melted>") {
		t.Fatalf("expected the warning naming Send and the baseline role, got %v", rt.bodies)
	}
	if len(audit.records) != 1 || !strings.Contains(audit.records[0].newValue, "withheld=Send Messages") {
		t.Fatalf("audit: %+v", audit.records)
	}

	// The appeals room is untouched by the cap: members can talk there.
	p.handleAllowChannel(context.Background(), s, rolesInteraction("configure", "allow-channel", channelArg("channel", "appeals")))
	if ow := ops.overwrites[overwriteKey{"appeals", "jail-role"}]; ow.allow != view|send {
		t.Fatalf("expected view+send in the appeals room, got %+v", ow)
	}
	if !rt.said("<#appeals> will stay visible to jailed members.") {
		t.Fatalf("expected a plain success, got %v", rt.bodies)
	}
}

// TestAllowChannelWarnsWhenMembersCannotSee: a room ordinary members
// cannot see at all is refused outright and the admin is told the entry
// does nothing.
func TestAllowChannelWarnsWhenMembersCannotSee(t *testing.T) {
	view := int64(discordgo.PermissionViewChannel)
	p, ops, _, _, _, _ := handlerFixture()
	ops.channel["staff"] = lockedRoom("staff", discordgo.ChannelTypeGuildText,
		&discordgo.PermissionOverwrite{ID: "g1", Type: discordgo.PermissionOverwriteTypeRole, Deny: view})
	p.jailRoleID["g1"] = "jail-role"
	s, rt := handlerSession(t)

	p.handleAllowChannel(context.Background(), s, rolesInteraction("configure", "allow-channel", channelArg("channel", "staff")))

	if ow := ops.overwrites[overwriteKey{"staff", "jail-role"}]; ow.allow != 0 || ow.deny&view == 0 {
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
	view, send := int64(discordgo.PermissionViewChannel), int64(discordgo.PermissionSendMessages)
	p, ops, _, audit, _, settings := handlerFixture()
	// @everyone cannot talk anywhere; melted can, except in the locked room.
	ops.roles["g1"] = append(ops.roles["g1"],
		&discordgo.Role{ID: "g1", Permissions: view},
		&discordgo.Role{ID: "melted", Permissions: view | send},
		&discordgo.Role{ID: "island", Permissions: 0})
	settings.allowed["g1"] = []string{"appeals", "locked"}
	settings.vacationRole["g1"] = "island"
	settings.vacationAllowed["g1"] = []string{"locked"}
	ops.channel["appeals"] = lockedRoom("appeals", discordgo.ChannelTypeGuildText)
	ops.channel["locked"] = lockedRoom("locked", discordgo.ChannelTypeGuildText,
		&discordgo.PermissionOverwrite{ID: "melted", Type: discordgo.PermissionOverwriteTypeRole, Deny: send})
	p.jailRoleID["g1"] = "jail-role"
	s, rt := handlerSession(t)

	p.handleMemberRole(context.Background(), s, rolesInteraction("configure", "member-role", roleArg("role", "melted")))

	if settings.memberRole["g1"] != "melted" || !rt.said("Member role set") {
		t.Fatalf("expected the role saved and confirmed, got %q / %v", settings.memberRole["g1"], rt.bodies)
	}
	if ow := ops.overwrites[overwriteKey{"appeals", "jail-role"}]; ow.allow != view|send {
		t.Fatalf("melted can talk in the appeals room, so jail may too: %+v", ow)
	}
	if ow := ops.overwrites[overwriteKey{"locked", "jail-role"}]; ow.allow != view || ow.deny&send == 0 {
		t.Fatalf("a room locked on the member role caps the jail marker: %+v", ow)
	}
	if ow := ops.overwrites[overwriteKey{"locked", "island"}]; ow.allow != view || ow.deny&send == 0 {
		t.Fatalf("and the island's role alike: %+v", ow)
	}
	if got := auditActions(audit); !slices.Equal(got, []string{"roles.configure_jail_channels"}) {
		t.Fatalf("audit: %v", got)
	}

	p.handleMemberRole(context.Background(), s, rolesInteraction("configure", "member-role"))
	if settings.memberRole["g1"] != "" || !rt.said("Member role cleared") {
		t.Fatalf("expected the role cleared, got %q / %v", settings.memberRole["g1"], rt.bodies)
	}
	// Back on @everyone alone, which cannot talk at all here: Send is
	// withdrawn from the appeals room too.
	if ow := ops.overwrites[overwriteKey{"appeals", "jail-role"}]; ow.allow != view || ow.deny&send == 0 {
		t.Fatalf("with @everyone unable to talk, jail may not either: %+v", ow)
	}
}

func TestListChannelsNamesTheBaseline(t *testing.T) {
	p, _, _, _, _, settings := handlerFixture()
	s, rt := handlerSession(t)
	p.handleListChannels(context.Background(), s, rolesInteraction("configure", "list-channels"))
	if !rt.said("Baseline for both: @everyone") {
		t.Fatalf("got %v", rt.bodies)
	}
	settings.memberRole["g1"] = "melted"
	p.handleListChannels(context.Background(), s, rolesInteraction("configure", "list-channels"))
	if !rt.said("Baseline for both: <@&melted>") {
		t.Fatalf("got %v", rt.bodies)
	}
}
