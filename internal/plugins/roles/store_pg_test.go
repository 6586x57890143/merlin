package roles

import (
	"context"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/dbtest"
)

// The eternal-role copy is the one row in this package with a BYTEA column
// and a NULL-able one, which the in-memory fake cannot vouch for. Skips
// itself when TEST_DATABASE_URL is unset, like every other Postgres test.
func TestEternalRoleRoundTripThroughPostgres(t *testing.T) {
	store := NewPostgresStore(dbtest.Pool(t))
	ctx := context.Background()
	guild := t.Name()
	at := time.Now().UTC().Truncate(time.Second)

	rec := EternalRoleRecord{GuildID: guild, UserID: "u1", OriginRoleID: "r1", RoleID: "r1", Name: "neumale", Color: 0xABCDEF,
		Hoist: true, Mentionable: true, Permissions: 1 << 40, UnicodeEmoji: "", IconHash: "h", Icon: []byte{0x89, 'P', 'N', 'G'}, CapturedAt: at}
	if err := store.PutEternalRole(ctx, rec); err != nil {
		t.Fatalf("put: %v", err)
	}
	// A second member with no icon at all: NULL round-trips as nil.
	if err := store.PutEternalRole(ctx, EternalRoleRecord{GuildID: guild, UserID: "u2", OriginRoleID: "r2", RoleID: "r2", Name: "plain", CapturedAt: at.Add(time.Second)}); err != nil {
		t.Fatalf("put 2: %v", err)
	}

	got, err := store.ListEternalRoles(ctx, guild)
	if err != nil || len(got) != 2 {
		t.Fatalf("list: %v %d", err, len(got))
	}
	g := got[0]
	if g.UserID != "u1" || g.Name != "neumale" || g.Color != 0xABCDEF || !g.Hoist || !g.Mentionable || g.Permissions != 1<<40 ||
		g.IconHash != "h" || string(g.Icon) != "\x89PNG" || !g.CapturedAt.Equal(at) {
		t.Fatalf("row 1 did not round-trip: %+v", g)
	}
	if got[1].Icon != nil {
		t.Fatalf("no icon should read back as nil, got %v", got[1].Icon)
	}

	// Retarget is an upsert on the same key.
	rec.RoleID, rec.IconHash = "r1-new", "h2"
	if err := store.PutEternalRole(ctx, rec); err != nil {
		t.Fatalf("retarget: %v", err)
	}
	got, _ = store.ListEternalRoles(ctx, guild)
	if len(got) != 2 || got[0].RoleID != "r1-new" || got[0].IconHash != "h2" {
		t.Fatalf("retarget did not upsert: %+v", got)
	}

	if err := store.DeleteEternalRole(ctx, guild, "u1", "r1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ = store.ListEternalRoles(ctx, guild); len(got) != 1 || got[0].UserID != "u2" {
		t.Fatalf("delete should drop exactly one row: %+v", got)
	}
	if got, err := store.ListEternalRoles(ctx, "nobody"); err != nil || len(got) != 0 {
		t.Fatalf("unknown guild: %v %v", got, err)
	}
}
