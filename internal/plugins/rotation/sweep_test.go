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
		ChannelID: "arch1", GuildID: "g1", SourceChannelID: "old1",
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
		ChannelID: "arch1", GuildID: "g1", SourceChannelID: "old1",
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
		ChannelID: "arch1", GuildID: "g1", SourceChannelID: "old1",
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

func TestSweepContinuesAfterPerRowFailure(t *testing.T) {
	ops, archives, _, p := setupSweep(t)
	ops.addChannel(&discordgo.Channel{ID: "arch1", GuildID: "g1", Name: "archive-1", ParentID: "archivecat"})
	ops.addChannel(&discordgo.Channel{ID: "arch2", GuildID: "g1", Name: "archive-2", ParentID: "archivecat"})
	archives.records["arch1"] = ArchiveRecord{ChannelID: "arch1", GuildID: "g1", SourceChannelID: "old1", ArchivedAt: fixedNow, DeleteAfter: timePtr(fixedNow.AddDate(0, 0, -1))}
	archives.records["arch2"] = ArchiveRecord{ChannelID: "arch2", GuildID: "g1", SourceChannelID: "old1", ArchivedAt: fixedNow, DeleteAfter: timePtr(fixedNow.AddDate(0, 0, -1))}

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
