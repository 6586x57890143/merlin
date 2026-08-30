package aimod

import (
	"context"
	"testing"

	"github.com/6586x57890143/merlin/internal/dbtest"
)

// The triage model is a 32KB block of float32s in a BYTEA column, and the fake
// store hands the same slice straight back, so nothing else in this package
// proves it survives Postgres. A blob that came back short or reordered would
// be refused by decodeTriageModel and the guild would silently warm up again
// on every restart, which looks exactly like the rung not working.
//
// Skips itself when TEST_DATABASE_URL is unset, matching internal/settings and
// internal/storage, so go test ./... still runs everywhere with no setup.
func TestTriageModelRoundTripsThroughPostgres(t *testing.T) {
	pool := dbtest.Pool(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()
	guild := t.Name()

	m := newTriageModel()
	trainTriage(m, 60)
	raw, examples := encodeTriageModel(m)

	if err := store.SaveTriageModel(ctx, guild, raw, examples); err != nil {
		t.Fatalf("SaveTriageModel: %v", err)
	}
	gotRaw, gotExamples, err := store.TriageModel(ctx, guild)
	if err != nil {
		t.Fatalf("TriageModel: %v", err)
	}
	if gotExamples != examples {
		t.Fatalf("examples = %d, want %d", gotExamples, examples)
	}

	restored, ok := decodeTriageModel(gotRaw, gotExamples)
	if !ok {
		t.Fatalf("a model that round-tripped through Postgres was refused (%d bytes)", len(gotRaw))
	}
	// Compared on a score rather than byte by byte, because a score is what
	// the rung actually acts on: a blob that survived but decoded to different
	// weights would be the failure this test exists to catch.
	for _, s := range append(append([]string{}, triageClean...), triageFlagged...) {
		want, got := m.Score(triageFeatures(s)), restored.Score(triageFeatures(s))
		if want != got {
			t.Fatalf("score for %q changed across storage: %v -> %v", s, want, got)
		}
	}

	// Saving again replaces rather than appends, so a guild has one model and
	// the column does not grow without bound.
	trainTriage(m, 5)
	raw2, examples2 := encodeTriageModel(m)
	if err := store.SaveTriageModel(ctx, guild, raw2, examples2); err != nil {
		t.Fatalf("second SaveTriageModel: %v", err)
	}
	gotRaw, gotExamples, err = store.TriageModel(ctx, guild)
	if err != nil {
		t.Fatalf("TriageModel after replace: %v", err)
	}
	if len(gotRaw) != len(raw) || gotExamples != examples2 {
		t.Fatalf("replace produced %d bytes / %d examples, want %d / %d",
			len(gotRaw), gotExamples, len(raw), examples2)
	}

	// A guild that has never saved is not an error: it is a fresh model, which
	// skips nothing until it has warmed up.
	missingRaw, missingExamples, err := store.TriageModel(ctx, guild+"-never-saved")
	if err != nil {
		t.Fatalf("a guild with no stored model should not error: %v", err)
	}
	if len(missingRaw) != 0 || missingExamples != 0 {
		t.Fatalf("a guild with no stored model returned %d bytes / %d examples", len(missingRaw), missingExamples)
	}
}

// triage_mode is guild configuration on aimod_config, so it has to survive the
// same read path every other setting uses, including the default that applies
// to a guild nobody has configured.
func TestTriageModePersists(t *testing.T) {
	pool := dbtest.Pool(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()
	guild := t.Name()

	// The default is shadow, for the same reason calibration defaults to
	// suggest: this changes what gets looked at, so a guild watches it work
	// rather than discovering it.
	cfg, err := store.Config(ctx, guild)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.TriageMode != TriageShadow {
		t.Fatalf("default triage mode = %q, want %q", cfg.TriageMode, TriageShadow)
	}

	for _, mode := range []TriageMode{TriageOn, TriageOff, TriageShadow} {
		if err := store.SetTriageMode(ctx, guild, mode); err != nil {
			t.Fatalf("SetTriageMode(%q): %v", mode, err)
		}
		cfg, err := store.Config(ctx, guild)
		if err != nil {
			t.Fatalf("Config after %q: %v", mode, err)
		}
		if cfg.TriageMode != mode {
			t.Errorf("triage mode = %q, want %q", cfg.TriageMode, mode)
		}
	}
}
