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
		out := desiredArchiveOverwrites(nil, guildID, botID, nil, nil)
		everyone := findOW(out, guildID, discordgo.PermissionOverwriteTypeRole)
		if everyone == nil || everyone.Deny&viewBit == 0 || everyone.Allow&viewBit != 0 {
			t.Fatalf("expected @everyone denied view, got %+v", everyone)
		}
		if bot := findOW(out, botID, discordgo.PermissionOverwriteTypeMember); bot == nil || bot.Allow&viewBit == 0 {
			t.Fatalf("expected the bot allowed view, got %+v", bot)
		}
	})

	t.Run("grants mod and viewer roles", func(t *testing.T) {
		out := desiredArchiveOverwrites(nil, guildID, botID, []string{"mod", "gem"}, nil)
		for _, id := range []string{"mod", "gem"} {
			if ow := findOW(out, id, discordgo.PermissionOverwriteTypeRole); ow == nil || ow.Allow&viewBit == 0 {
				t.Fatalf("expected role %s allowed view, got %+v", id, ow)
			}
		}
	})

	t.Run("strips a stray role's view allow but keeps its other bits", func(t *testing.T) {
		stray := roleOW("stray", viewBit|int64(discordgo.PermissionSendMessages), 0)
		out := desiredArchiveOverwrites([]*discordgo.PermissionOverwrite{stray}, guildID, botID, []string{"mod"}, nil)
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
		out := desiredArchiveOverwrites([]*discordgo.PermissionOverwrite{jailed, shutOut}, guildID, botID, []string{"mod"}, nil)
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
		out := desiredArchiveOverwrites([]*discordgo.PermissionOverwrite{roleOW("stray", viewBit, 0)}, guildID, botID, nil, nil)
		if got := findOW(out, "stray", discordgo.PermissionOverwriteTypeRole); got != nil {
			t.Fatalf("expected an allow-only stray to be dropped entirely, got %+v", got)
		}
	})

	t.Run("legacy whitelisted member keeps access", func(t *testing.T) {
		out := desiredArchiveOverwrites(nil, guildID, botID, nil, []string{"friend"})
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
		once := desiredArchiveOverwrites(start, guildID, botID, []string{"mod", "gem"}, []string{"friend"})
		twice := desiredArchiveOverwrites(once, guildID, botID, []string{"mod", "gem"}, []string{"friend"})
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
