package rotation

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

const viewBit = discord.PermissionViewChannel

func roleOW(id snowflake.ID, allow, deny discord.Permissions) discord.PermissionOverwrite {
	return discord.RolePermissionOverwrite{RoleID: id, Allow: allow, Deny: deny}
}

func memberOW(id snowflake.ID, allow, deny discord.Permissions) discord.PermissionOverwrite {
	return discord.MemberPermissionOverwrite{UserID: id, Allow: allow, Deny: deny}
}

func findOW(out []discord.PermissionOverwrite, id snowflake.ID, kind discord.PermissionOverwriteType) *overwrite {
	for _, ow := range overwritesOf(out) {
		if ow.ID == id && ow.Type == kind {
			return &ow
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
		guildID snowflake.ID = 100
		botID   snowflake.ID = 800
	)

	t.Run("denies everyone and keeps the bot in", func(t *testing.T) {
		out := desiredArchiveOverwrites(nil, guildID, botID, archiveAccess{})
		everyone := findOW(out, guildID, discord.PermissionOverwriteTypeRole)
		if everyone == nil || everyone.Deny&viewBit == 0 || everyone.Allow&viewBit != 0 {
			t.Fatalf("expected @everyone denied view, got %+v", everyone)
		}
		if bot := findOW(out, botID, discord.PermissionOverwriteTypeMember); bot == nil || bot.Allow&viewBit == 0 {
			t.Fatalf("expected the bot allowed view, got %+v", bot)
		}
	})

	t.Run("grants mod and viewer roles", func(t *testing.T) {
		out := desiredArchiveOverwrites(nil, guildID, botID, archiveAccess{modRoleIDs: []snowflake.ID{600}, viewerRoleIDs: []snowflake.ID{602}})
		for _, id := range []snowflake.ID{600, 602} {
			if ow := findOW(out, id, discord.PermissionOverwriteTypeRole); ow == nil || ow.Allow&viewBit == 0 {
				t.Fatalf("expected role %s allowed view, got %+v", id, ow)
			}
		}
	})

	// A viewer role is added to read an archive, so it gets exactly that and
	// nothing else. Granting ViewChannel alone would leave the role's own
	// guild-level SendMessages, AddReactions and thread permissions applying
	// inside the archive, which is the opposite of a record nobody can edit.
	t.Run("viewer roles are read-only", func(t *testing.T) {
		out := desiredArchiveOverwrites(nil, guildID, botID, archiveAccess{viewerRoleIDs: []snowflake.ID{602}})
		gem := findOW(out, 602, discord.PermissionOverwriteTypeRole)
		if gem == nil {
			t.Fatal("expected an overwrite for the viewer role")
		}
		if gem.Allow&viewBit == 0 || gem.Allow&discord.PermissionReadMessageHistory == 0 {
			t.Fatalf("expected view and read-history allowed, got allow=%d", gem.Allow)
		}
		for _, bit := range []struct {
			name string
			mask discord.Permissions
		}{
			{"SendMessages", discord.PermissionSendMessages},
			{"SendMessagesInThreads", discord.PermissionSendMessagesInThreads},
			{"AddReactions", discord.PermissionAddReactions},
			{"CreatePublicThreads", discord.PermissionCreatePublicThreads},
			{"CreatePrivateThreads", discord.PermissionCreatePrivateThreads},
			{"AttachFiles", discord.PermissionAttachFiles},
			{"EmbedLinks", discord.PermissionEmbedLinks},
			{"ManageMessages", discord.PermissionManageMessages},
			{"ManageWebhooks", discord.PermissionManageWebhooks},
			{"VoiceConnect", discord.PermissionConnect},
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
		out := desiredArchiveOverwrites(nil, guildID, botID, archiveAccess{modRoleIDs: []snowflake.ID{600}})
		mod := findOW(out, 600, discord.PermissionOverwriteTypeRole)
		if mod == nil || mod.Deny != 0 {
			t.Fatalf("expected a mod role to carry no denies, got %+v", mod)
		}
	})

	// Promoting a viewer role to a mod role has to lift the clamp this file
	// wrote for it, or the role ends up able to moderate an archive it cannot
	// type in.
	t.Run("promoting a viewer role to mod lifts the read-only clamp", func(t *testing.T) {
		asViewer := desiredArchiveOverwrites(nil, guildID, botID, archiveAccess{viewerRoleIDs: []snowflake.ID{602}})
		asMod := desiredArchiveOverwrites(asViewer, guildID, botID, archiveAccess{modRoleIDs: []snowflake.ID{602}})
		gem := findOW(asMod, 602, discord.PermissionOverwriteTypeRole)
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
		existing := roleOW(602, discord.PermissionSendMessages|discord.PermissionAddReactions, 0)
		out := desiredArchiveOverwrites([]discord.PermissionOverwrite{existing}, guildID, botID, archiveAccess{viewerRoleIDs: []snowflake.ID{602}})
		gem := findOW(out, 602, discord.PermissionOverwriteTypeRole)
		if gem.Allow&discord.PermissionSendMessages != 0 || gem.Allow&discord.PermissionAddReactions != 0 {
			t.Fatalf("expected the pre-existing write allows stripped, got allow=%d", gem.Allow)
		}
		if gem.Deny&discord.PermissionSendMessages == 0 {
			t.Fatalf("expected SendMessages denied instead, got deny=%d", gem.Deny)
		}
	})

	t.Run("strips a stray role's view allow but keeps its other bits", func(t *testing.T) {
		stray := roleOW(900, viewBit|discord.PermissionSendMessages, 0)
		out := desiredArchiveOverwrites([]discord.PermissionOverwrite{stray}, guildID, botID, archiveAccess{modRoleIDs: []snowflake.ID{600}})
		got := findOW(out, 900, discord.PermissionOverwriteTypeRole)
		if got == nil {
			t.Fatal("expected the stray overwrite to survive, since it still carries a non-view allow")
		}
		if got.Allow&viewBit != 0 {
			t.Fatalf("expected the stray's view allow stripped, got allow=%d", got.Allow)
		}
		if got.Allow&discord.PermissionSendMessages == 0 {
			t.Fatalf("expected unrelated bits left alone, got allow=%d", got.Allow)
		}
	})

	t.Run("never touches a deny", func(t *testing.T) {
		jailed := roleOW(901, 0, viewBit)
		shutOut := memberOW(902, 0, viewBit)
		out := desiredArchiveOverwrites([]discord.PermissionOverwrite{jailed, shutOut}, guildID, botID, archiveAccess{modRoleIDs: []snowflake.ID{600}})
		for _, want := range []struct {
			id   snowflake.ID
			kind discord.PermissionOverwriteType
		}{
			{901, discord.PermissionOverwriteTypeRole},
			{902, discord.PermissionOverwriteTypeMember},
		} {
			got := findOW(out, want.id, want.kind)
			if got == nil || got.Deny&viewBit == 0 {
				t.Fatalf("expected %s's view deny preserved, got %+v", want.id, got)
			}
		}
	})

	t.Run("drops an overwrite that now says nothing", func(t *testing.T) {
		out := desiredArchiveOverwrites([]discord.PermissionOverwrite{roleOW(900, viewBit, 0)}, guildID, botID, archiveAccess{})
		if got := findOW(out, 900, discord.PermissionOverwriteTypeRole); got != nil {
			t.Fatalf("expected an allow-only stray to be dropped entirely, got %+v", got)
		}
	})

	t.Run("legacy whitelisted member keeps access", func(t *testing.T) {
		out := desiredArchiveOverwrites(nil, guildID, botID, archiveAccess{viewerUserIDs: []snowflake.ID{700}})
		if ow := findOW(out, 700, discord.PermissionOverwriteTypeMember); ow == nil || ow.Allow&viewBit == 0 {
			t.Fatalf("expected the whitelisted member allowed view, got %+v", ow)
		}
	})

	// The drift check writes (and audits) only when the live overwrites differ
	// from the desired ones, so a second pass over its own output has to be a
	// no-op. Without this the hourly sweep would rewrite every archive channel
	// and post an audit line every hour, forever.
	t.Run("is idempotent", func(t *testing.T) {
		start := []discord.PermissionOverwrite{
			roleOW(900, viewBit|discord.PermissionSendMessages, 0),
			roleOW(901, 0, viewBit),
			roleOW(602, 0, 0),
		}
		access := archiveAccess{modRoleIDs: []snowflake.ID{600}, viewerRoleIDs: []snowflake.ID{602}, viewerUserIDs: []snowflake.ID{700}}
		once := desiredArchiveOverwrites(start, guildID, botID, access)
		twice := desiredArchiveOverwrites(once, guildID, botID, access)
		if !overwritesEqual(once, twice) {
			t.Fatalf("expected a second pass to change nothing:\nonce:  %+v\ntwice: %+v", once, twice)
		}
	})
}

func TestOverwritesEqualIgnoresOrder(t *testing.T) {
	a := []discord.PermissionOverwrite{roleOW(903, viewBit, 0), roleOW(904, 0, viewBit)}
	b := []discord.PermissionOverwrite{roleOW(904, 0, viewBit), roleOW(903, viewBit, 0)}
	if !overwritesEqual(a, b) {
		t.Fatal("expected order not to matter: Discord returns overwrites in whatever order it likes")
	}
	if overwritesEqual(a, []discord.PermissionOverwrite{roleOW(903, viewBit, 0)}) {
		t.Fatal("expected a shorter list to differ")
	}
}
