package rotation

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/settings"
)

func intPtr(n int) *int { return &n }

func finiteRetentionRC() settings.RotationChannel {
	return settings.RotationChannel{
		GuildID: "g1", ChannelID: "old1", IntervalMinutes: 24 * 60,
		ArchiveCategoryID: "archivecat", ArchiveVisibility: "mod_only",
		RetentionHours: intPtr(7 * 24),
		StickyEnabled:  true, StickyMessages: []string{"hello!"},
	}
}

func whitelistVisibilityRC() settings.RotationChannel {
	return settings.RotationChannel{
		GuildID: "g1", ChannelID: "old1", IntervalMinutes: 24 * 60,
		ArchiveCategoryID: "archivecat", ArchiveVisibility: "whitelist",
		ArchiveWhitelistRoleIDs: []string{"vip-role"}, ArchiveWhitelistUserIDs: []string{"vip-user"},
		RetentionHours: intPtr(7 * 24),
	}
}

func foreverRetentionRC() settings.RotationChannel {
	return settings.RotationChannel{
		GuildID: "g1", ChannelID: "old1", IntervalMinutes: 24 * 60,
		ArchiveCategoryID: "archivecat", ArchiveVisibility: "mod_only",
	}
}

func hourRetentionRC() settings.RotationChannel {
	return settings.RotationChannel{
		GuildID: "g1", ChannelID: "old1", IntervalMinutes: 24 * 60,
		ArchiveCategoryID: "archivecat", ArchiveVisibility: "mod_only",
		RetentionHours: intPtr(1),
	}
}

var fixedNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func setupRotation(t *testing.T, rc settings.RotationChannel) (*fakeOps, *fakeArchiveStore, *fakeAudit, *Plugin, settings.RotationChannel) {
	t.Helper()
	fs := newFakeSettings()
	fs.modRoles["g1"] = []string{"modrole1"}
	_ = fs.UpsertRotationChannel(context.Background(), rc)

	ops := newFakeOps()
	ops.addChannel(&discordgo.Channel{
		ID:       "old1",
		GuildID:  "g1",
		Name:     "general-chat",
		Topic:    "chat here",
		Position: 3,
		ParentID: "cat0",
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{ID: "g1", Type: discordgo.PermissionOverwriteTypeRole, Allow: discordgo.PermissionViewChannel},
		},
	})

	archives := newFakeArchiveStore()
	audit := &fakeAudit{}
	p := newTestPlugin(ops, archives, audit, fs, fixedNow)
	return ops, archives, audit, p, rc
}

func TestRotateFullCycle(t *testing.T) {
	ops, archives, audit, p, rc := setupRotation(t, finiteRetentionRC())

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	oldCh, err := ops.Channel("old1")
	if err != nil {
		t.Fatalf("Channel(old1): %v", err)
	}
	if oldCh.ParentID != "archivecat" {
		t.Fatalf("expected old channel moved to archivecat, got %s", oldCh.ParentID)
	}
	if !strings.Contains(oldCh.Name, "general-chat-archive-") {
		t.Fatalf("expected archive name, got %s", oldCh.Name)
	}
	foundDeny := false
	for _, ow := range oldCh.PermissionOverwrites {
		if ow.ID == "g1" && ow.Deny&discordgo.PermissionViewChannel != 0 {
			foundDeny = true
		}
	}
	if !foundDeny {
		t.Fatal("expected @everyone denied on the archived channel")
	}
	foundBotAccess := false
	for _, ow := range oldCh.PermissionOverwrites {
		if ow.ID == "bot-user-id" && ow.Allow&discordgo.PermissionViewChannel != 0 {
			foundBotAccess = true
		}
	}
	if !foundBotAccess {
		t.Fatal("expected the bot itself to retain VIEW_CHANNEL on the archived channel it just denied @everyone on — " +
			"regression test for the bug where the bot locked itself out of channels it created/archived")
	}

	channels, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	newCh := findOtherChannelByName(channels, "general-chat", "old1")
	if newCh == nil {
		t.Fatal("expected a new channel named general-chat")
	}
	everyoneAllowed := false
	for _, ow := range newCh.PermissionOverwrites {
		if ow.ID == "g1" && ow.Allow&discordgo.PermissionViewChannel != 0 {
			everyoneAllowed = true
		}
	}
	if !everyoneAllowed {
		t.Fatal("expected the revealed new channel to restore @everyone's original view access")
	}

	msgs, err := ops.ChannelMessages(newCh.ID, 10, "", "", "")
	if err != nil {
		t.Fatalf("ChannelMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (sticky + notice) in the new channel, got %d", len(msgs))
	}

	due := archives.dueForDeletion("g1", fixedNow.AddDate(0, 0, 8))
	if len(due) != 1 {
		t.Fatalf("expected 1 due archive 8 days after a 7-day retention rotation, got %d", len(due))
	}

	if len(audit.records) != 1 || audit.records[0].action != "channel.rotated" {
		t.Fatalf("expected 1 channel.rotated audit record, got %+v", audit.records)
	}
}

// TestRotateRevealRestoresAccessWhenOldChannelHadNoExplicitOverwrites is a
// regression test for the bug the user hit in production: a fully public
// channel like general chat typically has zero explicit permission
// overwrites (everyone sees it purely via @everyone's guild-level role
// permissions, no channel-specific entry needed). revealNewChannel used to
// pass that empty/nil slice straight through to ChannelEditComplex, which
// discordgo marshals with `omitempty` — Discord then left the staging
// channel's "deny @everyone, allow only the bot" overwrites in place
// forever on what was supposed to become the fully public replacement, so
// nobody but the bot could ever see the rotated channel again.
func TestRotateRevealRestoresAccessWhenOldChannelHadNoExplicitOverwrites(t *testing.T) {
	fs := newFakeSettings()
	rc := finiteRetentionRC()
	_ = fs.UpsertRotationChannel(context.Background(), rc)

	ops := newFakeOps()
	ops.addChannel(&discordgo.Channel{
		ID:      "old1",
		GuildID: "g1",
		Name:    "general-chat",
		// Deliberately no PermissionOverwrites — the common case for a
		// fully public channel relying only on @everyone's base role perms.
	})

	p := newTestPlugin(ops, newFakeArchiveStore(), &fakeAudit{}, fs, fixedNow)

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	channels, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	newCh := findOtherChannelByName(channels, "general-chat", "old1")
	if newCh == nil {
		t.Fatal("expected a new channel named general-chat")
	}
	for _, ow := range newCh.PermissionOverwrites {
		if ow.ID == "g1" && ow.Deny&discordgo.PermissionViewChannel != 0 {
			t.Fatalf("expected @everyone NOT denied on the revealed channel, got overwrites %+v", newCh.PermissionOverwrites)
		}
	}
}

// TestRotateRetargetsRotationConfigToNewChannel is a regression test for the
// bug where the rotation config kept tracking the OLD (now-archived) channel
// ID forever after a successful rotation: the next scheduled fire would
// refetch by that stale ID and try to "rotate" the archive instead of the
// live replacement.
func TestRotateRetargetsRotationConfigToNewChannel(t *testing.T) {
	ops, _, _, p, rc := setupRotation(t, finiteRetentionRC())
	fs := p.settings.(*fakeSettings)

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if _, ok := fs.RotationChannel("g1", "old1"); ok {
		t.Fatal("expected the rotation config no longer tracked under the archived channel's ID")
	}

	channels, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	newCh := findOtherChannelByName(channels, "general-chat", "old1")
	if newCh == nil {
		t.Fatal("expected a new channel named general-chat")
	}

	updated, ok := fs.RotationChannel("g1", newCh.ID)
	if !ok {
		t.Fatal("expected the rotation config retargeted to the new channel's ID")
	}
	if updated.IntervalMinutes != rc.IntervalMinutes || updated.ArchiveCategoryID != rc.ArchiveCategoryID {
		t.Fatalf("expected the retargeted config to preserve its other settings, got %+v", updated)
	}
}

// TestRotateSucceedsDespiteAuditFailure guards against a real bug fixed
// alongside this test: rotation's own steps (rename, archive, new channel,
// stickies, archive record) had already fully succeeded by the time audit
// posting ran, so an audit-embed failure (e.g. #bot-audit-log not yet
// configured, or deleted) must not make the Scheduler treat this job as
// failed — that would trigger pointless retries of an already-complete
// rotation and eventually a false failure alert, masking that rotation
// itself is fine. Every other audit call site in this codebase already
// logs-and-continues; rotate must too.
func TestRotateSucceedsDespiteAuditFailure(t *testing.T) {
	ops, _, audit, p, rc := setupRotation(t, finiteRetentionRC())
	audit.failErr = errors.New("audit channel deleted")

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate should succeed even when audit posting fails, got: %v", err)
	}

	channels, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	if findOtherChannelByName(channels, "general-chat", "old1") == nil {
		t.Fatal("expected the rotation to have completed (new channel present) despite the audit failure")
	}
}

func TestRotateWhitelistVisibilityGrantsExtraAccess(t *testing.T) {
	ops, _, _, p, rc := setupRotation(t, whitelistVisibilityRC())

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	oldCh, err := ops.Channel("old1")
	if err != nil {
		t.Fatalf("Channel(old1): %v", err)
	}

	var roleAllowed, userAllowed, modAllowed, everyoneDenied bool
	for _, ow := range oldCh.PermissionOverwrites {
		switch {
		case ow.ID == "g1" && ow.Deny&discordgo.PermissionViewChannel != 0:
			everyoneDenied = true
		case ow.ID == "modrole1" && ow.Allow&discordgo.PermissionViewChannel != 0:
			modAllowed = true
		case ow.ID == "vip-role" && ow.Type == discordgo.PermissionOverwriteTypeRole && ow.Allow&discordgo.PermissionViewChannel != 0:
			roleAllowed = true
		case ow.ID == "vip-user" && ow.Type == discordgo.PermissionOverwriteTypeMember && ow.Allow&discordgo.PermissionViewChannel != 0:
			userAllowed = true
		}
	}
	if !everyoneDenied {
		t.Error("expected @everyone denied on the whitelist archive")
	}
	if !modAllowed {
		t.Error("expected mod roles to always retain access under whitelist visibility")
	}
	if !roleAllowed {
		t.Error("expected the whitelisted role to be granted access")
	}
	if !userAllowed {
		t.Error("expected the whitelisted user to be granted access")
	}
}

func TestRotateForeverRetentionNeverDue(t *testing.T) {
	_, archives, _, p, rc := setupRotation(t, foreverRetentionRC())

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	due := archives.dueForDeletion("g1", fixedNow.AddDate(10, 0, 0))
	if len(due) != 0 {
		t.Fatalf("expected a forever-retention archive to never be due, got %d due rows", len(due))
	}
}

// TestRotateRetentionHourBoundaryIsPrecise is a regression test for
// retention moving from day-granular to hour-granular storage (migration
// 0010): a 1-hour retention window must become due right at the 1-hour
// mark, not rounded up to a day boundary the way the old
// time.AddDate(0,0,days) arithmetic would have.
func TestRotateRetentionHourBoundaryIsPrecise(t *testing.T) {
	_, archives, _, p, rc := setupRotation(t, hourRetentionRC())

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	notYetDue := archives.dueForDeletion("g1", fixedNow.Add(59*time.Minute))
	if len(notYetDue) != 0 {
		t.Fatalf("expected a 1-hour retention archive to NOT be due at 59 minutes, got %d due rows", len(notYetDue))
	}

	due := archives.dueForDeletion("g1", fixedNow.Add(61*time.Minute))
	if len(due) != 1 {
		t.Fatalf("expected a 1-hour retention archive to be due at 61 minutes, got %d due rows", len(due))
	}
}

func TestRotateFailureLeavesNoLaterStepApplied(t *testing.T) {
	tests := []struct {
		name       string
		failMethod string
		failOnCall int
	}{
		{"fetch old channel fails", "Channel", 1},
		{"list channels fails", "GuildChannels", 1},
		{"create staging channel fails", "GuildChannelCreateComplex", 1},
		{"post sticky message fails", "ChannelMessageSend", 1},
		{"reveal new channel fails", "ChannelEditComplex", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops, archives, audit, p, rc := setupRotation(t, finiteRetentionRC())
			ops.failOnCall[tt.failMethod] = tt.failOnCall

			err := p.rotate(context.Background(), "g1", rc)
			if err == nil {
				t.Fatal("expected rotate to return an error")
			}

			oldCh, chErr := ops.Channel("old1")
			if chErr != nil {
				t.Fatalf("Channel(old1): %v", chErr)
			}
			if oldCh.Name != "general-chat" || oldCh.ParentID != "cat0" {
				t.Fatalf("old channel must be untouched on this failure, got name=%s parent=%s", oldCh.Name, oldCh.ParentID)
			}
			if len(archives.records) != 0 {
				t.Fatalf("expected no archive record on this failure, got %d", len(archives.records))
			}
			if len(audit.records) != 0 {
				t.Fatalf("expected no audit record on this failure, got %d", len(audit.records))
			}
		})
	}
}

// TestRotateConcurrentFetchFailurePrioritizesChannelError guards the
// parallelized preflight fetch (Channel + GuildChannels now run
// concurrently via goroutines, see execute.go's rotate): when both fail
// simultaneously, the channel-fetch error must still win, matching the
// priority the original sequential code had (it never even reached
// GuildChannels if Channel failed first).
func TestRotateConcurrentFetchFailurePrioritizesChannelError(t *testing.T) {
	ops, _, _, p, rc := setupRotation(t, finiteRetentionRC())
	ops.failOnCall["Channel"] = 1
	ops.failOnCall["GuildChannels"] = 1

	err := p.rotate(context.Background(), "g1", rc)
	if err == nil {
		t.Fatal("expected rotate to return an error")
	}
	if !strings.Contains(err.Error(), "fetch channel") {
		t.Fatalf("expected the channel-fetch error to take priority when both preflight reads fail, got: %v", err)
	}
}

// TestRotateSkipsThreadCaptureOnAlreadyRevealedRetry is a regression test
// for the thread-capture reordering in execute.go's rotate: capturing
// active threads only matters (and only happens) on a fresh run, not when
// a retry finds the new channel already revealed — re-fetching them again
// would just be a wasted ThreadsActive call, since the first attempt
// already captured and logged them.
func TestRotateSkipsThreadCaptureOnAlreadyRevealedRetry(t *testing.T) {
	ops, _, _, p, rc := setupRotation(t, finiteRetentionRC())
	// Fail archiving old (2nd ChannelEditComplex call) on the first attempt
	// only — reveal succeeds, so the retry finds alreadyRevealed == true.
	ops.failOnCall["ChannelEditComplex"] = 2

	if err := p.rotate(context.Background(), "g1", rc); err == nil {
		t.Fatal("expected the first attempt to fail while archiving old")
	}
	if got := ops.callCounts["ThreadsActive"]; got != 1 {
		t.Fatalf("expected ThreadsActive called exactly once on the fresh-run first attempt, got %d", got)
	}

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("retry rotate: %v", err)
	}
	if got := ops.callCounts["ThreadsActive"]; got != 1 {
		t.Fatalf("expected ThreadsActive still called exactly once after the already-revealed retry (skipped, not re-fetched), got %d", got)
	}
}

func TestRotateFailureArchivingOldLeavesDualVisibleWindow(t *testing.T) {
	ops, archives, audit, p, rc := setupRotation(t, finiteRetentionRC())
	// The 2nd ChannelEditComplex call is the "archive old" step (the 1st is
	// "reveal new"). This is the accepted trade-off from the Milestone 3
	// design: a brief window where both channels are visible under the
	// same name, rather than any zero-live-channel outage.
	ops.failOnCall["ChannelEditComplex"] = 2

	err := p.rotate(context.Background(), "g1", rc)
	if err == nil {
		t.Fatal("expected rotate to return an error")
	}

	oldCh, chErr := ops.Channel("old1")
	if chErr != nil {
		t.Fatalf("Channel(old1): %v", chErr)
	}
	if oldCh.Name != "general-chat" {
		t.Fatalf("expected old channel NOT yet archived (still named general-chat), got %s", oldCh.Name)
	}

	channels, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	newCh := findOtherChannelByName(channels, "general-chat", "old1")
	if newCh == nil {
		t.Fatal("expected the new channel to already be revealed as general-chat (the accepted dual-visible window)")
	}

	if len(archives.records) != 0 {
		t.Fatalf("expected no archive record yet, got %d", len(archives.records))
	}
	if len(audit.records) != 0 {
		t.Fatalf("expected no audit record yet, got %d", len(audit.records))
	}
}

func TestRotateRetryAfterArchiveFailureIsIdempotent(t *testing.T) {
	ops, archives, audit, p, rc := setupRotation(t, finiteRetentionRC())
	ops.failOnCall["ChannelEditComplex"] = 2 // fail archiving old on the first attempt only

	if err := p.rotate(context.Background(), "g1", rc); err == nil {
		t.Fatal("expected first rotate attempt to fail")
	}

	// Retry: should NOT create a second staging/replacement channel, should
	// NOT re-post sticky messages, and should finish archiving old1.
	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("retry rotate: %v", err)
	}

	channels, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	var generalChatCount int
	for _, c := range channels {
		if c.Name == "general-chat" {
			generalChatCount++
		}
	}
	if generalChatCount != 1 {
		t.Fatalf("expected exactly 1 channel named general-chat after retry, got %d", generalChatCount)
	}

	oldCh, err := ops.Channel("old1")
	if err != nil {
		t.Fatalf("Channel(old1): %v", err)
	}
	if !strings.Contains(oldCh.Name, "general-chat-archive-") {
		t.Fatalf("expected old channel archived after retry, got name %s", oldCh.Name)
	}

	newCh := findOtherChannelByName(channels, "general-chat", "old1")
	if newCh == nil {
		t.Fatal("expected the revealed replacement channel to still exist")
	}
	msgs, err := ops.ChannelMessages(newCh.ID, 10, "", "", "")
	if err != nil {
		t.Fatalf("ChannelMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected exactly 2 messages (no duplicate sticky/notice from the retry), got %d", len(msgs))
	}

	if len(archives.records) != 1 {
		t.Fatalf("expected exactly 1 archive record after retry, got %d", len(archives.records))
	}
	if len(audit.records) != 1 {
		t.Fatalf("expected exactly 1 audit record after retry, got %d", len(audit.records))
	}
}

func TestRotateChannelCapPreflightBlocksRotation(t *testing.T) {
	ops, _, _, p, rc := setupRotation(t, finiteRetentionRC())
	for i := range maxChannelsPerGuild {
		ops.addChannel(&discordgo.Channel{ID: intToID(i), GuildID: "g1", Name: intToID(i)})
	}

	if err := p.rotate(context.Background(), "g1", rc); err == nil {
		t.Fatal("expected rotation to be blocked by the channel-cap preflight")
	}
}

func intToID(i int) string {
	return "filler" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
}

// denyEveryone/botOverwriteAllow's own regression tests moved to
// internal/core/channels_test.go — the logic now lives in
// core.DenyEveryoneExceptBot, shared with adminconfig's setup channels.

// TestReconcileJobKeyStableAcrossRetarget is a regression test for a bug
// that shipped in the previous fix: reconcile used to build the Scheduler
// job key from rc.ChannelID, which execute.go's rotate() now retargets onto
// the new live channel after every successful rotation. The Scheduler
// persists a job's last-run/interval-due state under its exact key string —
// so a key that changes every rotation reset that state every single time,
// and a job with no run history is immediately due again on the Scheduler's
// very next ~30s tick. In production this meant a fresh rotation (and
// archive) roughly every 30 seconds forever, instead of once per
// interval_hours. Keying off rc.ID (stable across retargets, see migration
// 0009) instead of rc.ChannelID fixes this: reconcile must register the
// job exactly once and never touch it again just because the channel it
// points at changed.
func TestReconcileJobKeyStableAcrossRetarget(t *testing.T) {
	fs := newFakeSettings()
	fs.modRoles["g1"] = []string{"modrole1"}
	_ = fs.UpsertRotationChannel(context.Background(), settings.RotationChannel{
		GuildID: "g1", ChannelID: "old1", IntervalMinutes: 24 * 60,
		ArchiveCategoryID: "archivecat", ArchiveVisibility: "mod_only",
	})

	sched := newFakeScheduler()
	p := &Plugin{
		settings:        fs,
		sched:           sched,
		log:             testLogger(),
		bus:             core.NewEventBus(testLogger()),
		now:             func() time.Time { return fixedNow },
		sweepRegistered: make(map[string]bool),
		registeredJobs:  make(map[string]time.Duration),
	}

	p.reconcile(context.Background(), "g1")

	rc, ok := fs.RotationChannel("g1", "old1")
	if !ok {
		t.Fatal("expected the seeded rotation config to be findable by its initial channel ID")
	}
	jobKey := scheduler.JobKey("g1", "rotation:"+strconv.FormatInt(rc.ID, 10))
	if sched.registerCalls[jobKey] != 1 {
		t.Fatalf("expected the rotation job registered exactly once, got %d", sched.registerCalls[jobKey])
	}

	// Mirrors exactly what execute.go's rotate does after a successful
	// rotation (retarget), followed by the reconcile that
	// RetargetRotationChannel's publishChanged triggers in real usage.
	if err := fs.RetargetRotationChannel(context.Background(), "g1", "old1", "new1"); err != nil {
		t.Fatalf("RetargetRotationChannel: %v", err)
	}
	p.reconcile(context.Background(), "g1")

	if sched.registerCalls[jobKey] != 1 {
		t.Fatalf("expected the SAME job key to still be registered exactly once after retargeting, got %d registrations — "+
			"a second registration under a fresh key would reset the Scheduler's persisted last-run state, "+
			"causing rotation to fire again on the very next tick instead of waiting a full interval", sched.registerCalls[jobKey])
	}
	if sched.unregisterCalls[jobKey] != 0 {
		t.Fatalf("expected the job to never be unregistered across a retarget, got %d unregister calls", sched.unregisterCalls[jobKey])
	}
}

// TestDeferFirstRotationSeedsNewChannelJob verifies handleAdd's fix for a
// real reported bug: adding a brand-new rotation channel used to rotate it
// on the Scheduler's very next tick, since a freshly-registered job with no
// run history is otherwise treated as immediately due. deferFirstRotation
// (called from handleAdd right after the channel is saved) must seed that
// job's persisted state so its first real rotation waits a full interval.
func TestDeferFirstRotationSeedsNewChannelJob(t *testing.T) {
	fs := newFakeSettings()
	fs.modRoles["g1"] = []string{"modrole1"}
	_ = fs.UpsertRotationChannel(context.Background(), settings.RotationChannel{
		GuildID: "g1", ChannelID: "new1", IntervalMinutes: 24 * 60,
		ArchiveCategoryID: "archivecat", ArchiveVisibility: "mod_only",
	})

	sched := newFakeScheduler()
	p := &Plugin{
		settings:        fs,
		sched:           sched,
		log:             testLogger(),
		bus:             core.NewEventBus(testLogger()),
		now:             func() time.Time { return fixedNow },
		sweepRegistered: make(map[string]bool),
		registeredJobs:  make(map[string]time.Duration),
	}

	// fs.UpsertRotationChannel doesn't publish core.EventConfigChanged (the
	// fake mirrors the store's write path, not its event side effect), so
	// reconcile is called explicitly here to mirror what the real
	// settings.Store's publishChanged would trigger synchronously in
	// production before handleAdd calls deferFirstRotation.
	p.reconcile(context.Background(), "g1")

	rc, ok := fs.RotationChannel("g1", "new1")
	if !ok {
		t.Fatal("expected the seeded rotation config to be findable")
	}
	jobKey := scheduler.JobKey("g1", "rotation:"+strconv.FormatInt(rc.ID, 10))

	p.deferFirstRotation(context.Background(), "g1", "new1")

	seededAt, ok := sched.seedCalls[jobKey]
	if !ok {
		t.Fatalf("expected deferFirstRotation to seed job %q, but it never called Seed", jobKey)
	}
	if !seededAt.Equal(fixedNow) {
		t.Fatalf("expected job seeded at %v, got %v", fixedNow, seededAt)
	}
}

// TestReconcileAloneNeverSeeds guards the other half of the same fix: a
// restart calls SyncGuild (-> reconcile) for every already-configured
// channel, including ones with real run history. reconcile must never seed
// on its own — only handleAdd's explicit deferFirstRotation call may do
// that — or a restart would incorrectly reset an overdue rotation's clock.
func TestReconcileAloneNeverSeeds(t *testing.T) {
	fs := newFakeSettings()
	fs.modRoles["g1"] = []string{"modrole1"}
	_ = fs.UpsertRotationChannel(context.Background(), settings.RotationChannel{
		GuildID: "g1", ChannelID: "existing1", IntervalMinutes: 24 * 60,
		ArchiveCategoryID: "archivecat", ArchiveVisibility: "mod_only",
	})

	sched := newFakeScheduler()
	p := &Plugin{
		settings:        fs,
		sched:           sched,
		log:             testLogger(),
		bus:             core.NewEventBus(testLogger()),
		now:             func() time.Time { return fixedNow },
		sweepRegistered: make(map[string]bool),
		registeredJobs:  make(map[string]time.Duration),
	}

	p.SyncGuild(context.Background(), "g1")

	if len(sched.seedCalls) != 0 {
		t.Fatalf("expected reconcile/SyncGuild to never seed on its own, got seed calls: %v", sched.seedCalls)
	}
}

func TestArchiveOverwritesGrantsBotViewAccess(t *testing.T) {
	out := archiveOverwrites("g1", "bot-user-id", nil, finiteRetentionRC())

	found := false
	for _, ow := range out {
		if ow.ID == "bot-user-id" && ow.Type == discordgo.PermissionOverwriteTypeMember && ow.Allow&discordgo.PermissionViewChannel != 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the bot to retain VIEW_CHANNEL on the archived channel (needed later by sweep.go)")
	}
}

// TestRotateRetryAfterArchiveRecordFailureDoesNotRotateTwice covers the
// window that used to be unrecoverable: the flip fully succeeded (replacement
// revealed, old channel renamed into the archive category) and only the
// bookkeeping afterwards failed. On the retry the "old" channel no longer
// carries its original name, so name-based matching found nothing and the
// rotation ran a second time — leaving the guild with two replacement
// channels, a doubly-archived original, and sticky messages posted twice.
// rotate now recognizes an already-archived channel by its name and category
// and resumes at the bookkeeping step instead.
func TestRotateRetryAfterArchiveRecordFailureDoesNotRotateTwice(t *testing.T) {
	ops, archives, _, p, rc := setupRotation(t, finiteRetentionRC())
	archives.insertErr = errors.New("postgres unavailable")

	if err := p.rotate(context.Background(), "g1", rc); err == nil {
		t.Fatal("expected the first attempt to fail recording the archive")
	}

	archives.insertErr = nil
	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("retry rotate: %v", err)
	}

	channels, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	var live, archived int
	for _, c := range channels {
		switch {
		case c.Name == "general-chat":
			live++
		case strings.Contains(c.Name, "general-chat-archive-"):
			archived++
		}
	}
	if live != 1 {
		t.Fatalf("expected exactly 1 live general-chat after the retry, got %d", live)
	}
	if archived != 1 {
		t.Fatalf("expected exactly 1 archived channel after the retry, got %d", archived)
	}
	if len(archives.records) != 1 {
		t.Fatalf("expected exactly 1 archive record, got %d", len(archives.records))
	}

	newCh := findOtherChannelByName(channels, "general-chat", "old1")
	msgs, err := ops.ChannelMessages(newCh.ID, 10, "", "", "")
	if err != nil {
		t.Fatalf("ChannelMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected the replacement to still hold exactly 2 messages (no duplicate sticky/notice), got %d", len(msgs))
	}
}

// TestRotateRetryAfterArchiveKeepsArchiveNameStable guards a subtler part of
// the same resume path: re-archiving on every retry would re-stamp the
// archive's name with a fresh timestamp, so a channel that failed bookkeeping
// repeatedly would drift its own archived-at label away from when it was
// actually archived.
func TestRotateRetryAfterArchiveKeepsArchiveNameStable(t *testing.T) {
	ops, archives, _, p, rc := setupRotation(t, finiteRetentionRC())
	archives.insertErr = errors.New("postgres unavailable")
	if err := p.rotate(context.Background(), "g1", rc); err == nil {
		t.Fatal("expected the first attempt to fail")
	}
	archivedAfterFirst, err := ops.Channel("old1")
	if err != nil {
		t.Fatalf("Channel(old1): %v", err)
	}

	archives.insertErr = nil
	p.now = func() time.Time { return fixedNow.Add(9 * time.Hour) }
	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("retry rotate: %v", err)
	}

	archivedAfterRetry, err := ops.Channel("old1")
	if err != nil {
		t.Fatalf("Channel(old1): %v", err)
	}
	if archivedAfterRetry.Name != archivedAfterFirst.Name {
		t.Fatalf("archive name changed on retry: %q -> %q", archivedAfterFirst.Name, archivedAfterRetry.Name)
	}
}

// TestRotateIgnoresSameNamedChannelInAnotherCategory is the duplicate-name
// guard. Discord allows two channels to share a name in different
// categories, and the replacement-detection used to match on name alone —
// so an unrelated #general elsewhere in the guild looked exactly like a
// replacement a previous attempt had already revealed. Rotation would then
// archive the live channel without creating anything, and retarget its own
// config onto a channel it doesn't own.
func TestRotateIgnoresSameNamedChannelInAnotherCategory(t *testing.T) {
	ops, _, _, p, rc := setupRotation(t, finiteRetentionRC())
	ops.addChannel(&discordgo.Channel{ID: "unrelated1", GuildID: "g1", Name: "general-chat", ParentID: "some-other-category"})

	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	unrelated, err := ops.Channel("unrelated1")
	if err != nil {
		t.Fatalf("Channel(unrelated1): %v", err)
	}
	if unrelated.ParentID != "some-other-category" || unrelated.Name != "general-chat" {
		t.Fatalf("the unrelated same-named channel was modified: %+v", unrelated)
	}
	if rcNow, ok := p.settings.RotationChannel("g1", "unrelated1"); ok {
		t.Fatalf("rotation retargeted itself onto the unrelated channel: %+v", rcNow)
	}

	channels, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	replacement := findChannel(channels, func(c *discordgo.Channel) bool {
		return c.Name == "general-chat" && c.ID != "unrelated1" && c.ID != "old1"
	})
	if replacement == nil {
		t.Fatal("expected a real replacement channel to have been created")
	}
	if _, ok := p.settings.RotationChannel("g1", replacement.ID); !ok {
		t.Fatal("expected the rotation config to be retargeted onto the real replacement")
	}
}

func TestArchivedChannelOrigin(t *testing.T) {
	for _, tc := range []struct {
		name         string
		channel      *discordgo.Channel
		categoryID   string
		wantOriginal string
		wantArchived bool
	}{
		{
			name:         "archive name in the archive category",
			channel:      &discordgo.Channel{Name: "general-chat-archive-2026-01-01-0000", ParentID: "archivecat"},
			categoryID:   "archivecat",
			wantOriginal: "general-chat",
			wantArchived: true,
		},
		{
			name:         "archive name outside the archive category is someone else's channel",
			channel:      &discordgo.Channel{Name: "general-chat-archive-2026-01-01-0000", ParentID: "cat0"},
			categoryID:   "archivecat",
			wantOriginal: "general-chat-archive-2026-01-01-0000",
		},
		{
			name:         "ordinary channel that happens to sit in the archive category",
			channel:      &discordgo.Channel{Name: "notes", ParentID: "archivecat"},
			categoryID:   "archivecat",
			wantOriginal: "notes",
		},
		{
			name:         "a name already containing -archive- keeps its longest prefix",
			channel:      &discordgo.Channel{Name: "war-archive-chat-archive-2026-01-01-0000", ParentID: "archivecat"},
			categoryID:   "archivecat",
			wantOriginal: "war-archive-chat",
			wantArchived: true,
		},
		{
			name:         "unconfigured archive category never matches",
			channel:      &discordgo.Channel{Name: "general-chat-archive-2026-01-01-0000"},
			wantOriginal: "general-chat-archive-2026-01-01-0000",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original, archived := archivedChannelOrigin(tc.channel, tc.categoryID)
			if original != tc.wantOriginal || archived != tc.wantArchived {
				t.Errorf("archivedChannelOrigin() = (%q, %v), want (%q, %v)", original, archived, tc.wantOriginal, tc.wantArchived)
			}
		})
	}
}

// TestArchiveNameRoundTrips ties archiveChannelName and archivedChannelOrigin
// together: whatever the archiver writes, the resume path must be able to
// read back. They'd otherwise be free to drift apart silently, and the only
// symptom would be duplicate channels after a mid-rotation failure.
func TestArchiveNameRoundTrips(t *testing.T) {
	for _, name := range []string{"general-chat", "off-topic", "a", "war-archive-chat", "channel-with-2026-in-it"} {
		archived := &discordgo.Channel{Name: archiveChannelName(name, fixedNow), ParentID: "archivecat"}
		got, ok := archivedChannelOrigin(archived, "archivecat")
		if !ok || got != name {
			t.Errorf("round trip of %q via %q = (%q, %v)", name, archived.Name, got, ok)
		}
	}
}

// TestRotateResumeRebuildsAReplacementDeletedAfterTheFlip is the worst
// corner of the resume path: the flip completed, the bookkeeping failed, and
// then someone deleted the replacement channel before the retry. The retry
// has to rebuild a live channel — and must not build it inside the archive
// category, which is where the "old" channel now lives and therefore what
// its parent would otherwise be copied from.
func TestRotateResumeRebuildsAReplacementDeletedAfterTheFlip(t *testing.T) {
	ops, archives, _, p, rc := setupRotation(t, finiteRetentionRC())
	archives.insertErr = errors.New("postgres unavailable")
	if err := p.rotate(context.Background(), "g1", rc); err == nil {
		t.Fatal("expected the first attempt to fail")
	}

	channels, err := ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	revealed := findOtherChannelByName(channels, "general-chat", "old1")
	if revealed == nil {
		t.Fatal("expected a revealed replacement after the first attempt")
	}
	if _, err := ops.ChannelDelete(revealed.ID); err != nil {
		t.Fatalf("ChannelDelete: %v", err)
	}

	archives.insertErr = nil
	if err := p.rotate(context.Background(), "g1", rc); err != nil {
		t.Fatalf("retry rotate: %v", err)
	}

	channels, err = ops.GuildChannels("g1")
	if err != nil {
		t.Fatalf("GuildChannels: %v", err)
	}
	rebuilt := findOtherChannelByName(channels, "general-chat", "old1")
	if rebuilt == nil {
		t.Fatal("expected the retry to rebuild a live general-chat")
	}
	if rebuilt.ParentID == "archivecat" {
		t.Fatal("the rebuilt live channel was created inside the archive category")
	}
	if _, ok := p.settings.RotationChannel("g1", rebuilt.ID); !ok {
		t.Fatal("expected the rotation config to be retargeted onto the rebuilt channel")
	}

	oldCh, err := ops.Channel("old1")
	if err != nil {
		t.Fatalf("Channel(old1): %v", err)
	}
	if !strings.Contains(oldCh.Name, "general-chat-archive-") || oldCh.ParentID != "archivecat" {
		t.Fatalf("the already-archived channel should have been left alone, got %+v", oldCh)
	}
}

// reconcileTestPlugin builds a Plugin wired only with what reconcile needs.
func reconcileTestPlugin(fs *fakeSettings, sched *fakeScheduler, archives ArchiveStore) *Plugin {
	return &Plugin{
		settings:        fs,
		archives:        archives,
		sched:           sched,
		log:             testLogger(),
		bus:             core.NewEventBus(testLogger()),
		now:             func() time.Time { return fixedNow },
		sweepRegistered: make(map[string]bool),
		registeredJobs:  make(map[string]time.Duration),
	}
}

// TestSweepJobNotArmedForGuildsWithoutRotation: the sweep permanently deletes
// channels, and it used to be registered for every guild the bot could see
// the moment it saw it — whether or not that guild had ever configured
// rotation, and (since a job with no run history is immediately due) firing
// within one tick of startup. It was harmless only because the table it read
// happened to be empty. A deletion job should not exist where nobody asked
// for one.
func TestSweepJobNotArmedForGuildsWithoutRotation(t *testing.T) {
	sched := newFakeScheduler()
	p := reconcileTestPlugin(newFakeSettings(), sched, newFakeArchiveStore())

	p.reconcile(context.Background(), "g1")

	sweepKey := scheduler.JobKey("g1", "rotation-sweep")
	if sched.registered[sweepKey] {
		t.Fatal("a guild that never configured rotation must not get an archive-deletion job")
	}
}

// TestSweepJobArmedOnceRotationIsConfigured is the counterweight: the moment
// a guild actually configures a rotating channel, the sweep has to exist, or
// its retention window would never be enforced at all.
func TestSweepJobArmedOnceRotationIsConfigured(t *testing.T) {
	fs := newFakeSettings()
	sched := newFakeScheduler()
	p := reconcileTestPlugin(fs, sched, newFakeArchiveStore())

	p.reconcile(context.Background(), "g1")
	if err := fs.UpsertRotationChannel(context.Background(), finiteRetentionRC()); err != nil {
		t.Fatalf("UpsertRotationChannel: %v", err)
	}
	p.reconcile(context.Background(), "g1")

	sweepKey := scheduler.JobKey("g1", "rotation-sweep")
	if !sched.registered[sweepKey] {
		t.Fatal("expected the sweep job once the guild configured a rotating channel")
	}
	if sched.registerCalls[sweepKey] != 1 {
		t.Fatalf("expected the sweep registered exactly once, got %d", sched.registerCalls[sweepKey])
	}
}

// TestSweepJobSurvivesRemovalWhileArchivesArePending guards the direction
// that would silently break a retention promise: /rotation configure remove
// says existing archives are left untouched, so those archives still have a
// deletion date owed. Dropping the sweep because the rotation slot is gone
// would leave them alive forever with nothing to ever collect them.
func TestSweepJobSurvivesRemovalWhileArchivesArePending(t *testing.T) {
	fs := newFakeSettings()
	sched := newFakeScheduler()
	archives := newFakeArchiveStore()
	p := reconcileTestPlugin(fs, sched, archives)

	if err := fs.UpsertRotationChannel(context.Background(), finiteRetentionRC()); err != nil {
		t.Fatalf("UpsertRotationChannel: %v", err)
	}
	p.reconcile(context.Background(), "g1")

	archives.records["arch1"] = ArchiveRecord{
		ChannelID: "arch1", GuildID: "g1", ArchiveCategoryID: "archivecat",
		ArchivedAt: fixedNow, DeleteAfter: timePtr(fixedNow.AddDate(0, 0, 7)),
	}
	if err := fs.RemoveRotationChannel(context.Background(), "g1", "old1"); err != nil {
		t.Fatalf("RemoveRotationChannel: %v", err)
	}
	p.reconcile(context.Background(), "g1")

	sweepKey := scheduler.JobKey("g1", "rotation-sweep")
	if !sched.registered[sweepKey] {
		t.Fatal("sweep job was dropped while archives were still awaiting their retention deadline")
	}

	// Once nothing is pending, it goes away again.
	delete(archives.records, "arch1")
	p.reconcile(context.Background(), "g1")
	if sched.registered[sweepKey] {
		t.Fatal("expected the sweep job to be unregistered once no rotation and no archives remain")
	}
}

// TestSweepJobRegistrationUnchangedOnArchiveLookupFailure: a transient DB
// error must not decide either way — arming a deletion job we can't justify,
// or dropping one that's still owed work, are both worse than leaving the
// current state alone until the next reconcile.
func TestSweepJobRegistrationUnchangedOnArchiveLookupFailure(t *testing.T) {
	fs := newFakeSettings()
	sched := newFakeScheduler()
	archives := newFakeArchiveStore()
	p := reconcileTestPlugin(fs, sched, archives)

	if err := fs.UpsertRotationChannel(context.Background(), finiteRetentionRC()); err != nil {
		t.Fatalf("UpsertRotationChannel: %v", err)
	}
	p.reconcile(context.Background(), "g1")
	sweepKey := scheduler.JobKey("g1", "rotation-sweep")

	if err := fs.RemoveRotationChannel(context.Background(), "g1", "old1"); err != nil {
		t.Fatalf("RemoveRotationChannel: %v", err)
	}
	archives.listErr = transientErr()
	p.reconcile(context.Background(), "g1")

	if !sched.registered[sweepKey] {
		t.Fatal("a failed archive lookup must not unregister an existing sweep job")
	}
}
