package rapsheet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

type fakeReviewer struct {
	out     string
	err     error
	calls   int
	lastSys string
	lastUsr string
}

func (f *fakeReviewer) Complete(_ context.Context, _, system, user string) (string, error) {
	f.calls++
	f.lastSys, f.lastUsr = system, user
	return f.out, f.err
}

type noModel struct{ msg string }

func (e noModel) Error() string     { return e.msg }
func (e noModel) Unavailable() bool { return true }

func TestSummaryAsksTheModelWithLedgerTextOnly(t *testing.T) {
	h := newHarness()
	rev := &fakeReviewer{out: "Two spam warnings a month apart and one automatic removal; recent, but not escalating."}
	h.p.reviewer = rev
	seedEntries(h, "u1", 2)
	_, _ = h.store.Insert(context.Background(), Entry{GuildID: testGuild, UserID: "u1", Kind: KindRemoval, Category: CategoryHateSpeech, Points: 50,
		ActorID: "system", Reason: "message removed automatically: a slur", Source: SourceAIMod, Ref: "9", CreatedAt: testNow.Add(-48 * time.Hour)})

	s, rt := stubSession()
	h.p.handleSummary(context.Background(), s, withResolved(interaction("summary", userOpt("user", "u1")), &discordgo.User{ID: "u1", Username: "dana"}))
	if rev.calls != 1 {
		t.Fatalf("model calls = %d", rev.calls)
	}
	if strings.Contains(rev.lastUsr, "u1") || strings.Contains(rev.lastUsr, modID) || strings.Contains(rev.lastUsr, "<@") {
		t.Errorf("the prompt carried an id or a mention: %s", rev.lastUsr)
	}
	for _, want := range []string{"by a moderator", "by the bot (AI moderation)", "hate speech", "50 points", "Server ladder", "Current score: 68"} {
		if !strings.Contains(rev.lastUsr, want) {
			t.Errorf("prompt should contain %q: %s", want, rev.lastUsr)
		}
	}
	if !strings.Contains(rt.said(), "not escalating") || !strings.Contains(rt.said(), "Summary: dana") || !strings.Contains(rt.said(), "Written by") {
		t.Errorf("got %s", rt.said())
	}
}

func TestSummaryDegradesToTheSheetWithoutAModel(t *testing.T) {
	h := newHarness()
	seedEntries(h, "u1", 1)
	s, rt := stubSession()
	h.p.handleSummary(context.Background(), s, interaction("summary", userOpt("user", "u1")))
	if !strings.Contains(rt.said(), "Summary unavailable") || !strings.Contains(rt.said(), "No model is wired") || !strings.Contains(rt.said(), "#1 warned") {
		t.Errorf("got %s", rt.said())
	}

	h.p.reviewer = &fakeReviewer{err: noModel{"aimod: no gateway key configured for this guild"}}
	s2, rt2 := stubSession()
	h.p.handleSummary(context.Background(), s2, interaction("summary", userOpt("user", "u1")))
	if !strings.Contains(rt2.said(), "no model key is configured") || strings.Contains(rt2.said(), "aimod:") || !strings.Contains(rt2.said(), "#1 warned") {
		t.Errorf("got %s", rt2.said())
	}

	h.p.reviewer = &fakeReviewer{err: errors.New("gateway exploded")}
	s3, rt3 := stubSession()
	h.p.handleSummary(context.Background(), s3, interaction("summary", userOpt("user", "u1")))
	if !strings.Contains(rt3.said(), "did not answer") {
		t.Errorf("a real failure should say so: %s", rt3.said())
	}

	s4, rt4 := stubSession()
	h.p.handleSummary(context.Background(), s4, interaction("summary", userOpt("user", "clean")))
	if !strings.Contains(rt4.said(), "nothing to summarise") {
		t.Errorf("got %s", rt4.said())
	}
}

func TestWeeklyReviewRunsOnlyWhereItCanAndPostsToTheModChannel(t *testing.T) {
	h := newHarness()
	h.p.SyncGuild(context.Background(), testGuild)
	if h.sched.has(reviewKey(testGuild)) {
		t.Fatal("registered with no model, no mod channel")
	}
	rev := &fakeReviewer{out: "Spam was warned at 10 points twice and jailed once for the same thing (#1, #2, #4). Otherwise consistent."}
	h.p.reviewer = rev
	h.p.SyncGuild(context.Background(), testGuild)
	if h.sched.has(reviewKey(testGuild)) {
		t.Fatal("registered with a model but no mod channel")
	}
	withModChannel(h, ModeSuggest)
	h.p.SyncGuild(context.Background(), testGuild)
	if !h.sched.has(reviewKey(testGuild)) {
		t.Fatal("not registered with a model and a mod channel")
	}

	// Nothing on record: nothing posted, no model call.
	if err := h.sched.RunNow(context.Background(), reviewKey(testGuild)); err != nil || rev.calls != 0 || len(h.ops.sentTo("mods")) != 0 {
		t.Errorf("empty month: err %v calls %d posts %d", err, rev.calls, len(h.ops.sentTo("mods")))
	}
	seedEntries(h, "u1", 3)
	seedEntries(h, "u2", 2)
	if err := h.sched.RunNow(context.Background(), reviewKey(testGuild)); err != nil {
		t.Fatalf("review: %v", err)
	}
	posts := h.ops.sentTo("mods")
	if rev.calls != 1 || len(posts) != 1 || !strings.Contains(posts[0].Embeds[0].Description, "Otherwise consistent") || !strings.Contains(posts[0].Embeds[0].Description, "5 entries") {
		t.Errorf("calls %d posts %+v", rev.calls, posts)
	}
	if strings.Contains(rev.lastUsr, "<@") || strings.Contains(rev.lastUsr, "u1") {
		t.Errorf("review prompt carried ids: %s", rev.lastUsr)
	}

	// Unavailable is a quiet skip; a failure is returned for the Scheduler.
	rev.err = noModel{"budget spent"}
	if err := h.sched.RunNow(context.Background(), reviewKey(testGuild)); err != nil {
		t.Errorf("unavailable model should not fail the job: %v", err)
	}
	rev.err = errors.New("gateway exploded")
	if err := h.sched.RunNow(context.Background(), reviewKey(testGuild)); err == nil {
		t.Error("a failed model call should be returned so the Scheduler backs off")
	}

	// Turning the ladder off unregisters it.
	cfg, _ := h.store.Config(context.Background(), testGuild)
	cfg.EscalationMode = ModeOff
	_ = h.store.SetConfig(context.Background(), cfg)
	h.p.SyncGuild(context.Background(), testGuild)
	if h.sched.has(reviewKey(testGuild)) {
		t.Error("still registered with the ladder off")
	}
}

func TestAltNoticeCarriesASecondOpinionWhenThereIsAModel(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeSuggest)
	rev := &fakeReviewer{out: "Strong: identical avatar and a join minutes after the ban. Worth checking whether they greet the same people."}
	h.p.reviewer = rev
	base := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	banned := snowflakeAt(base)
	h.ops.addMember(banned)
	h.ops.users[banned] = &discordgo.User{ID: banned, Username: "dana_k", Avatar: "abc123"}
	s, _ := stubSession()
	h.p.handleBan(context.Background(), s, withResolved(banCmd(banned, strOpt("duration", "7d")), h.ops.users[banned]))

	h.p.HandleMemberJoin(context.Background(), testGuild, &discordgo.Member{
		User: &discordgo.User{ID: snowflakeAt(base.Add(48 * time.Hour)), Username: "danak2", Avatar: "abc123"}, JoinedAt: testNow.Add(2 * time.Minute)})
	posts := h.ops.sentTo("mods")
	if len(posts) != 1 || !strings.Contains(posts[0].Embeds[0].Description, "A model's read") || !strings.Contains(posts[0].Embeds[0].Description, "Strong:") {
		t.Fatalf("posts = %+v", posts)
	}
	if strings.Contains(rev.lastUsr, "<@") {
		t.Errorf("opinion prompt carried a mention: %s", rev.lastUsr)
	}
	hints, _ := h.store.Hints(context.Background(), testGuild, banned)
	if len(hints) != 1 || !strings.HasPrefix(hints[0].Opinion, "Strong") {
		t.Errorf("hints = %+v", hints)
	}
	// A failing model costs the opinion and nothing else.
	rev.err = errors.New("exploded")
	h.p.HandleMemberJoin(context.Background(), testGuild, &discordgo.Member{
		User: &discordgo.User{ID: snowflakeAt(base.Add(72 * time.Hour)), Username: "danak3", Avatar: "abc123"}, JoinedAt: testNow.Add(3 * time.Minute)})
	if posts := h.ops.sentTo("mods"); len(posts) != 2 || strings.Contains(posts[1].Embeds[0].Description, "A model's read") {
		t.Errorf("second notice = %+v", posts)
	}
}

func TestStatusNamesTheModelState(t *testing.T) {
	h := newHarness()
	s, rt := stubSession()
	h.p.handleStatus(context.Background(), s, interaction("status"))
	if !strings.Contains(rt.said(), "none in this build") {
		t.Errorf("got %s", rt.said())
	}
	h.p.reviewer = &fakeReviewer{}
	withModChannel(h, ModeSuggest)
	h.p.SyncGuild(context.Background(), testGuild)
	s2, rt2 := stubSession()
	h.p.handleStatus(context.Background(), s2, interaction("status"))
	if !strings.Contains(rt2.said(), "Mondays 09:00 UTC") {
		t.Errorf("got %s", rt2.said())
	}
}
