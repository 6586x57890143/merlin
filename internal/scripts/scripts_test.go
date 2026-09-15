package scripts

import (
	"context"
	"testing"

	"github.com/6586x57890143/merlin/internal/dbtest"
)

// Skips itself when TEST_DATABASE_URL is unset, like every other
// Postgres-backed test here.
func TestScriptSwitchRoundTrip(t *testing.T) {
	store := NewPostgresStore(dbtest.Pool(t))
	ctx := context.Background()
	guild := t.Name()
	// The database outlives the run, so start from a known state rather
	// than trusting the last run's final write.
	if err := store.SetEnabled(ctx, guild, "eternal-role", false); err != nil {
		t.Fatalf("reset: %v", err)
	}

	on, err := store.Enabled(ctx, guild, "eternal-role")
	if err != nil || on {
		t.Fatalf("zero state must be off: on=%v err=%v", on, err)
	}
	for _, want := range []bool{true, true, false, false, true, false} {
		if err := store.SetEnabled(ctx, guild, "eternal-role", want); err != nil {
			t.Fatalf("set %v: %v", want, err)
		}
		if on, err := store.Enabled(ctx, guild, "eternal-role"); err != nil || on != want {
			t.Fatalf("after set %v: on=%v err=%v", want, on, err)
		}
	}
	if on, _ := store.Enabled(ctx, guild, "other-script"); on {
		t.Fatal("switches are per script")
	}
}
