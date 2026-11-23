package dragoman

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/dbtest"
)

func TestPostgresStore(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	s := NewPostgresStore(pool)
	const g = "dragoman-test"
	clear := func() {
		for _, table := range []string{"dragoman_config", "dragoman_translations"} {
			if _, err := pool.Exec(ctx, `DELETE FROM `+table+` WHERE guild_id LIKE $1`, g+"%"); err != nil {
				t.Fatal(err)
			}
		}
	}
	clear()
	t.Cleanup(clear)
	ok := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	c, err := s.Config(ctx, g)
	ok(err)
	if c.Enabled || len(c.ExemptChannelIDs) != 0 || len(c.OptOutUserIDs) != 0 {
		t.Fatalf("a guild with no row has %+v", c)
	}
	// Each setter makes the row, and leaves the other settings alone.
	ok(s.SetExempt(ctx, g, "c1", true))
	ok(s.SetExempt(ctx, g, "c1", true))
	ok(s.SetExempt(ctx, g, "c2", true))
	ok(s.SetOptOut(ctx, g, "u1", true))
	ok(s.SetEnabled(ctx, g, true))
	ok(s.SetExempt(ctx, g, "c2", false))
	c, err = s.Config(ctx, g)
	ok(err)
	if !c.Enabled || !slices.Equal(c.ExemptChannelIDs, []string{"c1"}) || !slices.Equal(c.OptOutUserIDs, []string{"u1"}) {
		t.Fatalf("config %+v", c)
	}
	// Opting in where there is no row is not an opt-out.
	ok(s.SetOptOut(ctx, g+"2", "u1", false))
	if c, err = s.Config(ctx, g+"2"); err != nil || len(c.OptOutUserIDs) != 0 {
		t.Fatalf("config %+v, %v", c, err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	old := Translation{GuildID: g, ChannelID: "c", RepostID: "r1", AuthorID: "u", Language: "nl", CreatedAt: now.Add(-48 * time.Hour)}
	fresh := old
	fresh.RepostID, fresh.CreatedAt = "r2", now
	ok(s.Record(ctx, old))
	ok(s.Record(ctx, fresh))
	ok(s.Record(ctx, fresh))
	if n, err := s.CountSince(ctx, g, now.Add(-time.Hour)); err != nil || n != 1 {
		t.Fatalf("count %d, %v", n, err)
	}
	got, err := s.Find(ctx, g, "r2")
	ok(err)
	if got.AuthorID != "u" || got.ChannelID != "c" || got.Language != "nl" || !got.CreatedAt.Equal(now) {
		t.Fatalf("found %+v", got)
	}
	if _, err := s.Find(ctx, g+"2", "r2"); !errors.Is(err, errNotFound) {
		t.Fatalf("another guild's: %v", err)
	}
	if _, err := s.PruneBefore(ctx, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Find(ctx, g, "r1"); !errors.Is(err, errNotFound) {
		t.Fatalf("an old row outlived the prune: %v", err)
	}
	if _, err := s.Find(ctx, g, "r2"); err != nil {
		t.Fatalf("a fresh row did not: %v", err)
	}

	// Every read and write reports a database that is not there.
	dead, cancel := context.WithCancel(ctx)
	cancel()
	_, e1 := s.Config(dead, g)
	_, e2 := s.Find(dead, g, "r2")
	_, e3 := s.CountSince(dead, g, now)
	_, e4 := s.PruneBefore(dead, now)
	for i, err := range []error{e1, e2, e3, e4, s.SetEnabled(dead, g, true), s.SetExempt(dead, g, "c", true), s.Record(dead, fresh)} {
		if err == nil || errors.Is(err, errNotFound) {
			t.Errorf("call %d: %v", i, err)
		}
	}
}
