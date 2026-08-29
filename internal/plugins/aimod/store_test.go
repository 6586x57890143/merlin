package aimod

import (
	"context"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/dbtest"
	"github.com/6586x57890143/merlin/internal/storage"
)

// The Postgres store, against a real migrated database.
//
// These skip rather than fail when TEST_DATABASE_URL is unset, so
// `go test ./...` still runs everywhere with no setup, and CI's lint-test job
// (which runs a postgres:16-alpine service) is where they actually execute.
// Same harness and same bargain as internal/settings and internal/storage.
//
// Worth having as real tests rather than fakes: fakes_test.go's fakeStore is
// what every other test in this package runs against, and it agrees with
// pgStore only by inspection. The things that can differ are exactly the ones
// that hurt, because they are silent: a JSONB column that round-trips to nil
// instead of an empty slice, an upsert that clobbers a column it should have
// left alone, a CHECK constraint nobody exercised, a query whose WHERE clause
// reads the wrong way round.

// testStore gives each test its own migrated schema.
//
// FreshSchema rather than Pool, plus a Migrate of its own: Pool hands back one
// shared already-migrated database and asks callers to keep out of each
// other's way by deriving guild IDs from t.Name(), which works but makes every
// assertion here carry a naming convention that has nothing to do with what it
// is testing. FreshSchema is empty by design (it exists so storage's own tests
// can watch Migrate run from scratch), so migrating it costs one pass over 23
// files per test and buys real isolation.
func testStore(t *testing.T) *pgStore {
	t.Helper()
	pool := dbtest.FreshSchema(t)
	if err := storage.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate fresh schema: %v", err)
	}
	return &pgStore{pool: pool}
}

// A guild with no row is a working state, not an error: this plugin does
// nothing at all until an admin asks it to, so the first read has to hand
// back the safe defaults rather than fail.
func TestConfigDefaultsForAnUnknownGuild(t *testing.T) {
	s := testStore(t)
	cfg, err := s.Config(context.Background(), "g-never-seen")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.Mode != ModeOff {
		t.Errorf("mode = %q, want off: a new guild must scan nothing until asked", cfg.Mode)
	}
	if cfg.CalibrationMode != CalibrationSuggest {
		t.Errorf("calibration mode = %q, want suggest to match the column default", cfg.CalibrationMode)
	}
	if len(cfg.Calibration) != 0 || len(cfg.CalibrationPending) != 0 {
		t.Error("a guild that has never been reviewed came back carrying calibration")
	}
	if !cfg.CalibrationRanAt.IsZero() {
		t.Error("a guild that has never been reviewed came back with a run timestamp")
	}
}

// Every scalar setter goes through the same upsert, and every one of them has
// to create the row: a guild's first /aimod command is as likely to be
// `configure budget` as `configure key`. Written as one table because the
// failure they share (an upsert that only updates) would hit all of them.
func TestEverySetterCreatesTheRowAndRoundTrips(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		set   func(*pgStore, string) error
		check func(Config) bool
	}{
		{"api key", func(s *pgStore, g string) error { return s.SetAPIKey(ctx, g, []byte("sealed")) },
			func(c Config) bool { return string(c.APIKeySealed) == "sealed" }},
		{"mode", func(s *pgStore, g string) error { return s.SetMode(ctx, g, ModeEnforce) },
			func(c Config) bool { return c.Mode == ModeEnforce }},
		{"budget", func(s *pgStore, g string) error { return s.SetBudget(ctx, g, 2.5) },
			func(c Config) bool { return c.DailyBudgetUSD == 2.5 }},
		{"evidence", func(s *pgStore, g string) error { return s.SetEvidenceHours(ctx, g, 72) },
			func(c Config) bool { return c.EvidenceHours == 72 }},
		{"models", func(s *pgStore, g string) error { return s.SetModels(ctx, g, []string{"a/b"}, []string{"c/d"}) },
			func(c Config) bool { return len(c.FastModels) == 1 && len(c.DeepModels) == 1 }},
		{"exempt channels", func(s *pgStore, g string) error { return s.SetExemptChannels(ctx, g, []string{"c1"}) },
			func(c Config) bool { return len(c.ExemptChannelIDs) == 1 }},
		{"exempt roles", func(s *pgStore, g string) error { return s.SetExemptRoles(ctx, g, []string{"r1"}) },
			func(c Config) bool { return len(c.ExemptRoleIDs) == 1 }},
		{"sanction action", func(s *pgStore, g string) error { return s.SetSanctionAction(ctx, g, SanctionJail) },
			func(c Config) bool { return c.SanctionAction == SanctionJail }},
		{"sanction opt-in", func(s *pgStore, g string) error { return s.SetSanctionOptIn(ctx, g, []string{"u1"}) },
			func(c Config) bool { return len(c.SanctionOptInUserIDs) == 1 }},
		{"calibration mode", func(s *pgStore, g string) error { return s.SetCalibrationMode(ctx, g, CalibrationAuto) },
			func(c Config) bool { return c.CalibrationMode == CalibrationAuto }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			if err := tc.set(s, "g1"); err != nil {
				t.Fatalf("set: %v", err)
			}
			cfg, err := s.Config(ctx, "g1")
			if err != nil {
				t.Fatalf("Config: %v", err)
			}
			if !tc.check(cfg) {
				t.Errorf("value did not round-trip: %+v", cfg)
			}
		})
	}
}

// SetBucketAction merges into the existing JSONB rather than replacing it,
// which is what lets a guild set one policy without silently resetting the
// nine it had already configured.
func TestBucketActionsMergeRatherThanReplace(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	if err := s.SetBucketAction(ctx, "g1", BucketHateSpeech, ActionRewrite); err != nil {
		t.Fatalf("SetBucketAction: %v", err)
	}
	if err := s.SetBucketAction(ctx, "g1", BucketGore, ActionFlag); err != nil {
		t.Fatalf("SetBucketAction: %v", err)
	}

	cfg, err := s.Config(ctx, "g1")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if got := EffectiveAction(cfg.BucketActions, BucketHateSpeech); got != ActionRewrite {
		t.Errorf("hate_speech = %q, want rewrite: the second write clobbered the first", got)
	}
	if got := EffectiveAction(cfg.BucketActions, BucketGore); got != ActionFlag {
		t.Errorf("gore = %q, want flag", got)
	}
}

// The three calibration columns move together, which is why they are one
// setter: split up, an apply that half-failed would leave a guild enforcing a
// set it had also kept queued.
func TestCalibrationRoundTripsAndClears(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ran := time.Date(2026, 3, 1, 4, 0, 0, 0, time.UTC)

	active := []CalibrationExample{{Text: "this is retarded", Bucket: BucketHateSpeech, Note: "generic profanity"}}
	pending := []CalibrationExample{{Text: "a slur alone", Bucket: BucketHateSpeech, ShouldAct: true}}
	if err := s.SetCalibration(ctx, "g1", active, pending, ran); err != nil {
		t.Fatalf("SetCalibration: %v", err)
	}

	cfg, err := s.Config(ctx, "g1")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if len(cfg.Calibration) != 1 || cfg.Calibration[0].Text != "this is retarded" {
		t.Errorf("active = %+v, want the stored example", cfg.Calibration)
	}
	if len(cfg.CalibrationPending) != 1 || !cfg.CalibrationPending[0].ShouldAct {
		t.Errorf("pending = %+v, want the proposal with its verdict intact", cfg.CalibrationPending)
	}
	if !cfg.CalibrationRanAt.Equal(ran) {
		t.Errorf("ran at = %v, want %v", cfg.CalibrationRanAt, ran)
	}

	// Clearing stores '[]' rather than leaving the old rows or writing null,
	// and a zero timestamp must not erase when the last review happened.
	if err := s.SetCalibration(ctx, "g1", nil, nil, time.Time{}); err != nil {
		t.Fatalf("SetCalibration clear: %v", err)
	}
	cfg, err = s.Config(ctx, "g1")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if len(cfg.Calibration) != 0 || len(cfg.CalibrationPending) != 0 {
		t.Errorf("clear left %d active and %d pending", len(cfg.Calibration), len(cfg.CalibrationPending))
	}
	if !cfg.CalibrationRanAt.Equal(ran) {
		t.Errorf("clearing the set also erased when the review last ran: %v", cfg.CalibrationRanAt)
	}
}

// The CHECK constraint from migration 0023, which is the backstop against a
// hand-edited row rather than something the command layer can produce.
func TestCalibrationModeRejectsAnUnknownValue(t *testing.T) {
	s := testStore(t)
	if err := s.SetCalibrationMode(context.Background(), "g1", CalibrationMode("nonsense")); err == nil {
		t.Error("the database accepted a calibration mode outside the CHECK constraint")
	}
}

// An incident is written before the message is touched, so it has to survive
// the round trip completely: /aimod why reads the reason and /aimod undo
// reads the stored text.
func TestIncidentRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)

	id, err := s.RecordIncident(ctx, Incident{
		GuildID: "g1", ChannelID: "c1", MessageID: "m1", AuthorID: "u1",
		Bucket: BucketThreats, Action: ActionRemove, Confidence: 0.91,
		Reason: "a specific person, a specific act", Content: "the original words",
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}

	inc, err := s.IncidentByMessage(ctx, "g1", "m1")
	if err != nil {
		t.Fatalf("IncidentByMessage: %v", err)
	}
	if inc.ID != id || inc.Bucket != BucketThreats || inc.Action != ActionRemove {
		t.Errorf("incident = %+v, want the one just written", inc)
	}
	if inc.Content != "the original words" {
		t.Error("the evidence did not round-trip, so /aimod undo has nothing to restore")
	}
	if inc.Undone {
		t.Error("a fresh incident came back already reversed")
	}

	if err := s.MarkUndone(ctx, id); err != nil {
		t.Fatalf("MarkUndone: %v", err)
	}
	if inc, err = s.IncidentByMessage(ctx, "g1", "m1"); err != nil || !inc.Undone {
		t.Errorf("after MarkUndone: undone = %v, err = %v", inc.Undone, err)
	}

	if err := s.MarkActioned(ctx, id, ActionFlag); err != nil {
		t.Fatalf("MarkActioned: %v", err)
	}
	if inc, err = s.IncidentByMessage(ctx, "g1", "m1"); err != nil || inc.Action != ActionFlag {
		t.Errorf("after MarkActioned: action = %q, err = %v", inc.Action, err)
	}
}

// The sentinel /aimod why and /aimod undo get for a message this plugin never
// touched, which has to be distinguishable from a database failure.
func TestIncidentByMessageReportsAbsenceAsItsOwnError(t *testing.T) {
	_, err := testStore(t).IncidentByMessage(context.Background(), "g1", "never-seen")
	if err != ErrNoIncident {
		t.Errorf("err = %v, want ErrNoIncident", err)
	}
}

// CountSanctions is what the escalation ladder multiplies by, and both of its
// exclusions matter: counting flags escalates somebody for being argued
// about, and counting a reversal means one false positive lengthens every
// sentence that member ever gets afterwards.
func TestCountSanctionsExcludesFlagsAndReversals(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC()

	write := func(msgID string, action Action, undone bool) {
		t.Helper()
		id, err := s.RecordIncident(ctx, Incident{
			GuildID: "g1", ChannelID: "c1", MessageID: msgID, AuthorID: "u1",
			Bucket: BucketThreats, Action: action, CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("RecordIncident: %v", err)
		}
		if undone {
			if err := s.MarkUndone(ctx, id); err != nil {
				t.Fatalf("MarkUndone: %v", err)
			}
		}
	}
	write("m1", ActionRemove, false)
	write("m2", ActionFlag, false)
	write("m3", ActionRemove, true)

	n, err := s.CountSanctions(ctx, "g1", "u1", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("CountSanctions: %v", err)
	}
	if n != 1 {
		t.Errorf("counted %d sanctions, want 1: flags and reversals must not count", n)
	}

	// And the cutoff is honoured, or a member's history never ages out.
	if n, err = s.CountSanctions(ctx, "g1", "u1", now.Add(time.Hour)); err != nil || n != 0 {
		t.Errorf("counted %d past the cutoff (err %v), want 0", n, err)
	}
}

// PendingFlags is what the abuse path clears: flags only, never something
// already acted on, and never something a moderator restored.
func TestPendingFlagsIsNarrow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC()

	for _, tc := range []struct {
		msgID  string
		action Action
	}{{"m1", ActionFlag}, {"m2", ActionRemove}} {
		if _, err := s.RecordIncident(ctx, Incident{
			GuildID: "g1", ChannelID: "c1", MessageID: tc.msgID, AuthorID: "u1",
			Bucket: BucketThreats, Action: tc.action, CreatedAt: now,
		}); err != nil {
			t.Fatalf("RecordIncident: %v", err)
		}
	}

	flags, err := s.PendingFlags(ctx, "g1", "u1", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("PendingFlags: %v", err)
	}
	if len(flags) != 1 || flags[0].MessageID != "m1" {
		t.Errorf("flags = %+v, want only the unactioned one", flags)
	}
}

// IncidentsSince is the calibration review's input, and is deliberately NOT
// filtered the way CountSanctions is: a reversal is the most informative row
// in the table, because it is the only human-labelled data in the system.
func TestIncidentsSinceKeepsReversalsAndHonoursTheLimit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC()

	for _, msgID := range []string{"m1", "m2", "m3"} {
		id, err := s.RecordIncident(ctx, Incident{
			GuildID: "g1", ChannelID: "c1", MessageID: msgID, AuthorID: "u1",
			Bucket: BucketHateSpeech, Action: ActionRemove, CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("RecordIncident: %v", err)
		}
		if msgID == "m2" {
			if err := s.MarkUndone(ctx, id); err != nil {
				t.Fatalf("MarkUndone: %v", err)
			}
		}
	}

	all, err := s.IncidentsSince(ctx, "g1", now.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("IncidentsSince: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d incidents, want all 3 including the reversal", len(all))
	}
	var sawReversal bool
	for _, inc := range all {
		sawReversal = sawReversal || inc.Undone
	}
	if !sawReversal {
		t.Error("the reversal was filtered out, losing the only human-labelled row")
	}

	capped, err := s.IncidentsSince(ctx, "g1", now.Add(-time.Hour), 2)
	if err != nil {
		t.Fatalf("IncidentsSince: %v", err)
	}
	if len(capped) != 2 {
		t.Errorf("got %d incidents for a limit of 2", len(capped))
	}
}

// Spend is what the daily cap reads, and it accumulates rather than
// overwriting: a budget that only remembered the last call is one a busy
// guild walks straight through.
func TestSpendAccumulatesAndSplitsByTier(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	day := today(time.Now().UTC())

	fast := Usage{PromptTokens: 900, CompletionTokens: 20, Cost: 0.001}
	deep := Usage{PromptTokens: 4000, CompletionTokens: 200, Cost: 0.02}
	deep.CompletionTokensDetails.ReasoningTokens = 150

	for range 2 {
		if err := s.AddSpend(ctx, "g1", day, fast, false); err != nil {
			t.Fatalf("AddSpend fast: %v", err)
		}
	}
	if err := s.AddSpend(ctx, "g1", day, deep, true); err != nil {
		t.Fatalf("AddSpend deep: %v", err)
	}
	if err := s.AddScanned(ctx, "g1", day, 40); err != nil {
		t.Fatalf("AddScanned: %v", err)
	}

	sp, err := s.SpendToday(ctx, "g1", day)
	if err != nil {
		t.Fatalf("SpendToday: %v", err)
	}
	if sp.FastCalls != 2 || sp.DeepCalls != 1 {
		t.Errorf("calls: fast=%d deep=%d, want 2 and 1", sp.FastCalls, sp.DeepCalls)
	}
	if sp.Scanned != 40 {
		t.Errorf("scanned = %d, want 40: this is the denominator every cost estimate divides by", sp.Scanned)
	}
	if sp.FastPromptTokens != 1800 || sp.DeepPromptTokens != 4000 {
		t.Errorf("tokens landed in the wrong tier: %+v", sp)
	}
	// Spend.ReasoningTokens is documented as the share of the completion that
	// was the model thinking, "broken out because it is the one part of the
	// bill that buys nothing a JSON schema does not already pin down". It is
	// currently dead: no column, no write in AddSpend, no read in SpendToday,
	// and no consumer, so the value OpenRouter returns is dropped. Asserted as
	// zero rather than left unmentioned so the gap is written down where
	// somebody wiring it up will look.
	if sp.ReasoningTokens != 0 {
		t.Errorf("reasoning tokens = %d: the column now exists, so this assertion is stale", sp.ReasoningTokens)
	}
	if sp.SpentUSD < 0.021 {
		t.Errorf("spent = %v, want every call's cost added", sp.SpentUSD)
	}

	since, err := s.SpendSince(ctx, "g1", day.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("SpendSince: %v", err)
	}
	if len(since) != 1 {
		t.Errorf("SpendSince returned %d days, want 1", len(since))
	}
}

// Evidence retention is re-derived per guild on every prune, never frozen
// into a per-row column. That is the rotation_archives.delete_after mistake
// pointing the other way, and here the direction that has to work is
// shortening the window: a guild that drops to 1 hour expects yesterday's
// text gone, not kept on the schedule that applied when it was written.
func TestPruneEvidenceUsesEachGuildsCurrentWindow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	old := time.Now().UTC().Add(-48 * time.Hour)

	// g1 keeps a week, so its old incident survives. g2 keeps an hour.
	if err := s.SetEvidenceHours(ctx, "g1", 24*7); err != nil {
		t.Fatalf("SetEvidenceHours: %v", err)
	}
	if err := s.SetEvidenceHours(ctx, "g2", 1); err != nil {
		t.Fatalf("SetEvidenceHours: %v", err)
	}
	for _, g := range []string{"g1", "g2"} {
		if _, err := s.RecordIncident(ctx, Incident{
			GuildID: g, ChannelID: "c1", MessageID: "m-" + g, AuthorID: "u1",
			Bucket: BucketThreats, Action: ActionRemove,
			Content: "the original words", CreatedAt: old,
		}); err != nil {
			t.Fatalf("RecordIncident: %v", err)
		}
	}

	n, err := s.PruneEvidence(ctx)
	if err != nil {
		t.Fatalf("PruneEvidence: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want only the guild whose window had passed", n)
	}

	kept, err := s.IncidentByMessage(ctx, "g1", "m-g1")
	if err != nil {
		t.Fatalf("IncidentByMessage: %v", err)
	}
	if kept.Content == "" {
		t.Error("pruned a guild that keeps evidence for a week")
	}
	gone, err := s.IncidentByMessage(ctx, "g2", "m-g2")
	if err != nil {
		t.Fatalf("IncidentByMessage: %v", err)
	}
	if gone.Content != "" {
		t.Error("kept text past a guild's own one hour window")
	}

	// PruneBefore takes the housekeeping tick without taking its cutoff,
	// which is what lets the schedule be shared while the policy stays per
	// guild. Nothing is left to prune, so it reports nothing.
	if n, err = s.PruneBefore(ctx, time.Now().UTC()); err != nil || n != 0 {
		t.Errorf("PruneBefore = (%d, %v), want (0, nil)", n, err)
	}
}

// Two guilds must never read each other's configuration or incidents. Cheap
// to assert and the kind of thing a mistyped WHERE clause breaks silently.
func TestGuildsAreIsolated(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	if err := s.SetMode(ctx, "g1", ModeEnforce); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	cfg, err := s.Config(ctx, "g2")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.Mode != ModeOff {
		t.Errorf("g2 mode = %q, want off: it read g1's row", cfg.Mode)
	}

	if _, err := s.RecordIncident(ctx, Incident{
		GuildID: "g1", ChannelID: "c1", MessageID: "m1", AuthorID: "u1",
		Bucket: BucketThreats, Action: ActionRemove, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}
	if _, err := s.IncidentByMessage(ctx, "g2", "m1"); err != ErrNoIncident {
		t.Errorf("g2 found g1's incident: %v", err)
	}
}

// The production constructor, read through the Store interface rather than
// the concrete type. cmd/bot only ever sees the interface, so this is the
// shape that actually has to work.
func TestNewPostgresStoreReadsThroughTheInterface(t *testing.T) {
	store := NewPostgresStore(testStore(t).pool)
	cfg, err := store.Config(context.Background(), "g1")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.GuildID != "g1" || cfg.Mode != ModeOff {
		t.Errorf("config = %+v, want the safe defaults for an unconfigured guild", cfg)
	}
}
