package storage_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/6586x57890143/merlin/internal/dbtest"
	"github.com/6586x57890143/merlin/internal/storage"
)

func TestRoleAccessMigrationDefaultsExistingListsToReadOnly(t *testing.T) {
	pool, ctx := dbtest.FreshSchema(t), context.Background()
	// Apply the exact old schema, seed legacy settings, then use the real
	// migrator to upgrade it. This also checks migration version bookkeeping.
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".up.sql") || name >= "0051" {
			continue
		}
		body, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Migrate's own table is separate from application migrations.
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version BIGINT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now());
	INSERT INTO schema_migrations (version) SELECT generate_series(1,50);
	INSERT INTO settings_guild (guild_id,jail_allowed_channel_ids,vacation_allowed_channel_ids) VALUES ('existing',ARRAY['jail-room'],ARRAY['beach']);`); err != nil {
		t.Fatal(err)
	}
	if err := storage.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var jail, vacation, jailConversation, vacationConversation []string
	if err := pool.QueryRow(ctx, `SELECT jail_allowed_channel_ids,vacation_allowed_channel_ids,jail_conversation_channel_ids,vacation_conversation_channel_ids FROM settings_guild WHERE guild_id='existing'`).Scan(&jail, &vacation, &jailConversation, &vacationConversation); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(jail, []string{"jail-room"}) || !slices.Equal(vacation, []string{"beach"}) || len(jailConversation)+len(vacationConversation) != 0 {
		t.Fatalf("migration escalated legacy access: %v %v %v %v", jail, vacation, jailConversation, vacationConversation)
	}
	if err := pool.QueryRow(ctx, `SELECT vacation_allowed_channel_ids FROM settings_guild WHERE guild_id='1459404984943644747'`).Scan(&vacation); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(vacation, []string{"1549902953631195227"}) {
		t.Fatal("implicit beach was not persisted as read-only")
	}
	if _, err := pool.Exec(ctx, `UPDATE settings_guild SET jail_conversation_channel_ids=jail_allowed_channel_ids WHERE guild_id='existing'`); err == nil {
		t.Fatal("database accepted conflicting modes")
	}
}
