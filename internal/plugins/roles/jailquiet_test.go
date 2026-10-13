package roles

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
)

// Every permission write is one entry in the guild's own Discord audit log,
// so the tests below assert on the write *count*, not just on the resulting
// overwrites: a sync that reaches the right state by rewriting every channel
// is exactly the behaviour these skips exist to remove.

func jailQuietOps() *fakeOps {
	ops := newFakeOps()
	ops.channel["7000609"] = &fch{ID: "7000609", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	ops.channel["7000619"] = &fch{ID: "7000619", GuildID: "7000001", Type: discord.ChannelTypeGuildVoice}
	ops.channel["7000613"] = &fch{ID: "7000613", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	ops.channel["7000618"] = &fch{ID: "7000618", GuildID: "7000001", Type: discord.ChannelTypeGuildCategory}
	return ops
}

func TestSyncAllJailChannelOverwritesWritesNothingWhenAlreadyCorrect(t *testing.T) {
	ops := jailQuietOps()
	settings := newFakeSettings()
	settings.allowed["7000001"] = []string{"7000613"}
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.syncAllJailChannelOverwrites("7000001", "7000301"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	first := ops.permSetCalls
	if first != len(ops.channel) {
		t.Fatalf("expected the first sync to write every channel, got %d writes for %d channels", first, len(ops.channel))
	}

	if err := p.syncAllJailChannelOverwrites("7000001", "7000301"); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if ops.permSetCalls != first {
		t.Fatalf("expected a re-run over an unchanged guild to write nothing, got %d extra writes", ops.permSetCalls-first)
	}
}

func TestSyncAllJailChannelOverwritesWritesOnlyTheDriftedChannel(t *testing.T) {
	ops := jailQuietOps()
	settings := newFakeSettings()
	settings.allowed["7000001"] = []string{"7000613"}
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.syncAllJailChannelOverwrites("7000001", "7000301"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	first := ops.permSetCalls

	// Somebody removed the deny on one channel by hand.
	ops.channel["7000609"].PermissionOverwrites = nil

	if err := p.syncAllJailChannelOverwrites("7000001", "7000301"); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got := ops.permSetCalls - first; got != 1 {
		t.Fatalf("expected exactly the drifted channel rewritten, got %d writes", got)
	}
	ow, ok := ops.overwrites[overwriteKey{"7000609", "7000301"}]
	if !ok || ow.deny&discord.PermissionViewChannel == 0 {
		t.Fatalf("expected the drifted channel denied again, got %+v (present=%v)", ow, ok)
	}
}

// Moving a channel onto the allowlist has to still write it, since its
// desired overwrite changed from deny to allow.
func TestSyncAllJailChannelOverwritesWritesWhenAllowlistChanges(t *testing.T) {
	ops := jailQuietOps()
	settings := newFakeSettings()
	p := newTestPlugin(ops, newFakeStore(), settings, newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.syncAllJailChannelOverwrites("7000001", "7000301"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	first := ops.permSetCalls

	settings.allowed["7000001"] = []string{"7000609"}
	if err := p.syncAllJailChannelOverwrites("7000001", "7000301"); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got := ops.permSetCalls - first; got != 1 {
		t.Fatalf("expected only the newly allowlisted channel rewritten, got %d writes", got)
	}
	ow := ops.overwrites[overwriteKey{"7000609", "7000301"}]
	if ow.allow&discord.PermissionViewChannel == 0 {
		t.Fatalf("expected the allowlisted channel to allow view, got allow=%d", ow.allow)
	}
}

func TestSyncJailChannelOverwriteSkipsAnAlreadyCorrectChannel(t *testing.T) {
	ops := newFakeOps()
	ops.channel["7000609"] = &fch{ID: "7000609", GuildID: "7000001", Type: discord.ChannelTypeGuildText}
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	if _, err := p.syncJailChannelOverwrite("7000001", "7000301", "7000609"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if ops.permSetCalls != 1 {
		t.Fatalf("expected one write to establish the deny, got %d", ops.permSetCalls)
	}
	if _, err := p.syncJailChannelOverwrite("7000001", "7000301", "7000609"); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if ops.permSetCalls != 1 {
		t.Fatalf("expected the repeat to write nothing, got %d writes total", ops.permSetCalls)
	}
}

func TestSyncMemberJailOverwritesSkipsAnAlreadyDeniedMember(t *testing.T) {
	ops := newFakeOps()
	ops.channel["7000606"] = &fch{ID: "7000606", GuildID: "7000001", Type: discord.ChannelTypeGuildText,
		PermissionOverwrites: []*fow{
			{ID: "7000316", Type: discord.PermissionOverwriteTypeRole, Allow: discord.PermissionViewChannel},
		}}
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	if err := p.syncMemberJailOverwrites("7000001", "7000102"); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first := ops.permSetCalls

	// A rejoin re-apply, or a regrant reassertion, over a member whose deny
	// is still in place.
	if err := p.syncMemberJailOverwrites("7000001", "7000102"); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if ops.permSetCalls != first {
		t.Fatalf("expected no further writes for an already-denied member, got %d extra", ops.permSetCalls-first)
	}
}

func TestClearMemberJailOverwritesSkipsChannelsTheMemberHasNoOverwriteOn(t *testing.T) {
	ops := newFakeOps()
	ops.channel["7000606"] = &fch{ID: "7000606", GuildID: "7000001", Type: discord.ChannelTypeGuildText,
		PermissionOverwrites: []*fow{
			{ID: "7000316", Type: discord.PermissionOverwriteTypeRole, Allow: discord.PermissionViewChannel},
		}}
	p := newTestPlugin(ops, newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())

	// Nobody was ever jailed here, so there is nothing to delete.
	if err := p.clearMemberJailOverwrites("7000001", "7000102"); err != nil {
		t.Fatalf("clear with nothing to clear: %v", err)
	}
	if ops.permDeleteCalls != 0 {
		t.Fatalf("expected no delete calls when the member has no overwrite, got %d", ops.permDeleteCalls)
	}

	if err := p.syncMemberJailOverwrites("7000001", "7000102"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := p.clearMemberJailOverwrites("7000001", "7000102"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if ops.permDeleteCalls != 1 {
		t.Fatalf("expected exactly one delete for the one overwrite that existed, got %d", ops.permDeleteCalls)
	}
	// And a second release attempt (a sweep racing an on-demand release) is
	// silent rather than deleting nothing, loudly.
	if err := p.clearMemberJailOverwrites("7000001", "7000102"); err != nil {
		t.Fatalf("second clear: %v", err)
	}
	if ops.permDeleteCalls != 1 {
		t.Fatalf("expected the repeat clear to delete nothing, got %d deletes total", ops.permDeleteCalls)
	}
}

func TestJailOverwriteForEachChannelKind(t *testing.T) {
	view := discord.PermissionViewChannel
	connect := discord.PermissionConnect
	send := discord.PermissionSendMessages

	cases := []struct {
		name        string
		kind        discord.ChannelType
		allowed     bool
		allow, deny discord.Permissions
	}{
		{"denied text", discord.ChannelTypeGuildText, false, 0, view},
		{"denied voice", discord.ChannelTypeGuildVoice, false, 0, view | connect},
		// Where they may go, the ways out of the room (threads, polls,
		// invites, ...) are denied alongside.
		{"allowed text", discord.ChannelTypeGuildText, true, view | send, restrictedBits},
		{"allowed voice", discord.ChannelTypeGuildVoice, true, view | connect, restrictedBits},
		// A category is never permission-checked directly; it carries the
		// superset deny so a channel created under it later inherits it.
		{"category", discord.ChannelTypeGuildCategory, false, 0, view | connect},
		{"allowlisted category stays denied", discord.ChannelTypeGuildCategory, true, 0, view | connect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allow, deny := jailOverwriteFor(tc.kind, tc.allowed)
			if allow != tc.allow || deny != tc.deny {
				t.Fatalf("jailOverwriteFor(%d, %v) = (%d, %d), want (%d, %d)", tc.kind, tc.allowed, allow, deny, tc.allow, tc.deny)
			}
		})
	}
}
