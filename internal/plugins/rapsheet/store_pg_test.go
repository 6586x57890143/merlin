package rapsheet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/dbtest"
)

// What the in-memory fake cannot check: the half of the store's contract
// Postgres itself enforces. The partial unique index that makes ingestion
// idempotent, the ON CONFLICT DO NOTHING that reports a duplicate as "not
// written" rather than an error, the predicate behind the ban-due index, the
// CHECK constraints standing between a hand-edited row and a kind this code
// cannot render, and the group subquery.
//
// Skips rather than fails without TEST_DATABASE_URL, so `go test ./...`
// still works with no setup. CI runs it against postgres:16-alpine.

func testStore(t *testing.T) (Store, string) {
	t.Helper()
	pool := dbtest.Pool(t)
	var b [8]byte
	_, _ = rand.Read(b[:])
	// A fresh guild per test, since dbtest's database is shared across them.
	return NewPostgresStore(pool), "g-" + hex.EncodeToString(b[:])
}

func TestPostgresConfigRoundTrips(t *testing.T) {
	s, g := testStore(t)
	ctx := context.Background()

	cfg, err := s.Config(ctx, g)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.EscalationMode != ModeSuggest || cfg.HalfLife != defaultHalfLife || !cfg.AltHints || len(cfg.Bands) != 0 {
		t.Errorf("unconfigured guild should read as defaults, got %+v", cfg)
	}

	cfg.EscalationMode = ModeAuto
	cfg.HalfLife = 14 * 24 * time.Hour
	cfg.CategoryPoints[CategorySpam] = 5
	cfg.Bands = []Band{{0, ActionNone, 0}, {30, ActionJail, 3 * time.Hour}, {90, ActionBan, 48 * time.Hour}}
	cfg.ModChannelID, cfg.ForumChannelID, cfg.AltHints = "mods", "forum", false
	if err := s.SetConfig(ctx, cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	got, err := s.Config(ctx, g)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if got.EscalationMode != ModeAuto || got.HalfLife != 14*24*time.Hour || got.CategoryPoints[CategorySpam] != 5 ||
		len(got.Bands) != 3 || got.Bands[1].Duration != 3*time.Hour || got.Bands[2].Action != ActionBan ||
		got.ModChannelID != "mods" || got.ForumChannelID != "forum" || got.AltHints {
		t.Errorf("round trip lost something: %+v", got)
	}
}

func TestPostgresEntryLifecycle(t *testing.T) {
	s, g := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	id, err := s.Insert(ctx, Entry{
		GuildID: g, UserID: "u1", Kind: KindWarn, Category: CategoryHateSpeech, Points: 50,
		ActorID: "mod-1", Reason: "r", Source: SourceCommand, CreatedAt: now,
	})
	if err != nil || id == 0 {
		t.Fatalf("Insert = %d, %v", id, err)
	}
	e, err := s.Entry(ctx, g, id)
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if e.Kind != KindWarn || e.Points != 50 || e.Duration != 0 || e.EndsAt != nil || !e.CreatedAt.Equal(now) {
		t.Errorf("entry = %+v", e)
	}
	if _, err := s.Entry(ctx, "other-guild", id); !errors.Is(err, ErrNoEntry) {
		t.Errorf("a case id is guild-scoped, got %v", err)
	}

	if err := s.UpdateReason(ctx, g, id, "clearer"); err != nil {
		t.Fatalf("UpdateReason: %v", err)
	}
	if err := s.Void(ctx, g, id, "mod-2", "oops", now); err != nil {
		t.Fatalf("Void: %v", err)
	}
	if err := s.Void(ctx, g, id, "mod-2", "again", now); !errors.Is(err, ErrNoEntry) {
		t.Errorf("voiding twice should report no such (unvoided) entry, got %v", err)
	}
	e, _ = s.Entry(ctx, g, id)
	if e.Reason != "clearer" || !e.Voided() || e.VoidedBy != "mod-2" || e.VoidReason != "oops" {
		t.Errorf("entry = %+v", e)
	}
	n, err := s.CountScored(ctx, g, []string{"u1"}, now.Add(-time.Hour))
	if err != nil || n != 0 {
		t.Errorf("CountScored after void = %d, %v; want 0", n, err)
	}

	if err := s.SetThreadMessage(ctx, id, "m-1"); err != nil {
		t.Fatalf("SetThreadMessage: %v", err)
	}
	if n, _ := s.CountUnmirrored(ctx, g); n != 0 {
		t.Errorf("CountUnmirrored = %d, want 0", n)
	}
}

func TestPostgresRefIndexMakesIngestionIdempotent(t *testing.T) {
	s, g := testStore(t)
	ctx := context.Background()
	e := Entry{GuildID: g, UserID: "u1", Kind: KindRemoval, Category: CategorySpam, Points: 10,
		ActorID: "system", Source: SourceAIMod, Ref: "inc-1"}
	id1, err := s.Insert(ctx, e)
	if err != nil || id1 == 0 {
		t.Fatalf("first insert = %d, %v", id1, err)
	}
	id2, err := s.Insert(ctx, e)
	if err != nil || id2 != 0 {
		t.Errorf("duplicate ref should be silently not written, got id %d err %v", id2, err)
	}
	// Same ref under a different source is a different event.
	e.Source = SourceDiscord
	if id3, err := s.Insert(ctx, e); err != nil || id3 == 0 {
		t.Errorf("same ref, other source = %d, %v", id3, err)
	}
	// Command rows carry no ref and never collide.
	cmd := Entry{GuildID: g, UserID: "u1", Kind: KindNote, ActorID: "mod-1", Source: SourceCommand}
	for i := 0; i < 2; i++ {
		if id, err := s.Insert(ctx, cmd); err != nil || id == 0 {
			t.Errorf("command insert %d = %d, %v", i, id, err)
		}
	}
	got, _, err := s.EntryByRef(ctx, g, SourceAIMod, "inc-1")
	if err != nil || got.ID != id1 {
		t.Errorf("EntryByRef = %+v, %v", got, err)
	}
	if _, ok, _ := s.EntryByRef(ctx, g, SourceAIMod, "nope"); ok {
		t.Error("EntryByRef found a ref that was never written")
	}
}

func TestPostgresEntriesOrderWindowAndGroup(t *testing.T) {
	s, g := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i, u := range []string{"a", "b", "a", "c"} {
		if _, err := s.Insert(ctx, Entry{GuildID: g, UserID: u, Kind: KindWarn, Points: 10, ActorID: "m",
			Source: SourceCommand, CreatedAt: now.Add(-time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	// Far outside any window.
	if _, err := s.Insert(ctx, Entry{GuildID: g, UserID: "a", Kind: KindWarn, Points: 10, ActorID: "m",
		Source: SourceCommand, CreatedAt: now.Add(-400 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Entries(ctx, g, []string{"a", "b"}, now.Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].UserID != "a" || got[1].UserID != "b" || got[2].UserID != "a" {
		t.Errorf("Entries should be the group's, newest first, inside the window: %+v", got)
	}
	if got, _ := s.Entries(ctx, g, []string{"a"}, now.Add(-24*time.Hour), 1); len(got) != 1 {
		t.Errorf("limit not honoured: %d", len(got))
	}
	if got, _ := s.Entries(ctx, g, nil, now.Add(-24*time.Hour), 10); len(got) != 0 {
		t.Errorf("empty group should read nothing, got %d", len(got))
	}
	if n, _ := s.CountScored(ctx, g, []string{"a", "b", "c"}, now.Add(-24*time.Hour)); n != 4 {
		t.Errorf("CountScored = %d, want 4", n)
	}
}

func TestPostgresBanQueue(t *testing.T) {
	s, g := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	past, future := now.Add(-time.Minute), now.Add(time.Hour)

	due, _ := s.Insert(ctx, Entry{GuildID: g, UserID: "u1", Kind: KindBan, ActorID: "m", Source: SourceCommand, Duration: time.Hour, EndsAt: &past})
	_, _ = s.Insert(ctx, Entry{GuildID: g, UserID: "u2", Kind: KindBan, ActorID: "m", Source: SourceCommand, Duration: time.Hour, EndsAt: &future})
	_, _ = s.Insert(ctx, Entry{GuildID: g, UserID: "u3", Kind: KindBan, ActorID: "m", Source: SourceCommand}) // permanent
	voided, _ := s.Insert(ctx, Entry{GuildID: g, UserID: "u4", Kind: KindBan, ActorID: "m", Source: SourceCommand, Duration: time.Hour, EndsAt: &past})
	_ = s.Void(ctx, g, voided, "m", "x", now)

	if n, _ := s.CountPendingBans(ctx, g); n != 2 {
		t.Errorf("CountPendingBans = %d, want 2 (one due, one future; permanent and voided excluded)", n)
	}
	got, err := s.DueBans(ctx, g, now)
	if err != nil || len(got) != 1 || got[0].ID != due {
		t.Fatalf("DueBans = %+v, %v", got, err)
	}
	if e, ok, _ := s.ActiveBan(ctx, g, "u3"); !ok || e.EndsAt != nil {
		t.Errorf("a permanent ban is active: %+v %v", e, ok)
	}
	if _, ok, _ := s.ActiveBan(ctx, g, "u1"); ok {
		t.Error("an expired temporary ban is not active")
	}

	if err := s.MarkLifted(ctx, due, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.DueBans(ctx, g, now); len(got) != 0 {
		t.Errorf("a lifted ban is no longer due: %+v", got)
	}
	if e, _ := s.Entry(ctx, g, due); e.LiftedAt == nil || e.Standing(now) {
		t.Errorf("lifted ban = %+v", e)
	}
}

func TestPostgresCaseFilesLinksAndHints(t *testing.T) {
	s, g := testStore(t)
	ctx := context.Background()

	if err := s.UpsertCaseFile(ctx, CaseFile{GuildID: g, UserID: "u1", Username: "dana", AvatarHash: "abc"}); err != nil {
		t.Fatal(err)
	}
	// A refresh with blanks keeps what was known.
	if err := s.UpsertCaseFile(ctx, CaseFile{GuildID: g, UserID: "u1", GlobalName: "Dana"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCaseThread(ctx, g, "u1", "t-1"); err != nil {
		t.Fatal(err)
	}
	cf, ok, err := s.CaseFile(ctx, g, "u1")
	if err != nil || !ok || cf.Username != "dana" || cf.GlobalName != "Dana" || cf.AvatarHash != "abc" || cf.ThreadID != "t-1" {
		t.Errorf("case file = %+v %v %v", cf, ok, err)
	}
	_ = s.UpsertCaseFile(ctx, CaseFile{GuildID: g, UserID: "u2"})
	if files, _ := s.CaseFiles(ctx, g, 10); len(files) != 2 || files[0].UserID != "u2" {
		t.Errorf("CaseFiles newest first = %+v", files)
	}

	// Links: u1 and u2 in one group, u3 alone.
	_ = s.Link(ctx, Link{GuildID: g, UserID: "u1", GroupID: "u1", LinkedBy: "m"})
	_ = s.Link(ctx, Link{GuildID: g, UserID: "u2", GroupID: "u1", LinkedBy: "m", Reason: "same avatar"})
	grp, err := s.Group(ctx, g, "u2")
	if err != nil || len(grp) != 2 || grp[0] != "u2" {
		t.Errorf("Group(u2) = %v, %v; want [u2 u1]", grp, err)
	}
	if grp, _ := s.Group(ctx, g, "u3"); len(grp) != 1 || grp[0] != "u3" {
		t.Errorf("an unlinked member is a group of one, got %v", grp)
	}
	if links, _ := s.GroupLinks(ctx, g, "u1"); len(links) != 2 {
		t.Errorf("GroupLinks = %+v", links)
	}
	_ = s.Unlink(ctx, g, "u2")
	if grp, _ := s.Group(ctx, g, "u1"); len(grp) != 1 {
		t.Errorf("after unlink, Group(u1) = %v", grp)
	}

	// Hints: visible from either side, opinion kept across a re-score.
	_ = s.UpsertHint(ctx, AltHint{GuildID: g, UserID: "new", CandidateID: "u1", Signals: []string{"avatar"}, Score: 3, Opinion: "likely"})
	_ = s.UpsertHint(ctx, AltHint{GuildID: g, UserID: "new", CandidateID: "u1", Signals: []string{"avatar", "name"}, Score: 5})
	hints, err := s.Hints(ctx, g, "u1")
	if err != nil || len(hints) != 1 || hints[0].Score != 5 || hints[0].Opinion != "likely" || len(hints[0].Signals) != 2 {
		t.Errorf("Hints(u1) = %+v, %v", hints, err)
	}
	_ = s.DeleteHint(ctx, g, "u1", "new") // reversed pair still deletes
	if hints, _ := s.Hints(ctx, g, "new"); len(hints) != 0 {
		t.Errorf("hint survived deletion: %+v", hints)
	}
}

func TestPostgresChecksRefuseUnknownKindsAndCategories(t *testing.T) {
	s, g := testStore(t)
	ctx := context.Background()
	if _, err := s.Insert(ctx, Entry{GuildID: g, UserID: "u", Kind: Kind("smite"), Category: CategoryOther, ActorID: "m", Source: SourceCommand}); err == nil {
		t.Error("an unknown kind was accepted")
	}
	if _, err := s.Insert(ctx, Entry{GuildID: g, UserID: "u", Kind: KindWarn, Category: Category("vibes"), ActorID: "m", Source: SourceCommand}); err == nil {
		t.Error("an unknown category was accepted")
	}
	if _, err := s.Insert(ctx, Entry{GuildID: g, UserID: "u", Kind: KindWarn, Category: CategoryOther, ActorID: "m", Source: Source("carrier pigeon")}); err == nil {
		t.Error("an unknown source was accepted")
	}
	cfg := defaultConfig(g)
	cfg.EscalationMode = Mode("yolo")
	if err := s.SetConfig(ctx, cfg); err == nil {
		t.Error("an unknown escalation mode was accepted")
	}
}
