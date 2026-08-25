package aimod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/discordguard"
)

// The two errors the enforcement path has to tell apart from a genuine
// failure, built the way the real callers see them: wrapped, so errors.Is
// and core.IsUnknownResource are what does the telling rather than a string
// comparison that would drift.
func discordguardDryRun() error {
	return fmt.Errorf("message.delete: %w", discordguard.ErrDryRun)
}

func unknownMessageError() error {
	return &discordgo.RESTError{
		Response: &http.Response{StatusCode: http.StatusNotFound},
		Message:  &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownMessage, Message: "Unknown Message"},
	}
}

func confirmed(reason string) deepVerdict {
	return deepVerdict{Violation: true, Bucket: BucketThreats, Confidence: 0.95, Reason: reason}
}

// The ordering argument, and the reason enforce() is shaped the way it is:
// deleting first and then failing to record leaves the message gone with no
// copy of what it said, nothing for /aimod undo to restore, and nothing to
// show a member who asks what happened.
func TestNothingIsDeletedIfTheIncidentCannotBeRecorded(t *testing.T) {
	store := newFakeStore()
	store.incidentErr = errors.New("database is down")
	ops := newFakeOps()
	p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})

	p.enforce(context.Background(), enforcingConfig(),
		candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "text"},
		BucketThreats, ActionRemove, confirmed("r"))

	if deleted, _ := ops.snapshot(); len(deleted) != 0 {
		t.Errorf("deleted %v with no incident row behind it", deleted)
	}
}

// A guild in flag mode has asked to watch the filter work before letting it
// act, and that overrides every per-bucket action underneath it.
func TestFlagModeTouchesNothing(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	audit := &fakeAudit{}
	p := testPlugin(t, store, &fakeClassifier{}, ops, audit)

	cfg := enforcingConfig()
	cfg.Mode = ModeFlag
	p.enforce(context.Background(), cfg,
		candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "text"},
		BucketThreats, ActionRemove, confirmed("r"))

	deleted, posted := ops.snapshot()
	if len(deleted) != 0 || len(posted) != 0 {
		t.Errorf("flag mode touched Discord: deleted=%v posted=%d", deleted, len(posted))
	}
	if !contains(audit.actions(), "aimod.flagged") {
		t.Errorf("audit actions = %v, want aimod.flagged", audit.actions())
	}
	if len(store.recorded()) != 1 {
		t.Error("flag mode recorded no incident, so there is nothing to review")
	}
}

// The rewrite path: delete, then repost the cleaned text through a webhook
// wearing the author's name, with the marker attached.
func TestRewriteRepostsUnderTheAuthorWithAMarker(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	ops.members["u1"] = &discordgo.Member{Nick: "Someguy", User: &discordgo.User{ID: "u1", Username: "someguy"}}
	p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})

	v := confirmed("one line had a phone number in it")
	v.Rewrite = "i think [removed] is wrong about this"
	p.enforce(context.Background(), enforcingConfig(),
		candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "i think 555-0100 is wrong about this"},
		BucketDoxxing, ActionRewrite, v)

	deleted, posted := ops.snapshot()
	if len(deleted) != 1 {
		t.Fatalf("deleted %v, want the original removed", deleted)
	}
	if len(posted) != 1 {
		t.Fatalf("posted %d webhook messages, want 1", len(posted))
	}
	if posted[0].Username != "Someguy" {
		t.Errorf("Username = %q, want the author's nickname", posted[0].Username)
	}
	if !strings.Contains(posted[0].Content, v.Rewrite) {
		t.Errorf("the cleaned text is missing from the repost: %q", posted[0].Content)
	}
	// Not optional and not configurable: the repost wears somebody's name
	// and is not what they wrote, and everyone reading is entitled to know.
	if !strings.Contains(posted[0].Content, "edited by Merlin") {
		t.Errorf("the repost carries no edit marker: %q", posted[0].Content)
	}
	if strings.Contains(posted[0].Content, "555-0100") {
		t.Error("the removed text survived into the repost")
	}
}

// The deep pass is told to return an empty string when nothing publishable
// remains. That is a normal outcome, not a failure, and it degrades to a
// removal rather than trying to post an empty message.
func TestRewriteWithNothingLeftBecomesARemoval(t *testing.T) {
	ops := newFakeOps()
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, ops, &fakeAudit{})

	v := confirmed("the whole message was the violation")
	v.Rewrite = "   "
	p.enforce(context.Background(), enforcingConfig(),
		candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "x"},
		BucketThreats, ActionRewrite, v)

	deleted, posted := ops.snapshot()
	if len(deleted) != 1 {
		t.Errorf("deleted %v, want the message removed", deleted)
	}
	if len(posted) != 0 {
		t.Errorf("posted %d webhook messages for an empty rewrite", len(posted))
	}
}

// A member whose message vanishes with no explanation is how a moderation
// bot earns a reputation for being arbitrary, and this one acts with no
// human in the loop, so the DM is the only thing standing in for one.
func TestAuthorIsAlwaysToldWhy(t *testing.T) {
	ops := newFakeOps()
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, ops, &fakeAudit{})

	p.enforce(context.Background(), enforcingConfig(),
		candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "the original words"},
		BucketThreats, ActionRemove, confirmed("a specific person, a specific act"))

	if len(ops.dms) != 1 {
		t.Fatalf("sent %d DMs, want 1", len(ops.dms))
	}
	embed := ops.dms[0].Embeds[0]
	var fields []string
	for _, f := range embed.Fields {
		fields = append(fields, f.Name+": "+f.Value)
	}
	joined := strings.Join(fields, " | ")
	if !strings.Contains(joined, "a specific person") {
		t.Errorf("the DM does not say why: %q", joined)
	}
	// Their own words back to them, so a false positive costs them nothing
	// they wrote.
	if !strings.Contains(joined, "the original words") {
		t.Errorf("the DM does not carry the member's original text: %q", joined)
	}
}

// A guild that keeps no evidence stores none, which is what costs it
// /aimod undo. The author's DM still carries their own words back to them;
// see notifyAuthor for why those are different things.
func TestZeroEvidenceStoresNoText(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})

	cfg := enforcingConfig()
	cfg.EvidenceHours = 0
	p.enforce(context.Background(), cfg,
		candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "the original words"},
		BucketThreats, ActionRemove, confirmed("r"))

	inc := store.recorded()
	if len(inc) != 1 {
		t.Fatalf("recorded %d incidents, want 1", len(inc))
	}
	if inc[0].Content != "" {
		t.Errorf("stored %q as evidence in a guild that keeps none", inc[0].Content)
	}
	if len(ops.dms) != 1 {
		t.Error("the author was not told what happened, which is not what evidence retention governs")
	}
}

// Dry-run and pause are the guild deliberately stopping the bot acting, so
// the record of what would have happened is the whole output and it is not
// a failure. Same reasoning discordguard.Skipped exists for.
func TestDryRunRecordsWhatItWouldHaveDone(t *testing.T) {
	ops := newFakeOps()
	ops.deleteErr = discordguardDryRun()
	audit := &fakeAudit{}
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, ops, audit)

	p.enforce(context.Background(), enforcingConfig(),
		candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "x"},
		BucketThreats, ActionRemove, confirmed("r"))

	if !contains(audit.actions(), "aimod.dryrun") {
		t.Errorf("audit actions = %v, want aimod.dryrun", audit.actions())
	}
	for _, a := range audit.actions() {
		if a == "aimod.remove" {
			t.Error("a dry-run guild was recorded as having actually removed something")
		}
	}
}

// A message somebody else already deleted is not an error and must not be
// logged as one, but nor may it be confused with a genuine failure.
func TestAlreadyDeletedIsNotAFailure(t *testing.T) {
	ops := newFakeOps()
	ops.deleteErr = unknownMessageError()
	store := newFakeStore()
	p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})

	p.enforce(context.Background(), enforcingConfig(),
		candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "x"},
		BucketThreats, ActionRemove, confirmed("r"))

	// The incident row still stands: the verdict happened even though there
	// was nothing left to act on.
	if len(store.recorded()) != 1 {
		t.Error("no incident recorded for a message that was already gone")
	}
	if len(ops.dms) != 0 {
		t.Error("DMed a member about a message this bot did not actually remove")
	}
}

// An audit failure must never fail the operation that triggered it: the
// action already succeeded and the incident row is already durable. The
// policy every other call site in this codebase uses.
func TestAuditFailureDoesNotBlockEnforcement(t *testing.T) {
	ops := newFakeOps()
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, ops, &fakeAudit{err: errors.New("no audit channel")})

	p.enforce(context.Background(), enforcingConfig(),
		candidate{MessageID: "m1", ChannelID: "c1", AuthorID: "u1", Content: "x"},
		BucketThreats, ActionRemove, confirmed("r"))

	if deleted, _ := ops.snapshot(); len(deleted) != 1 {
		t.Errorf("deleted %v: a failed audit post blocked the removal", deleted)
	}
}

// Undo is the false-positive rescue hatch, and the whole reason evidence is
// stored at all.
func TestUndoRepostsTheOriginal(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	ops.members["u1"] = &discordgo.Member{User: &discordgo.User{ID: "u1", Username: "someguy"}}
	p := testPlugin(t, store, &fakeClassifier{}, ops, &fakeAudit{})

	id, err := store.RecordIncident(context.Background(), Incident{
		GuildID: "g1", ChannelID: "c1", MessageID: "m1", AuthorID: "u1",
		Bucket: BucketThreats, Action: ActionRemove, Content: "the original words",
	})
	if err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}
	inc, err := store.IncidentByMessage(context.Background(), "g1", "m1")
	if err != nil {
		t.Fatalf("IncidentByMessage: %v", err)
	}

	if err := p.undo(context.Background(), "g1", inc); err != nil {
		t.Fatalf("undo: %v", err)
	}
	_, posted := ops.snapshot()
	if len(posted) != 1 || posted[0].Content != "the original words" {
		t.Errorf("posted %+v, want the original text back verbatim", posted)
	}
	// Marked reversed, which is also what stops it counting toward the next
	// sanction: a false positive must not lengthen every future sentence.
	for _, rec := range store.recorded() {
		if rec.ID == id && !rec.Undone {
			t.Error("the incident was not marked undone")
		}
	}
	n, err := store.CountSanctions(context.Background(), "g1", "u1", testNow.Add(-repeatWindow))
	if err != nil {
		t.Fatalf("CountSanctions: %v", err)
	}
	if n != 0 {
		t.Errorf("a reversed incident still counts as %d prior sanction(s)", n)
	}
}

// Past the evidence window there is nothing to restore, and saying so beats
// reposting an empty message.
func TestUndoWithNoEvidenceRefuses(t *testing.T) {
	ops := newFakeOps()
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, ops, &fakeAudit{})

	err := p.undo(context.Background(), "g1", Incident{GuildID: "g1", ChannelID: "c1", AuthorID: "u1"})
	if !errors.Is(err, ErrNothingToUndo) {
		t.Errorf("undo without evidence = %v, want ErrNothingToUndo", err)
	}
	if _, posted := ops.snapshot(); len(posted) != 0 {
		t.Error("posted something despite having nothing to restore")
	}
}

// Discord caps a channel at 15 webhooks. One per rewrite would exhaust that
// in an afternoon and then start failing with an error nobody would connect
// to moderation.
func TestWebhookIsCreatedOncePerChannel(t *testing.T) {
	ops := newFakeOps()
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, ops, &fakeAudit{})

	for range 5 {
		if _, err := p.resolveWebhook(context.Background(), "g1", "c1"); err != nil {
			t.Fatalf("resolveWebhook: %v", err)
		}
	}
	if ops.webhooks != 1 {
		t.Errorf("created %d webhooks for one channel, want 1", ops.webhooks)
	}

	// A deleted channel drops the cache, so the next rewrite re-resolves
	// rather than failing forever against a webhook that is gone.
	p.HandleChannelDeleted("c1")
	if _, err := p.resolveWebhook(context.Background(), "g1", "c1"); err != nil {
		t.Fatalf("resolveWebhook after delete: %v", err)
	}
	if ops.webhooks != 2 {
		t.Errorf("created %d webhooks after the cache was dropped, want 2", ops.webhooks)
	}
}

func TestDisplayNamePrefersNickThenGlobalName(t *testing.T) {
	tests := []struct {
		name   string
		member *discordgo.Member
		want   string
	}{
		{"nick wins", &discordgo.Member{Nick: "Nick", User: &discordgo.User{GlobalName: "Global", Username: "user"}}, "Nick"},
		{"then global name", &discordgo.Member{User: &discordgo.User{GlobalName: "Global", Username: "user"}}, "Global"},
		{"then username", &discordgo.Member{User: &discordgo.User{Username: "user"}}, "user"},
		{"nil member", nil, "member"},
		{"no user", &discordgo.Member{}, "member"},
	}
	for _, tc := range tests {
		if got := displayName(tc.member); got != tc.want {
			t.Errorf("%s: displayName = %q, want %q", tc.name, got, tc.want)
		}
	}
}
