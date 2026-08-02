package rotation

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/settings"
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
	// A mod moved the archived channel out of archivecat; this should be
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
	// No channel added: simulates a channel someone already deleted by hand.
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
// settings.RotationChannel(guildID, rec.SourceChannelID), but rotate now
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

	due := archives.dueForDeletion("g1", fixedNow.AddDate(0, 0, 8))
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
// to ever sweep it: a retention promise silently broken, which is the exact
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
		t.Fatal("archive row was dropped on a transient error; the channel would now never be swept")
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

// --- retention changes must apply to archives that already exist ---
//
// delete_after used to be computed once, at archive time, and never revisited,
// so /rotation configure edit only ever affected *future* archives. These
// pin down the fix: the sweep re-derives each archive's deadline from its
// rotation slot's current retention setting on every pass.

// archivedUnder registers rc, then records an archive produced by it at
// archivedAt with the deadline that rc's retention implied *at that moment*,
// exactly what rotate() writes. Tests then change rc's retention and assert
// the sweep honors the new value, not the frozen one.
func archivedUnder(t *testing.T, fs *fakeSettings, archives *fakeArchiveStore, rc settings.RotationChannel, archivedAt time.Time) settings.RotationChannel {
	t.Helper()
	if err := fs.UpsertRotationChannel(context.Background(), rc); err != nil {
		t.Fatalf("UpsertRotationChannel: %v", err)
	}
	stored, ok := fs.RotationChannel(rc.GuildID, rc.ChannelID)
	if !ok {
		t.Fatal("rotation channel missing right after upsert")
	}
	var deleteAfter *time.Time
	if stored.RetentionHours != nil {
		deleteAfter = timePtr(archivedAt.Add(time.Duration(*stored.RetentionHours) * time.Hour))
	}
	rotationID := stored.ID
	archives.records["arch1"] = ArchiveRecord{
		ChannelID: "arch1", GuildID: "g1", SourceChannelID: stored.ChannelID,
		ArchiveCategoryID: "archivecat", RotationID: &rotationID,
		ArchivedAt: archivedAt, DeleteAfter: deleteAfter,
	}
	return stored
}

// setRetention rewrites rc's retention the way /rotation configure edit does,
// preserving its stable ID.
func setRetention(t *testing.T, fs *fakeSettings, rc settings.RotationChannel, hours *int) {
	t.Helper()
	rc.RetentionHours = hours
	if err := fs.UpsertRotationChannel(context.Background(), rc); err != nil {
		t.Fatalf("UpsertRotationChannel: %v", err)
	}
}

// sweepAt runs a sweep with the clock pinned to at.
func sweepAt(t *testing.T, p *Plugin, at time.Time) {
	t.Helper()
	p.now = func() time.Time { return at }
	if err := p.sweep(context.Background(), "g1"); err != nil {
		t.Fatalf("sweep: %v", err)
	}
}

// TestSweepHonorsExtendedRetentionOnExistingArchives is the headline
// regression. An admin archives under a 24h window, decides that's too
// aggressive and widens it to 90 days, and the bot confirms the change, but
// the sweep still deleted the channel 24 hours in, because the deadline was
// frozen in the row. Channel deletion is permanent; there is no undo.
func TestSweepHonorsExtendedRetentionOnExistingArchives(t *testing.T) {
	ops, archives, _, p := setupSweep(t)
	fs := p.settings.(*fakeSettings)
	ops.addChannel(&discordgo.Channel{ID: "arch1", GuildID: "g1", Name: "general-chat-archive-x", ParentID: "archivecat"})

	rc := finiteRetentionRC()
	rc.RetentionHours = intPtr(24)
	stored := archivedUnder(t, fs, archives, rc, fixedNow)

	setRetention(t, fs, stored, intPtr(90*24))

	sweepAt(t, p, fixedNow.Add(25*time.Hour))
	if _, err := ops.Channel("arch1"); err != nil {
		t.Fatal("archive was deleted 25h in despite retention having been widened to 90 days")
	}
	if _, ok := archives.records["arch1"]; !ok {
		t.Fatal("archive row was dropped even though the channel is still within its retention window")
	}

	// ...and it is still deleted once the *new* window genuinely elapses.
	sweepAt(t, p, fixedNow.Add(91*24*time.Hour))
	if _, err := ops.Channel("arch1"); err == nil {
		t.Fatal("expected deletion once the widened retention window finally passed")
	}
}

// TestSweepNeverDeletesAfterRetentionSetToForever covers the strongest form
// of the same promise: "keep these forever" must protect archives that
// already exist, not just ones created afterwards.
func TestSweepNeverDeletesAfterRetentionSetToForever(t *testing.T) {
	ops, archives, _, p := setupSweep(t)
	fs := p.settings.(*fakeSettings)
	ops.addChannel(&discordgo.Channel{ID: "arch1", GuildID: "g1", Name: "general-chat-archive-x", ParentID: "archivecat"})

	rc := finiteRetentionRC()
	rc.RetentionHours = intPtr(24)
	stored := archivedUnder(t, fs, archives, rc, fixedNow)

	setRetention(t, fs, stored, nil) // /rotation configure edit retention_forever:true

	sweepAt(t, p, fixedNow.AddDate(10, 0, 0))
	if _, err := ops.Channel("arch1"); err != nil {
		t.Fatal("archive was deleted despite retention having been set to keep-forever")
	}
	if _, ok := archives.records["arch1"]; !ok {
		t.Fatal("expected the archive to stay tracked under keep-forever")
	}
}

// TestSweepHonorsShortenedRetentionOnExistingArchives is the other direction,
// and it matters just as much: this plugin's whole purpose is not retaining
// content longer than the guild promised (spec.MD §6). Tightening the window
// has to apply to what's already sitting in the archive category.
func TestSweepHonorsShortenedRetentionOnExistingArchives(t *testing.T) {
	ops, archives, _, p := setupSweep(t)
	fs := p.settings.(*fakeSettings)
	ops.addChannel(&discordgo.Channel{ID: "arch1", GuildID: "g1", Name: "general-chat-archive-x", ParentID: "archivecat"})

	rc := finiteRetentionRC()
	rc.RetentionHours = intPtr(30 * 24)
	stored := archivedUnder(t, fs, archives, rc, fixedNow)

	setRetention(t, fs, stored, intPtr(1))

	sweepAt(t, p, fixedNow.Add(2*time.Hour))
	if _, err := ops.Channel("arch1"); err == nil {
		t.Fatal("archive outlived the newly-tightened 1h retention window")
	}
}

// TestArchiveDeadlineFallbacks covers the two cases the live setting can't
// answer, where the deadline recorded at archive time remains the only
// promise on record.
func TestArchiveDeadlineFallbacks(t *testing.T) {
	stored := timePtr(fixedNow.Add(48 * time.Hour))
	rotationID := int64(7)

	t.Run("pre-migration row with no rotation link", func(t *testing.T) {
		rec := ArchiveRecord{ArchivedAt: fixedNow, DeleteAfter: stored}
		got := archiveDeadline(rec, func(int64) (settings.RotationChannel, bool) {
			t.Fatal("must not consult settings for a row with no RotationID")
			return settings.RotationChannel{}, false
		})
		if got == nil || !got.Equal(*stored) {
			t.Fatalf("archiveDeadline = %v, want the stored deadline %v", got, *stored)
		}
	})

	t.Run("rotation slot since removed", func(t *testing.T) {
		// /rotation configure remove promises existing archives are untouched,
		// so the retention they were created under still stands.
		rec := ArchiveRecord{RotationID: &rotationID, ArchivedAt: fixedNow, DeleteAfter: stored}
		got := archiveDeadline(rec, func(int64) (settings.RotationChannel, bool) {
			return settings.RotationChannel{}, false
		})
		if got == nil || !got.Equal(*stored) {
			t.Fatalf("archiveDeadline = %v, want the stored deadline %v", got, *stored)
		}
	})

	t.Run("keep-forever row stays nil", func(t *testing.T) {
		rec := ArchiveRecord{RotationID: &rotationID, ArchivedAt: fixedNow}
		got := archiveDeadline(rec, func(int64) (settings.RotationChannel, bool) {
			return settings.RotationChannel{RetentionHours: nil}, true
		})
		if got != nil {
			t.Fatalf("archiveDeadline = %v, want nil (never delete)", got)
		}
	})
}
