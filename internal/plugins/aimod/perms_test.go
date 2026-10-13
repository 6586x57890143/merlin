package aimod

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
)

// aimod asks for its permissions only where its mode is on, so a server
// that never turns it on is never asked to let merlin delete messages.
func TestPermsFollowTheMode(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, nil, newFakeOps(), &fakeAudit{})
	store.setConfig(Config{GuildID: "g1", Mode: ModeOff})
	if got := p.Perms("g1"); got != 0 {
		t.Fatalf("mode off asked for %b", got)
	}
	store.setConfig(Config{GuildID: "g1", Mode: ModeFlag})
	if got := p.Perms("g1"); !got.Has(discord.PermissionManageMessages | discord.PermissionManageWebhooks | discord.PermissionModerateMembers) {
		t.Fatalf("mode flag asked for %b", got)
	}
}
