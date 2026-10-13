package core

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
)

// DenyEveryone denies @everyone view and grants the bot its bits, merging
// into an existing overwrite for either rather than duplicating it (Discord
// rejects a duplicate) and leaving every other overwrite as it was. Without
// the bot grant the bot locks itself out of the channel it just made.
func TestDenyEveryone(t *testing.T) {
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
