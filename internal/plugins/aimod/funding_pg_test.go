package aimod

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/dbtest"
)

// The tip jar's per-rail breakdown is the one thing in this package that the
// fake store cannot vouch for: it is a JSONB column, and every other test in
// here talks to a map in memory. A breakdown that failed to encode or decode
// would leave the totals correct and the public view silently empty, which is
// exactly the kind of failure nobody notices until a donor asks why their
// transfer is not listed.
//
// Skips itself rather than failing when TEST_DATABASE_URL is unset, matching
// internal/settings and internal/storage, so go test ./... still runs
// everywhere with no setup.
func TestFundingBalancesRoundTripThroughPostgres(t *testing.T) {
	pool := dbtest.Pool(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()
	// One shared migrated database, so each test owns its own row space via a
	// guild ID derived from its name, the convention internal/settings uses.
	guild := t.Name()
	at := time.Now().UTC().Truncate(time.Second)

	// A breakdown spanning both decimal scales in the table, since the 18
	// decimal BNB rails are where a bad round trip would be least obvious.
	baseline := map[string]float64{
		"base:USDC":     10.5,
		"polygon:USDT":  2.25,
		"bsc:USDT":      0.000001,
		"ethereum:USDC": 0,
	}
	if err := store.SetFundingAddress(ctx, guild, testWallet, "owner", at, 12.750001, baseline); err != nil {
		t.Fatalf("SetFundingAddress: %v", err)
	}

	got, err := store.Funding(ctx, guild)
	if err != nil {
		t.Fatalf("Funding: %v", err)
	}
	if len(got.Balances) != len(baseline) {
		t.Fatalf("breakdown = %v, want %d rails", got.Balances, len(baseline))
	}
	for key, want := range baseline {
		if math.Abs(got.Balances[key]-want) > 1e-9 {
			t.Errorf("%s = %v, want %v", key, got.Balances[key], want)
		}
	}

	// A later poll replaces the breakdown wholesale rather than merging into
	// it, so a rail that has gone to zero stops being listed at its old value.
	next := map[string]float64{"base:USDC": 20}
	if err := store.UpdateFundingBalance(ctx, guild, 20, 7.249999, next, at.Add(time.Hour)); err != nil {
		t.Fatalf("UpdateFundingBalance: %v", err)
	}
	got, err = store.Funding(ctx, guild)
	if err != nil {
		t.Fatalf("Funding after update: %v", err)
	}
	if len(got.Balances) != 1 || math.Abs(got.Balances["base:USDC"]-20) > 1e-9 {
		t.Fatalf("breakdown = %v, want only base:USDC at 20", got.Balances)
	}
	if got.Donations != 1 {
		t.Fatalf("donations = %d, want 1", got.Donations)
	}

	// A nil breakdown is stored as an empty object, never NULL: the column is
	// NOT NULL, and a poll that legitimately found nothing has to be storable.
	if err := store.UpdateFundingBalance(ctx, guild, 0, 0, nil, at.Add(2*time.Hour)); err != nil {
		t.Fatalf("UpdateFundingBalance with a nil breakdown: %v", err)
	}
	if got, err = store.Funding(ctx, guild); err != nil {
		t.Fatalf("Funding after nil breakdown: %v", err)
	}
	if len(got.Balances) != 0 {
		t.Fatalf("breakdown = %v, want empty", got.Balances)
	}
}
