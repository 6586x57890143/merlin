package rotation

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func timePtr(t time.Time) *time.Time { return &t }

func setupSweep(t *testing.T) (*fakeOps, *fakeArchiveStore, *fakeAudit, *Plugin) {
	t.Helper()
	fs := newFakeSettings()
	fs.modRoles["g1"] = []string{"modrole1"}
	_ = fs.UpsertRotationChannel(context.Background(), finiteRetentionRC())

	ops := newFakeOps()
	archives := newFakeArchiveStore()
	audit := &fakeAudit{}
	p := newTestPlugin(ops, archives, audit, fs, fixedNow)
	return ops, archives, audit, p
}

func TestSweepDeletesDueArchive(t *testing.T) {
	ops, archives, audit, p := setupSweep(t)
	ops.addChannel(&discordgo.Channel{ID: "arch1", GuildID: "g1", Name: "general-chat-archive-x", ParentID: "archivecat"})
	archives.records["arch1"] = ArchiveRecord{
		ChannelID: "arch1", GuildID: "g1", SourceChannelID: "old1", ArchiveCategoryID: "archivecat",
		ArchivedAt: fixedNow.AddDate(0, 0, -8), DeleteAfter: timePtr(fixedNow.AddDate(0, 0, -1)),
	}

	if err := p.sweep(context.Background(), "g1"); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := ops.Channel("arch1"); err == nil {
		t.Fatal("expected the due archived channel to be deleted")
	}
	if _, ok := archives.records["arch1"]; ok {
		t.Fatal("expected the archive row to be removed after deletion")
	}
	if len(audit.records) != 1 || audit.records[0].action != "archive.deleted" {
		t.Fatalf("expected 1 archive.deleted audit record, got %+v", audit.records)
	}
}

func TestSweepSkipsNotYetDueArchive(t *testing.T) {
	ops, archives, _, p := setupSweep(t)
	ops.addChannel(&discordgo.Channel{ID: "arch1", GuildID: "g1", Name: "general-chat-archive-x", ParentID: "archivecat"})
	archives.records["arch1"] = ArchiveRecord{
		ChannelID: "arch1", GuildID: "g1", SourceChannelID: "old1", ArchiveCategoryID: "archivecat",
		ArchivedAt: fixedNow, DeleteAfter: timePtr(fixedNow.AddDate(0, 0, 30)),
	}

	if err := p.sweep(context.Background(), "g1"); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := ops.Channel("arch1"); err != nil {
		t.Fatal("expected a not-yet-due archived channel to remain")
	}
	if _, ok := archives.records["arch1"]; !ok {
		t.Fatal("expected the archive row to remain")
	}
}

func TestSweepRescuesChannelMovedOutOfArchiveCategory(t *testing.T) {
	ops, archives, _, p := setupSweep(t)
	// A mod moved the archived channel out of archivecat — this should be
	// treated as an implicit "keep it," not deleted.
	ops.addChannel(&discordgo.Channel{ID: "arch1", GuildID: "g1", Name: "general-chat-archive-x", ParentID: "some-other-category"})
	archives.records["arch1"] = ArchiveRecord{
		ChannelID: "arch1", GuildID: "g1", SourceChannelID: "old1", ArchiveCategoryID: "archivecat",
		ArchivedAt: fixedNow.AddDate(0, 0, -8), DeleteAfter: timePtr(fixedNow.AddDate(0, 0, -1)),
	}

	if err := p.sweep(context.Background(), "g1"); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := ops.Channel("arch1"); err != nil {
		t.Fatal("expected the rescued channel to NOT be deleted")
	}
	if _, ok := archives.records["arch1"]; ok {
		t.Fatal("expected the archive row to still be removed (stop tracking a rescued channel)")
	}
}

func TestSweepHandlesAlreadyDeletedChannel(t *testing.T) {
	_, archives, _, p := setupSweep(t)
	// No channel added — simulates a channel someone already deleted by hand.
	archives.records["gone1"] = ArchiveRecord{
		ChannelID: "gone1", GuildID: "g1", SourceChannelID: "old1",
		ArchivedAt: fixedNow.AddDate(0, 0, -8), DeleteAfter: timePtr(fixedNow.AddDate(0, 0, -1)),
	}

	if err := p.sweep(context.Background(), "g1"); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, ok := archives.records["gone1"]; ok {
		t.Fatal("expected the row for an already-deleted channel to be cleaned up")
	}
}

// TestSweepDeletesArchiveAfterRotationRetargetedConfig is a regression test
// for a bug introduced by rotation.rotate's retarget fix: sweepOne used to
// re-derive the archive's expected category by looking up
// settings.RotationChannel(guildID, rec.SourceChannelID) — but rotate now
// retargets that row's ChannelID onto the new live channel immediately after
// archiving, so the lookup by the OLD (archived) channel's ID stops finding
// anything on every single rotation, making every archive look
// "unconfigured" and therefore permanently rescued from deletion. Denormalizing
// ArchiveCategoryID onto the archive record itself (migration 0008) fixes
// this by making the check independent of the live, mutable settings row.
func TestSweepDeletesArchiveAfterRotationRetargetedConfig(t *testing.T) {
	ops, archives, _, p, rc := setupRotation(t, finiteRetentionRC())

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	due, err := archives.DueForDeletion(context.Background(), "g1", fixedNow.AddDate(0, 0, 8))
	if err != nil {
		t.Fatalf("DueForDeletion: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected exactly 1 due archive 8 days after a 7-day retention rotation, got %d", len(due))
	}

	p.now = func() time.Time { return fixedNow.AddDate(0, 0, 8) }
	if err := p.sweep(context.Background(), "g1"); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := ops.Channel(due[0].ChannelID); err == nil {
		t.Fatal("expected the due archived channel to actually be deleted, not rescued because its rotation config moved on")
	}
}

func TestSweepContinuesAfterPerRowFailure(t *testing.T) {
	ops, archives, _, p := setupSweep(t)
	ops.addChannel(&discordgo.Channel{ID: "arch1", GuildID: "g1", Name: "archive-1", ParentID: "archivecat"})
	ops.addChannel(&discordgo.Channel{ID: "arch2", GuildID: "g1", Name: "archive-2", ParentID: "archivecat"})
	archives.records["arch1"] = ArchiveRecord{ChannelID: "arch1", GuildID: "g1", SourceChannelID: "old1", ArchiveCategoryID: "archivecat", ArchivedAt: fixedNow, DeleteAfter: timePtr(fixedNow.AddDate(0, 0, -1))}
	archives.records["arch2"] = ArchiveRecord{ChannelID: "arch2", GuildID: "g1", SourceChannelID: "old1", ArchiveCategoryID: "archivecat", ArchivedAt: fixedNow, DeleteAfter: timePtr(fixedNow.AddDate(0, 0, -1))}

	ops.failOnCall["ChannelDelete"] = 1 // fail the first delete call, whichever row it lands on

	err := p.sweep(context.Background(), "g1")
	if err == nil {
		t.Fatal("expected sweep to return an error when one row fails")
	}

	// Exactly one of the two archived channels/rows should be gone (the one
	// whose delete succeeded); the failure on the other must not block it.
	_, err1 := ops.Channel("arch1")
	_, err2 := ops.Channel("arch2")
	deletedCount := 0
	if err1 != nil {
		deletedCount++
	}
	if err2 != nil {
		deletedCount++
	}
	if deletedCount != 1 {
		t.Fatalf("expected exactly 1 of 2 channels deleted despite the per-row failure, got %d", deletedCount)
	}
}

// TestSweepKeepsTrackingArchiveOnTransientFetchFailure is the counterpart to
// TestSweepHandlesAlreadyDeletedChannel: sweepOne used to treat *any* error
// fetching the archived channel as "already gone" and drop the tracking row.
// A rate limit or a 5xx would then leave the channel alive with nothing left
// to ever sweep it — a retention promise silently broken, which is the exact
// failure this feature exists to prevent. Only Discord's own "unknown
// channel" may untrack; everything else must fail and be retried.
func TestSweepKeepsTrackingArchiveOnTransientFetchFailure(t *testing.T) {
	ops, archives, _, p := setupSweep(t)
	ops.addChannel(&discordgo.Channel{ID: "arch1", GuildID: "g1", Name: "general-chat-archive-x", ParentID: "archivecat"})
	archives.records["arch1"] = ArchiveRecord{
		ChannelID: "arch1", GuildID: "g1", SourceChannelID: "old1", ArchiveCategoryID: "archivecat",
		ArchivedAt: fixedNow.AddDate(0, 0, -8), DeleteAfter: timePtr(fixedNow.AddDate(0, 0, -1)),
	}
	ops.failWith["Channel"] = transientErr()

	if err := p.sweep(context.Background(), "g1"); err == nil {
		t.Fatal("expected sweep to report the transient fetch failure")
	}
	if _, ok := archives.records["arch1"]; !ok {
		t.Fatal("archive row was dropped on a transient error — the channel would now never be swept")
	}

	// The next sweep, with Discord healthy again, finishes the job.
	delete(ops.failWith, "Channel")
	if err := p.sweep(context.Background(), "g1"); err != nil {
		t.Fatalf("retry sweep: %v", err)
	}
	if _, ok := archives.records["arch1"]; ok {
		t.Fatal("expected the archive to be swept on retry")
	}
	if _, err := ops.Channel("arch1"); err == nil {
		t.Fatal("expected the archived channel to be deleted on retry")
	}
}
