package core

import (
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"
)

const testBotAllow = discordgo.PermissionViewChannel | discordgo.PermissionSendMessages

// TestDenyEveryoneExceptBotGrantsBotAccess is a direct regression test for
// the bug this helper exists to make structurally impossible: denying
// @everyone VIEW_CHANNEL with no counteracting overwrite for the bot itself
// locked the bot out of channels it had just created (adminconfig's setup
// channels, rotation's staging channel, and rotation's archived channel all
// hit this independently before being consolidated into this one helper).
func TestDenyEveryoneExceptBotGrantsBotAccess(t *testing.T) {
	out := DenyEveryoneExceptBot(nil, "g1", "bot-user-id", testBotAllow)

	var everyoneDenied, botAllowed bool
	for _, ow := range out {
		if ow.ID == "g1" && ow.Type == discordgo.PermissionOverwriteTypeRole && ow.Deny&discordgo.PermissionViewChannel != 0 {
			everyoneDenied = true
		}
		if ow.ID == "bot-user-id" && ow.Type == discordgo.PermissionOverwriteTypeMember && ow.Allow&testBotAllow == testBotAllow {
			botAllowed = true
		}
	}
	if !everyoneDenied {
		t.Fatal("expected @everyone to be denied VIEW_CHANNEL")
	}
	if !botAllowed {
		t.Fatal("expected the bot to be explicitly granted the given allow bits on the channel it's creating")
	}
}

// TestDenyEveryoneExceptBotMergesExistingBotOverwrite guards against a
// second bug the naive fix could introduce: if the source channel already
// had a member overwrite for the bot, appending a second one for the same
// ID+Type would produce a duplicate entry Discord's API would reject.
func TestDenyEveryoneExceptBotMergesExistingBotOverwrite(t *testing.T) {
	src := []*discordgo.PermissionOverwrite{
		{ID: "bot-user-id", Type: discordgo.PermissionOverwriteTypeMember, Deny: discordgo.PermissionSendMessages},
	}
	out := DenyEveryoneExceptBot(src, "g1", "bot-user-id", testBotAllow)

	count := 0
	for _, ow := range out {
		if ow.ID == "bot-user-id" && ow.Type == discordgo.PermissionOverwriteTypeMember {
			count++
			if ow.Allow&testBotAllow != testBotAllow {
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

// DenyEveryone is the same promise on disgo's types: @everyone denied view,
// the bot granted its bits, an existing overwrite for either merged rather
// than duplicated, and every other overwrite left as it was.
func TestDenyEveryoneOnDisgo(t *testing.T) {
	allow := discord.PermissionViewChannel | discord.PermissionSendMessages
	out := DenyEveryone([]discord.PermissionOverwrite{
		discord.MemberPermissionOverwrite{UserID: 2, Deny: discord.PermissionSendMessages},
		discord.RolePermissionOverwrite{RoleID: 100, Allow: discord.PermissionViewChannel},
		discord.RolePermissionOverwrite{RoleID: 7, Allow: discord.PermissionAttachFiles},
	}, 100, 2, allow)
	ows := discord.PermissionOverwrites(out)
	if len(out) != 3 {
		t.Fatalf("got %d overwrites, want the three merged: %+v", len(out), out)
	}
	if e, ok := ows.Role(100); !ok || !e.Deny.Has(discord.PermissionViewChannel) || e.Allow.Has(discord.PermissionViewChannel) {
		t.Fatalf("@everyone %+v", e)
	}
	if b, ok := ows.Member(2); !ok || !b.Allow.Has(allow) || b.Deny.Has(discord.PermissionSendMessages) {
		t.Fatalf("bot %+v", b)
	}
	if o, ok := ows.Role(7); !ok || o.Allow != discord.PermissionAttachFiles {
		t.Fatalf("another role's overwrite changed: %+v", o)
	}
	fresh := discord.PermissionOverwrites(DenyEveryone(nil, 100, 2, allow))
	if _, ok := fresh.Role(100); !ok {
		t.Fatal("no @everyone deny on a new channel")
	}
	if b, ok := fresh.Member(2); !ok || b.Allow != allow {
		t.Fatal("no bot grant on a new channel")
	}
}
