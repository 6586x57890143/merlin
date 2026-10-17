package audit

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/6586x57890143/cygnet/ledger"
	"github.com/6586x57890143/cygnet/ledger/ledgertest"

	"github.com/6586x57890143/merlin/internal/dbtest"
)

// audit_log keeps the ledger's contract: one unbroken chain per guild,
// however many append at once, read back so that it still verifies. Skips
// itself when TEST_DATABASE_URL is unset, like every other Postgres test.
func TestStoreKeepsTheLedgerContract(t *testing.T) {
	ledgertest.Run(t, func(t *testing.T) ledger.Store { return NewStore(dbtest.Pool(t)) })
}

// Record keeps the call site's own strings beside the entry, and its
// actor sentinel, so the row stays greppable the way it always was.
func TestRecordKeepsTheLegacyColumns(t *testing.T) {
	pool := dbtest.Pool(t)
	guild := t.Name() + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	w := writer(&wire{}, channels{guild: "2"})
	w.pool, w.store = pool, NewStore(pool)
	ctx := context.Background()
	if err := w.Record(ctx, guild, "system", "roles.release", "", "user=<@1> restored=[<@&2>]"); err != nil {
		t.Fatal(err)
	}
	var actor, action, newValue string
	var seq int64
	if err := pool.QueryRow(ctx, `SELECT actor_id, action, new_value, seq FROM audit_log WHERE guild_id = $1`, guild).
		Scan(&actor, &action, &newValue, &seq); err != nil {
		t.Fatal(err)
	}
	if actor != "system" || action != "roles.release" || newValue != "user=<@1> restored=[<@&2>]" || seq != 1 {
		t.Fatalf("row %q %q %q %d", actor, action, newValue, seq)
	}
	rs, err := w.store.Range(ctx, guild, 0, 10)
	if err != nil || len(rs) != 1 || ledger.Verify(ledger.Hash{}, rs) != nil || rs[0].Entry.Actor.Kind != ledger.ActorAuto {
		t.Fatalf("chain %+v %v", rs, err)
	}
}
