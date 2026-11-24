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
		for _, table := range []string{"dragoman_config", "dragoman_translations", "dragoman_usage", "dragoman_dialects"} {
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
	if c.Enabled || c.MemberControls || len(c.ExemptChannelIDs) != 0 || len(c.OptOutUserIDs) != 0 {
		t.Fatalf("a guild with no row has %+v", c)
	}
	// Each setter makes the row, and leaves the other settings alone.
	ok(s.SetExempt(ctx, g, "c1", true))
	ok(s.SetExempt(ctx, g, "c1", true))
	ok(s.SetExempt(ctx, g, "c2", true))
	ok(s.SetOptOut(ctx, g, "u1", true))
	ok(s.SetEnabled(ctx, g, true))
	ok(s.SetMemberControls(ctx, g, true))
	ok(s.SetExempt(ctx, g, "c2", false))
	ok(s.SetModel(ctx, g, "a/b"))
	c, err = s.Config(ctx, g)
	ok(err)
	if !c.Enabled || !c.MemberControls || c.Model != "a/b" || !slices.Equal(c.ExemptChannelIDs, []string{"c1"}) || !slices.Equal(c.OptOutUserIDs, []string{"u1"}) {
		t.Fatalf("config %+v", c)
	}
	// Opting in where there is no row is not an opt-out.
	ok(s.SetOptOut(ctx, g+"2", "u1", false))
	if c, err = s.Config(ctx, g+"2"); err != nil || len(c.OptOutUserIDs) != 0 {
		t.Fatalf("config %+v, %v", c, err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	day := now.Truncate(24 * time.Hour)
	ok(s.AddUsage(ctx, g, day, rungModel, 0.25))
	ok(s.AddUsage(ctx, g, day, rungModel, 0.5))
	ok(s.AddUsage(ctx, g, day, rungCareful, 0))
	ok(s.AddUsage(ctx, g, day.Add(-72*time.Hour), rungModel, 9))
	if u, err := s.Usage(ctx, g, day); err != nil || len(u) != 2 || u[rungModel] != (Usage{2, 0.75}) || u[rungCareful] != (Usage{1, 0}) {
		t.Fatalf("usage %+v, %v", u, err)
	}
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
	if u, err := s.Usage(ctx, g, day.Add(-72*time.Hour)); err != nil || len(u) != 0 {
		t.Fatalf("old usage outlived the prune: %+v, %v", u, err)
	}
	if _, err := s.Find(ctx, g, "r1"); !errors.Is(err, errNotFound) {
		t.Fatalf("an old row outlived the prune: %v", err)
	}
	if _, err := s.Find(ctx, g, "r2"); err != nil {
		t.Fatalf("a fresh row did not: %v", err)
	}

	// Dialects: a write counts only against the row it was read as.
	ok(s.SetDialect(ctx, g, "da", "Jamaican English"))
	d, err := s.Dialect(ctx, g, "da")
	ok(err)
	if d.Guide != nil || d.Name != "Jamaican English" || len(d.Notes) != 0 {
		t.Fatalf("dialect %+v", d)
	}
	ok(s.SetDialectProblem(ctx, d, "no"))
	first := d
	d, err = s.SaveGuide(ctx, d, patois(1), 2)
	ok(err)
	if _, err := s.SaveGuide(ctx, first, patois(9), 2); !errors.Is(err, errStale) {
		t.Fatalf("a stale guide was saved: %v", err)
	}
	for i, want := range []bool{false, true} {
		if due, err := s.CountDialect(ctx, d, i == 0); err != nil || due != want {
			t.Fatalf("message %d: due %v, %v", i, due, err)
		}
	}
	if due, err := s.CountDialect(ctx, first, true); err != nil || due {
		t.Fatalf("a stale count: %v, %v", due, err)
	}
	ok(s.DeferReview(ctx, d, 50))
	ok(s.SetDialectNotes(ctx, d, []string{"lighter"}))
	if due, err := s.CountDialect(ctx, d, false); err != nil || due {
		t.Fatalf("due after being put off: %v, %v", due, err)
	}
	if d, err = s.Dialect(ctx, g, "da"); err != nil || d.Uses != 3 || d.Missed != 1 || d.Losses != 0 || d.Problem != "" || d.Previous != nil ||
		!slices.Equal(d.Notes, []string{"lighter"}) || d.Guide.Rules[0] != patois(1).Rules[0] {
		t.Fatalf("dialect %+v, %v", d, err)
	}
	d, err = s.SaveGuide(ctx, d, patois(2), 2)
	ok(err)
	if d.Previous == nil || d.Uses != 0 {
		t.Fatalf("dialect %+v", d)
	}
	ok(s.UndoGuide(ctx, d, 100))
	if d, err = s.Dialect(ctx, g, "da"); err != nil || d.Guide.Rules[0] != patois(1).Rules[0] || d.Previous != nil {
		t.Fatalf("undone to %+v, %v", d, err)
	}
	ok(s.SetDialect(ctx, g, "da", "Scottish English"))
	ok(s.SetDialect(ctx, g, "nl", "Welsh English"))
	if all, err := s.Dialects(ctx, g); err != nil || len(all) != 2 || all[0].Guide != nil || all[0].Version <= d.Version || len(all[0].Notes) != 0 {
		t.Fatalf("dialects %+v, %v", all, err)
	}
	ok(s.RemoveDialect(ctx, g, "da"))
	if _, err := s.Dialect(ctx, g, "da"); !errors.Is(err, errStale) {
		t.Fatalf("removed: %v", err)
	}

	// Every read and write reports a database that is not there.
	dead, cancel := context.WithCancel(ctx)
	cancel()
	_, e1 := s.Config(dead, g)
	_, e2 := s.Find(dead, g, "r2")
	_, e3 := s.CountSince(dead, g, now)
	_, e4 := s.PruneBefore(dead, now)
	_, e5 := s.Usage(dead, g, day)
	_, e6 := s.Dialects(dead, g)
	_, e7 := s.Dialect(dead, g, "nl")
	_, e8 := s.CountDialect(dead, d, true)
	_, e9 := s.SaveGuide(dead, d, patois(1), 1)
	for i, err := range []error{e1, e2, e3, e4, e5, e6, e7, e8, e9, s.SetDialect(dead, g, "nl", "x"), s.RemoveDialect(dead, g, "nl"), s.UndoGuide(dead, d, 1), s.SetModel(dead, g, ""), s.AddUsage(dead, g, day, rungCareful, 0), s.SetEnabled(dead, g, true), s.SetMemberControls(dead, g, true), s.SetExempt(dead, g, "c", true), s.Record(dead, fresh)} {
		if err == nil || errors.Is(err, errNotFound) || errors.Is(err, errStale) {
			t.Errorf("call %d: %v", i, err)
		}
	}
}
