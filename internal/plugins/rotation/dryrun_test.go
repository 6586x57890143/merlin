package rotation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// enableDryRun flips the plugin into rehearsal mode the same way a guild
// admin does with /config dryrun.
func enableDryRun(p *Plugin) {
	p.dryRun = func(string) bool { return true }
}

// A dry-run rotation must leave Discord completely untouched (no staging
// channel created, no channel renamed or moved) while still saying so in
// the audit trail. This is the pre-launch rehearsal the whole feature is
// meant to be verified with, so "it quietly rotated anyway" is the one
// outcome that would make the rehearsal worse than useless.
func TestRotateDryRunTouchesNothing(t *testing.T) {
	ops, archives, audit, p, rc := setupRotation(t, finiteRetentionRC())
	enableDryRun(p)

	before, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate under dry-run should succeed silently: %v", err)
	}

	after, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("dry-run changed the channel list: %d -> %d", len(before), len(after))
	}

	old, err := ops.Channel("old1")
	if err != nil {
		t.Fatalf("Channel(old1): %v", err)
	}
	if old.Name != "general-chat" {
		t.Errorf("dry-run renamed the live channel to %q", old.Name)
	}
	if old.ParentID != "cat0" {
		t.Errorf("dry-run moved the live channel to %q", old.ParentID)
	}

	if got := len(archives.records); got != 0 {
		t.Errorf("dry-run recorded %d archive(s), want 0", got)
	}

	// The point of a rehearsal is a record of what would have happened.
	found := false
	for _, r := range audit.records {
		if r.action == "rotation.dryrun" {
			found = true
		}
	}
	if !found {
		t.Error("dry-run rotation wrote no audit record; the rehearsal left no evidence")
	}
}

// The sweep is the irreversible one: a permanently deleted channel has no
// undo. A rehearsal must neither delete anything nor forget which archives
// it is still watching, or the real sweep afterwards would have lost track
// of the work it still owes.
func TestSweepDryRunDeletesNothingAndKeepsTracking(t *testing.T) {
	ops, archives, audit, p, _ := setupRotation(t, finiteRetentionRC())
	enableDryRun(p)

	// An archive that is comfortably past its retention window.
	seedDueArchive(t, ops, archives)
	beforeRows := len(archives.records)

	if err := p.sweep(context.Background(), "g1"); err != nil {
		t.Fatalf("sweep under dry-run should succeed silently: %v", err)
	}

	if got := len(archives.records); got != beforeRows {
		t.Errorf("dry-run sweep dropped tracking rows: %d -> %d", beforeRows, got)
	}
	if _, err := ops.Channel("archived1"); err != nil {
		t.Errorf("dry-run sweep deleted the archived channel: %v", err)
	}

	found := false
	for _, r := range audit.records {
		if r.action == "archive.dryrun" && strings.Contains(r.oldValue, "archived1") {
			found = true
		}
	}
	if !found {
		t.Error("dry-run sweep did not report which archives it would have deleted")
	}
}

// Regression guard for the failure mode that motivated the pause/skip
// distinction: a paused or rehearsing guild must not look like a broken job.
// If these returned errors, the Scheduler would count them toward
// maxConsecutiveFailures and start alerting #bird-status about a state the
// operator deliberately asked for.
func TestDryRunJobsReportSuccessToScheduler(t *testing.T) {
	_, _, _, p, rc := setupRotation(t, finiteRetentionRC())
	enableDryRun(p)

	if err := p.makeRotationJob("g1", rc.ID)(context.Background()); err != nil {
		t.Errorf("rotation job under dry-run returned %v, want nil", err)
	}
	if err := p.makeSweepJob("g1")(context.Background()); err != nil {
		t.Errorf("sweep job under dry-run returned %v, want nil", err)
	}
}

func seedDueArchive(t *testing.T, ops *fakeOps, archives *fakeArchiveStore) {
	t.Helper()
	ops.addChannel(&discordgo.Channel{
		ID:       "archived1",
		GuildID:  "g1",
		Name:     "general-chat-archive-2026-01-01-0000",
		ParentID: "archivecat",
	})
	deleteAfter := fixedNow.Add(-time.Hour)
	if err := archives.Insert(context.Background(), ArchiveRecord{
		GuildID:           "g1",
		ChannelID:         "archived1",
		SourceChannelID:   "old1",
		ArchiveCategoryID: "archivecat",
		ArchivedAt:        fixedNow.Add(-48 * time.Hour),
		DeleteAfter:       &deleteAfter,
	}); err != nil {
		t.Fatalf("seed archive: %v", err)
	}
}
