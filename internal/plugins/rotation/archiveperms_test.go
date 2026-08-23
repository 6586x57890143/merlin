package rotation

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

const viewBit = int64(discordgo.PermissionViewChannel)

func roleOW(id string, allow, deny int64) *discordgo.PermissionOverwrite {
	return &discordgo.PermissionOverwrite{ID: id, Type: discordgo.PermissionOverwriteTypeRole, Allow: allow, Deny: deny}
}

func memberOW(id string, allow, deny int64) *discordgo.PermissionOverwrite {
	return &discordgo.PermissionOverwrite{ID: id, Type: discordgo.PermissionOverwriteTypeMember, Allow: allow, Deny: deny}
}

func findOW(out []*discordgo.PermissionOverwrite, id string, kind discordgo.PermissionOverwriteType) *discordgo.PermissionOverwrite {
	for _, ow := range out {
		if ow.ID == id && ow.Type == kind {
			return ow
		}
	}
	return nil
}

// TestDesiredArchiveOverwrites is the whole contract of the archive
// permission policy: only the bot, the mod roles and the guild's configured
// archive viewer roles may see an archive, every other *allow* of view is
// taken back, and no *deny* is ever touched.
func TestDesiredArchiveOverwrites(t *testing.T) {
	const (
		guildID = "g1"
		botID   = "bot"
	)

	t.Run("denies everyone and keeps the bot in", func(t *testing.T) {
		out := desiredArchiveOverwrites(nil, guildID, botID, archiveAccess{})
		everyone := findOW(out, guildID, discordgo.PermissionOverwriteTypeRole)
		if everyone == nil || everyone.Deny&viewBit == 0 || everyone.Allow&viewBit != 0 {
			t.Fatalf("expected @everyone denied view, got %+v", everyone)
		}
		if bot := findOW(out, botID, discordgo.PermissionOverwriteTypeMember); bot == nil || bot.Allow&viewBit == 0 {
			t.Fatalf("expected the bot allowed view, got %+v", bot)
		}
	})

	t.Run("grants mod and viewer roles", func(t *testing.T) {
		out := desiredArchiveOverwrites(nil, guildID, botID, archiveAccess{modRoleIDs: []string{"mod"}, viewerRoleIDs: []string{"gem"}})
		for _, id := range []string{"mod", "gem"} {
			if ow := findOW(out, id, discordgo.PermissionOverwriteTypeRole); ow == nil || ow.Allow&viewBit == 0 {
				t.Fatalf("expected role %s allowed view, got %+v", id, ow)
			}
		}
	})

	// A viewer role is added to read an archive, so it gets exactly that and
	// nothing else. Granting ViewChannel alone would leave the role's own
	// guild-level SendMessages, AddReactions and thread permissions applying
	// inside the archive, which is the opposite of a record nobody can edit.
	t.Run("viewer roles are read-only", func(t *testing.T) {
		out := desiredArchiveOverwrites(nil, guildID, botID, archiveAccess{viewerRoleIDs: []string{"gem"}})
		gem := findOW(out, "gem", discordgo.PermissionOverwriteTypeRole)
		if gem == nil {
			t.Fatal("expected an overwrite for the viewer role")
		}
		if gem.Allow&viewBit == 0 || gem.Allow&int64(discordgo.PermissionReadMessageHistory) == 0 {
			t.Fatalf("expected view and read-history allowed, got allow=%d", gem.Allow)
		}
		for _, bit := range []struct {
			name string
			mask int64
		}{
			{"SendMessages", discordgo.PermissionSendMessages},
			{"SendMessagesInThreads", discordgo.PermissionSendMessagesInThreads},
			{"AddReactions", discordgo.PermissionAddReactions},
			{"CreatePublicThreads", discordgo.PermissionCreatePublicThreads},
			{"CreatePrivateThreads", discordgo.PermissionCreatePrivateThreads},
			{"AttachFiles", discordgo.PermissionAttachFiles},
			{"EmbedLinks", discordgo.PermissionEmbedLinks},
			{"ManageMessages", discordgo.PermissionManageMessages},
			{"ManageWebhooks", discordgo.PermissionManageWebhooks},
			{"VoiceConnect", discordgo.PermissionVoiceConnect},
		} {
			if gem.Deny&bit.mask == 0 {
				t.Errorf("expected %s denied for a read-only viewer role, got deny=%d", bit.name, gem.Deny)
			}
			if gem.Allow&bit.mask != 0 {
				t.Errorf("expected %s not allowed for a read-only viewer role, got allow=%d", bit.name, gem.Allow)
			}
		}
	})

	// Mods go back through archives to work out what happened, so the
	// read-only clamp is for viewer roles only.
	t.Run("mod roles are not clamped to read-only", func(t *testing.T) {
		out := desiredArchiveOverwrites(nil, guildID, botID, archiveAccess{modRoleIDs: []string{"mod"}})
		mod := findOW(out, "mod", discordgo.PermissionOverwriteTypeRole)
		if mod == nil || mod.Deny != 0 {
			t.Fatalf("expected a mod role to carry no denies, got %+v", mod)
		}
	})

	// Promoting a viewer role to a mod role has to lift the clamp this file
	// wrote for it, or the role ends up able to moderate an archive it cannot
	// type in.
	t.Run("promoting a viewer role to mod lifts the read-only clamp", func(t *testing.T) {
		asViewer := desiredArchiveOverwrites(nil, guildID, botID, archiveAccess{viewerRoleIDs: []string{"gem"}})
		asMod := desiredArchiveOverwrites(asViewer, guildID, botID, archiveAccess{modRoleIDs: []string{"gem"}})
		gem := findOW(asMod, "gem", discordgo.PermissionOverwriteTypeRole)
		if gem == nil || gem.Deny != 0 {
			t.Fatalf("expected the clamp lifted once gem is a mod role, got %+v", gem)
		}
		if gem.Allow&viewBit == 0 {
			t.Fatalf("expected gem still allowed view, got allow=%d", gem.Allow)
		}
	})

	// A role already carrying an allow the read-only clamp forbids has that
	// allow taken off it, or the grant would leave it able to post in a
	// channel it was only meant to read.
	t.Run("clamp overrides an existing allow on a viewer role", func(t *testing.T) {
		existing := roleOW("gem", int64(discordgo.PermissionSendMessages)|int64(discordgo.PermissionAddReactions), 0)
		out := desiredArchiveOverwrites([]*discordgo.PermissionOverwrite{existing}, guildID, botID, archiveAccess{viewerRoleIDs: []string{"gem"}})
		gem := findOW(out, "gem", discordgo.PermissionOverwriteTypeRole)
		if gem.Allow&int64(discordgo.PermissionSendMessages) != 0 || gem.Allow&int64(discordgo.PermissionAddReactions) != 0 {
			t.Fatalf("expected the pre-existing write allows stripped, got allow=%d", gem.Allow)
		}
		if gem.Deny&int64(discordgo.PermissionSendMessages) == 0 {
			t.Fatalf("expected SendMessages denied instead, got deny=%d", gem.Deny)
		}
	})

	t.Run("strips a stray role's view allow but keeps its other bits", func(t *testing.T) {
		stray := roleOW("stray", viewBit|int64(discordgo.PermissionSendMessages), 0)
		out := desiredArchiveOverwrites([]*discordgo.PermissionOverwrite{stray}, guildID, botID, archiveAccess{modRoleIDs: []string{"mod"}})
		got := findOW(out, "stray", discordgo.PermissionOverwriteTypeRole)
		if got == nil {
			t.Fatal("expected the stray overwrite to survive, since it still carries a non-view allow")
		}
		if got.Allow&viewBit != 0 {
			t.Fatalf("expected the stray's view allow stripped, got allow=%d", got.Allow)
		}
		if got.Allow&int64(discordgo.PermissionSendMessages) == 0 {
			t.Fatalf("expected unrelated bits left alone, got allow=%d", got.Allow)
		}
	})

	t.Run("never touches a deny", func(t *testing.T) {
		jailed := roleOW("jailed", 0, viewBit)
		shutOut := memberOW("nosy", 0, viewBit)
		out := desiredArchiveOverwrites([]*discordgo.PermissionOverwrite{jailed, shutOut}, guildID, botID, archiveAccess{modRoleIDs: []string{"mod"}})
		for _, want := range []struct {
			id   string
			kind discordgo.PermissionOverwriteType
		}{
			{"jailed", discordgo.PermissionOverwriteTypeRole},
			{"nosy", discordgo.PermissionOverwriteTypeMember},
		} {
			got := findOW(out, want.id, want.kind)
			if got == nil || got.Deny&viewBit == 0 {
				t.Fatalf("expected %s's view deny preserved, got %+v", want.id, got)
			}
		}
	})

	t.Run("drops an overwrite that now says nothing", func(t *testing.T) {
		out := desiredArchiveOverwrites([]*discordgo.PermissionOverwrite{roleOW("stray", viewBit, 0)}, guildID, botID, archiveAccess{})
		if got := findOW(out, "stray", discordgo.PermissionOverwriteTypeRole); got != nil {
			t.Fatalf("expected an allow-only stray to be dropped entirely, got %+v", got)
		}
	})

	t.Run("legacy whitelisted member keeps access", func(t *testing.T) {
		out := desiredArchiveOverwrites(nil, guildID, botID, archiveAccess{viewerUserIDs: []string{"friend"}})
		if ow := findOW(out, "friend", discordgo.PermissionOverwriteTypeMember); ow == nil || ow.Allow&viewBit == 0 {
			t.Fatalf("expected the whitelisted member allowed view, got %+v", ow)
		}
	})

	// The drift check writes (and audits) only when the live overwrites differ
	// from the desired ones, so a second pass over its own output has to be a
	// no-op. Without this the hourly sweep would rewrite every archive channel
	// and post an audit line every hour, forever.
	t.Run("is idempotent", func(t *testing.T) {
		start := []*discordgo.PermissionOverwrite{
			roleOW("stray", viewBit|int64(discordgo.PermissionSendMessages), 0),
			roleOW("jailed", 0, viewBit),
			roleOW("gem", 0, 0),
		}
		access := archiveAccess{modRoleIDs: []string{"mod"}, viewerRoleIDs: []string{"gem"}, viewerUserIDs: []string{"friend"}}
		once := desiredArchiveOverwrites(start, guildID, botID, access)
		twice := desiredArchiveOverwrites(once, guildID, botID, access)
		if !overwritesEqual(once, twice) {
			t.Fatalf("expected a second pass to change nothing:\nonce:  %+v\ntwice: %+v", once, twice)
		}
	})
}

func TestOverwritesEqualIgnoresOrder(t *testing.T) {
	a := []*discordgo.PermissionOverwrite{roleOW("x", viewBit, 0), roleOW("y", 0, viewBit)}
	b := []*discordgo.PermissionOverwrite{roleOW("y", 0, viewBit), roleOW("x", viewBit, 0)}
	if !overwritesEqual(a, b) {
		t.Fatal("expected order not to matter: Discord returns overwrites in whatever order it likes")
	}
	if overwritesEqual(a, []*discordgo.PermissionOverwrite{roleOW("x", viewBit, 0)}) {
		t.Fatal("expected a shorter list to differ")
	}
}
