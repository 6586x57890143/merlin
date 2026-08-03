package settings

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// No database needed for either of these: both fail before ImportFromLegacyYAML
// touches Postgres.

func TestImportFromLegacyYAMLMissingFileReturnsAnError(t *testing.T) {
	store := New(nil, nil) // pool/bus never reached; the read fails first
	_, err := store.ImportFromLegacyYAML(context.Background(), filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected an error reading a file that does not exist")
	}
}

func TestImportFromLegacyYAMLMalformedYAMLReturnsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("guilds: [this is not a map"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	store := New(nil, nil)
	_, err := store.ImportFromLegacyYAML(context.Background(), path)
	if err == nil {
		t.Fatal("expected an error parsing malformed YAML")
	}
}

// The happy path exercises every field the legacy format carries, including
// the two unit conversions (interval_hours -> minutes, retention_days ->
// hours) this code performs on the way in, and confirms Disclosure -- a
// field legacy YAML has never heard of -- lands as the normalized default
// rather than an empty string the CHECK constraint would have rejected.
func TestImportFromLegacyYAMLWritesEveryField(t *testing.T) {
	// store's own guild ID is used as the YAML's guild key, rather than a
	// literal like "g1", so setupStore's t.Cleanup (registered against that
	// same ID) actually deletes the rows this test writes. A mismatched
	// literal would leave every run's data behind for the next local run to
	// collide with, the exact bug this harness exists to avoid.
	store, _, guildID := setupStore(t)

	yamlDoc := `
guilds:
  "` + guildID + `":
    mod_role_ids: ["role-1", "role-2"]
    admin_user_ids: ["user-1"]
    audit_log_channel_id: "audit-chan"
    status_channel_id: "status-chan"
    rotating_channels:
      - channel_id: "chan-1"
        interval_hours: 24
        archive_category_id: "cat-1"
        archive_visibility: "mod_only"
        retention_days: 7
        sticky:
          enabled: true
          messages: ["welcome", "rules"]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.yaml")
	if err := os.WriteFile(path, []byte(yamlDoc), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	imported, err := store.ImportFromLegacyYAML(context.Background(), path)
	if err != nil {
		t.Fatalf("ImportFromLegacyYAML: %v", err)
	}
	if len(imported) != 1 || imported[0] != guildID {
		t.Fatalf("imported = %v, want [%s]", imported, guildID)
	}

	if got := store.ModRoleIDs(guildID); !stringSlicesEqual(got, []string{"role-1", "role-2"}) {
		t.Errorf("ModRoleIDs = %v, want [role-1 role-2]", got)
	}
	if got := store.AdminUserIDs(guildID); !stringSlicesEqual(got, []string{"user-1"}) {
		t.Errorf("AdminUserIDs = %v, want [user-1]", got)
	}
	gs := store.GuildSettings(guildID)
	if gs.AuditLogChannelID != "audit-chan" {
		t.Errorf("AuditLogChannelID = %q, want audit-chan", gs.AuditLogChannelID)
	}
	if gs.StatusChannelID != "status-chan" {
		t.Errorf("StatusChannelID = %q, want status-chan", gs.StatusChannelID)
	}

	rc, ok := store.RotationChannel(guildID, "chan-1")
	if !ok {
		t.Fatal("the imported rotating channel was not found")
	}
	if rc.IntervalMinutes != 24*60 {
		t.Errorf("IntervalMinutes = %d, want %d (interval_hours * 60)", rc.IntervalMinutes, 24*60)
	}
	if rc.RetentionHours == nil || *rc.RetentionHours != 7*24 {
		t.Errorf("RetentionHours = %v, want %d (retention_days * 24)", rc.RetentionHours, 7*24)
	}
	if rc.ArchiveCategoryID != "cat-1" {
		t.Errorf("ArchiveCategoryID = %q, want cat-1", rc.ArchiveCategoryID)
	}
	if !rc.StickyEnabled || !stringSlicesEqual(rc.StickyMessages, []string{"welcome", "rules"}) {
		t.Errorf("sticky = enabled:%v messages:%v, want enabled:true messages:[welcome rules]", rc.StickyEnabled, rc.StickyMessages)
	}
	// Legacy YAML has no notice_lead_minutes or disclosure key at all, so
	// both must land as their store-level defaults rather than the illegal
	// empty value.
	if rc.NoticeLeadMinutes != 0 {
		t.Errorf("NoticeLeadMinutes = %d, want 0 (off; legacy format never had this concept)", rc.NoticeLeadMinutes)
	}
	if rc.Disclosure != DisclosureFull {
		t.Errorf("Disclosure = %q, want full (legacy format predates disclosure modes)", rc.Disclosure)
	}
}

// Deliberately not tested: a mid-import failure leaving only some guilds
// committed. doc.Guilds is a Go map, so iteration order is nondeterministic,
// and an assertion about *which* guild survives a partial failure can't be
// written reliably. The no-transaction, no-rollback behavior itself is
// already visible in ImportFromLegacyYAML's own doc comment and return
// signature (importedGuilds is returned alongside a non-nil err).

