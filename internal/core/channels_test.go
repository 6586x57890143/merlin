package core

import (
	"testing"

	"github.com/bwmarrin/discordgo"
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
